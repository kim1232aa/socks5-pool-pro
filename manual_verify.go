package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	maxManualNodeVerifyConcurrent = 16
	manualNodeVerifyMaxAttempts   = 3
	manualNodeVerifyExitTimeout   = 3 * time.Second
	manualNodeVerifyGeoTimeout    = 3 * time.Second
	// Worst case: 6s + 150ms + 4s + 150ms + 4s connectivity, followed by
	// 3s exit and 3s geo. The outer guard stays below the status server write
	// timeout while allowing the final successful attempt to finish metadata.
	manualNodeVerifyTotalTimeout = 22 * time.Second
)

type manualNodeVerifyCheckFunc func(context.Context, Proxy, string, time.Duration) (bool, time.Duration)
type manualNodeVerifyCredentialCheckFunc func(context.Context, Proxy, string, time.Duration) (Proxy, bool, time.Duration, error)

type manualNodeVerifyOperations struct {
	checkURL            manualNodeVerifyCheckFunc
	checkURLCredentials manualNodeVerifyCredentialCheckFunc
	probeExitIP         func(context.Context, Proxy, time.Duration) string
	lookupGeo           func(context.Context, string, time.Duration) (string, string, string)
}

func defaultManualNodeVerifyOperations() manualNodeVerifyOperations {
	return manualNodeVerifyOperations{
		checkURL: func(ctx context.Context, px Proxy, target string, timeout time.Duration) (bool, time.Duration) {
			started := time.Now()
			return checkURLContext(ctx, px, target, timeout), time.Since(started)
		},
		checkURLCredentials: func(ctx context.Context, px Proxy, target string, timeout time.Duration) (Proxy, bool, time.Duration, error) {
			started := time.Now()
			verified, ok, err := checkURLCredentialCandidatesContext(ctx, px, target, timeout)
			return verified, ok, time.Since(started), err
		},
		probeExitIP: probeExitIPContext,
		lookupGeo:   LookupGeoContext,
	}
}

// nodeOperationCooldownError reports a same-node cooldown with an exact retry delay.
type nodeOperationCooldownError struct {
	Operation string
	Remaining time.Duration
}

func (e *nodeOperationCooldownError) Error() string {
	return fmt.Sprintf("该节点%s冷却中，请在 %d 秒后重试", e.Operation, retryAfterSeconds(e.Remaining))
}

func retryAfterSeconds(remaining time.Duration) int {
	seconds := int((remaining + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

// beginManualNodeVerify also rejects duplicate clicks for the same node instead
// of starting competing probes that would distort its health-failure streak.
func (s *StatusServer) beginManualNodeVerify(key string) error {
	s.nodeVerifyMu.Lock()
	defer s.nodeVerifyMu.Unlock()
	now := time.Now()
	for candidateKey, until := range s.nodeVerifyCooldownUntil {
		if !until.After(now) {
			delete(s.nodeVerifyCooldownUntil, candidateKey)
		}
	}
	if _, running := s.nodeVerifyRunning[key]; running {
		return fmt.Errorf("该节点正在复检")
	}
	if until := s.nodeVerifyCooldownUntil[key]; until.After(now) {
		return &nodeOperationCooldownError{Operation: "复检", Remaining: until.Sub(now)}
	}
	select {
	case s.nodeVerifySlots <- struct{}{}:
		s.nodeVerifyRunning[key] = struct{}{}
		return nil
	default:
		return fmt.Errorf("复检并发已达上限，请稍后重试")
	}
}

func (s *StatusServer) endManualNodeVerify(key string) {
	s.nodeVerifyMu.Lock()
	if _, running := s.nodeVerifyRunning[key]; running {
		delete(s.nodeVerifyRunning, key)
		s.nodeVerifyCooldownUntil[key] = time.Now().Add(nodeManualVerifyCooldown)
		<-s.nodeVerifySlots
	}
	s.nodeVerifyMu.Unlock()
}

func runManualNodeVerifyChecks(ctx context.Context, operations manualNodeVerifyOperations, px Proxy, target string) (verified Proxy, reachable bool, attempts int, latencyMs int64, err error) {
	verified = px
	for attempts < manualNodeVerifyMaxAttempts {
		if err := ctx.Err(); err != nil {
			return verified, false, attempts, 0, err
		}
		attemptTimeout := manualNodeVerifyAttemptTimeout(attempts)
		attempts++
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		checked := verified
		var ok bool
		var latency time.Duration
		if operations.checkURLCredentials != nil {
			checked, ok, latency, _ = operations.checkURLCredentials(attemptCtx, verified, target, attemptTimeout)
		} else if operations.checkURL != nil {
			ok, latency = operations.checkURL(attemptCtx, verified, target, attemptTimeout)
		}
		cancel()
		if err := ctx.Err(); err != nil {
			return verified, false, attempts, 0, err
		}
		// A tunnel can prove a credential before the target itself returns an
		// unacceptable response. Carry that declaration into the next ordinary
		// retry so we do not start again from a credential already rejected.
		if checked.Username != verified.Username || checked.Password != verified.Password || !credentialsEqual(checked.CredentialAlternates, verified.CredentialAlternates) {
			verified = checked
		}
		if ok {
			if latency < 0 {
				latency = 0
			}
			return verified, true, attempts, latency.Milliseconds(), nil
		}
		if attempts < manualNodeVerifyMaxAttempts {
			backoff := manualNodeVerifyRetryBackoff(attempts)
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return verified, false, attempts, 0, ctx.Err()
			}
		}
	}
	return verified, false, attempts, 0, nil
}

func manualNodeVerifyAttemptTimeout(attemptIndex int) time.Duration {
	if attemptIndex <= 0 {
		return 6 * time.Second
	}
	return 4 * time.Second
}

func manualNodeVerifyRetryBackoff(completedAttempts int) time.Duration {
	return 150 * time.Millisecond
}

// manualNodeLabelMatch keeps the historical label_matched boolean compatible
// while making its unknown state explicit. A missing/legacy "Unknown" country
// on either side is not evidence of a mismatch, so compatibility consumers see
// true and newer consumers consult label_match_known before claiming a match.
func manualNodeLabelMatch(currentCountry, previousCountry string) (known, matched bool) {
	currentCountry = strings.TrimSpace(currentCountry)
	previousCountry = strings.TrimSpace(previousCountry)
	known = currentCountry != "" && previousCountry != "" &&
		!strings.EqualFold(currentCountry, "Unknown") && !strings.EqualFold(previousCountry, "Unknown")
	if !known {
		return false, true
	}
	return true, strings.EqualFold(currentCountry, previousCountry)
}

func writeManualNodeVerifyCanceled(w http.ResponseWriter, attempts int, err error) {
	status := http.StatusRequestTimeout
	message := "手动复检请求已取消"
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
		message = "手动复检超时"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error":    fmt.Sprintf("%s: %v", message, err),
		"attempts": attempts,
	})
}

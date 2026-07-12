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
	maxManualNodeVerifyConcurrent = 4
	manualNodeVerifyMaxAttempts   = 3
	manualNodeVerifyExitTimeout   = 5 * time.Second
	manualNodeVerifyGeoTimeout    = 5 * time.Second
	// Worst case: 10s + 200ms + 8s + 400ms + 8s connectivity, followed by
	// 5s exit and 5s geo. The 40-second outer guard stays comfortably below
	// StatusServer's 45s WriteTimeout while preserving the old 10-second first
	// attempt for genuinely slow nodes.
	manualNodeVerifyTotalTimeout = 40 * time.Second
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

// beginManualNodeVerify bounds the expensive 40-second verification path and
// rejects duplicate clicks for the same node instead of starting competing
// probes that would distort its health-failure streak.
func (s *StatusServer) beginManualNodeVerify(key string) error {
	s.nodeVerifyMu.Lock()
	defer s.nodeVerifyMu.Unlock()
	if _, running := s.nodeVerifyRunning[key]; running {
		return fmt.Errorf("该节点正在复检")
	}
	select {
	case s.nodeVerifySlots <- struct{}{}:
		s.nodeVerifyRunning[key] = struct{}{}
		return nil
	default:
		return fmt.Errorf("复检任务已满,请稍后重试")
	}
}

func (s *StatusServer) endManualNodeVerify(key string) {
	s.nodeVerifyMu.Lock()
	if _, running := s.nodeVerifyRunning[key]; running {
		delete(s.nodeVerifyRunning, key)
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
		return 10 * time.Second
	}
	return 8 * time.Second
}

func manualNodeVerifyRetryBackoff(completedAttempts int) time.Duration {
	if completedAttempts <= 1 {
		return 200 * time.Millisecond
	}
	return 400 * time.Millisecond
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

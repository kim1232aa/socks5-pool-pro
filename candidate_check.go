package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type candidateCheckKind uint8

const (
	candidateCheckNoResult candidateCheckKind = iota
	candidateCheckAlive
	candidateCheckUnreachable
	candidateCheckPolicyFiltered
)

type candidateCheckOptions struct {
	Timeout         time.Duration
	MaxConcurrent   int
	RequireIPChange bool
	TestURL         string
	BaselineIP      string
}

type candidateCheckOutcome struct {
	Key   string
	Proxy Proxy
	Kind  candidateCheckKind
	Error string
}

var (
	candidateCheckCredentialCandidates = checkURLCredentialCandidatesContext
	candidateCheckProbeExitIP          = probeExitIPContext
	candidateCheckLookupGeo            = LookupGeoContext
	candidateCheckProbeAnonymity       = probeAnonymityContext
)

func checkCandidateContext(parent context.Context, px Proxy, options candidateCheckOptions) candidateCheckOutcome {
	outcome := candidateCheckOutcome{Key: px.Key(), Proxy: px}
	if !isForwardingProtocol(px.Protocol) || options.Timeout <= 0 {
		return outcome
	}
	if parent == nil {
		parent = context.Background()
	}
	if parent.Err() != nil {
		return outcome
	}

	nodeContext, cancelNode := context.WithTimeout(parent, options.Timeout)
	defer cancelNode()
	started := time.Now()
	checked, reachable, err := candidateCheckCredentialCandidates(nodeContext, px, options.TestURL, options.Timeout)
	if !reachable {
		if parent.Err() != nil {
			return outcome
		}
		outcome.Kind = candidateCheckUnreachable
		if err == nil {
			err = fmt.Errorf("candidate failed current health check criterion")
		}
		outcome.Error = sanitizeCandidateFailureError(err.Error())
		return outcome
	}
	checked.LatencyMs = time.Since(started).Milliseconds()
	outcome.Proxy = checked

	checked.ExitIP = candidateCheckProbeExitIP(nodeContext, checked, options.Timeout)
	policy := evaluateIPChangePolicy(checked.ExitIP, options.BaselineIP, options.RequireIPChange)
	checked.IPChangeKnown = policy.IPChangeKnown
	checked.IPChanged = policy.IPChanged
	outcome.Proxy = checked
	if parent.Err() != nil {
		outcome.Kind = candidateCheckNoResult
		return outcome
	}
	if !policy.PolicyAllowed {
		outcome.Kind = candidateCheckPolicyFiltered
		outcome.Error = sanitizeCandidateFailureError(fmt.Sprintf("proxy exit IP %s matches baseline egress", policy.ExitIP))
		return outcome
	}

	normalizeProxyGeoFields(&checked)
	if lookupIP := proxyGeoLookupTarget(checked); lookupIP != "" {
		country, city, continent := candidateCheckLookupGeo(nodeContext, lookupIP, options.Timeout)
		if country != "" && country != "Unknown" {
			checked.Country, checked.City, checked.Continent = country, city, continent
		} else if checked.Country == "" {
			checked.Country = "Unknown"
		}
	}
	checked.Anonymity = candidateCheckProbeAnonymity(nodeContext, checked, options.Timeout)
	outcome.Proxy = checked
	if parent.Err() != nil {
		outcome.Kind = candidateCheckNoResult
		return outcome
	}
	outcome.Kind = candidateCheckAlive
	return outcome
}

func checkCandidateBatchContext(parent context.Context, proxies []Proxy, options candidateCheckOptions, completed func(candidateCheckOutcome)) map[string]candidateCheckOutcome {
	if parent == nil {
		parent = context.Background()
	}
	maxConcurrent := options.MaxConcurrent
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	if maxConcurrent > len(proxies) && len(proxies) > 0 {
		maxConcurrent = len(proxies)
	}

	outcomes := make(map[string]candidateCheckOutcome, len(proxies))
	var mu sync.Mutex
	jobs := make(chan Proxy)
	var workers sync.WaitGroup
	for range maxConcurrent {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for px := range jobs {
				outcome := checkCandidateContext(parent, px, options)
				mu.Lock()
				outcomes[px.Key()] = outcome
				if completed != nil {
					completed(outcome)
				}
				mu.Unlock()
			}
		}()
	}

sendLoop:
	for _, px := range proxies {
		select {
		case jobs <- px:
		case <-parent.Done():
			break sendLoop
		}
	}
	close(jobs)
	workers.Wait()
	return outcomes
}

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestCandidateSamplerPersistsRotationPastFailingUnseenNodes(t *testing.T) {
	dir := t.TempDir()
	candidates := []Proxy{
		{IP: "192.0.2.1", Port: "1080", Protocol: "socks5", SourceName: "alpha"},
		{IP: "192.0.2.2", Port: "1080", Protocol: "socks5", SourceName: "alpha"},
		{IP: "192.0.2.3", Port: "1080", Protocol: "socks5", SourceName: "alpha"},
	}

	var got []string
	for range candidates {
		s := newCandidateSampler(dir)
		selection := s.selectCandidates(candidates, nil, 1)
		if len(selection) != 1 {
			t.Fatalf("selection length = %d, want 1", len(selection))
		}
		got = append(got, selection[0].Key())
	}
	want := []string{
		"socks5://192.0.2.1:1080",
		"socks5://192.0.2.2:1080",
		"socks5://192.0.2.3:1080",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("persistent rotation = %v, want %v", got, want)
	}

	info, err := os.Stat(filepath.Join(dir, candidateSamplerStateFile))
	if err != nil {
		t.Fatalf("stat sampler state: %v", err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o600 {
		t.Errorf("sampler state permissions = %#o, want 0600", gotMode)
	}
}

func TestCandidateSamplerSkipsKnownCandidatesAndAdvancesCursor(t *testing.T) {
	dir := t.TempDir()
	candidates := []Proxy{
		{IP: "192.0.2.1", Port: "8080", Protocol: "http", SourceName: "alpha"},
		{IP: "192.0.2.2", Port: "8080", Protocol: "http", SourceName: "alpha"},
		{IP: "192.0.2.3", Port: "8080", Protocol: "http", SourceName: "alpha"},
	}
	known := map[string]bool{candidates[0].Key(): true}

	first := newCandidateSampler(dir).selectCandidates(candidates, known, 1)
	second := newCandidateSampler(dir).selectCandidates(candidates, known, 1)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("selections = %#v / %#v, want one each", first, second)
	}
	if first[0].Key() != candidates[1].Key() || second[0].Key() != candidates[2].Key() {
		t.Fatalf("known skip/rotation = %q, %q; want %q, %q", first[0].Key(), second[0].Key(), candidates[1].Key(), candidates[2].Key())
	}
}

func TestCandidateSamplerRoundRobinsSourceProtocolBuckets(t *testing.T) {
	dir := t.TempDir()
	candidates := []Proxy{
		{IP: "192.0.2.1", Port: "1080", Protocol: "socks5", SourceName: "alpha"},
		{IP: "192.0.2.2", Port: "8080", Protocol: "http", SourceName: "alpha"},
		{IP: "192.0.2.3", Port: "1080", Protocol: "socks5", SourceName: "beta"},
	}

	var buckets []string
	for range candidates {
		selected := newCandidateSampler(dir).selectCandidates(candidates, nil, 1)
		if len(selected) != 1 {
			t.Fatalf("selection length = %d, want 1", len(selected))
		}
		buckets = append(buckets, candidateBucketKey(selected[0]))
	}
	want := []string{
		"alpha\x00http",
		"alpha\x00socks5",
		"beta\x00socks5",
	}
	if !reflect.DeepEqual(buckets, want) {
		t.Fatalf("bucket rotation = %q, want %q", buckets, want)
	}
}

func TestCandidateSamplerBalancesBucketsWithinOneRefresh(t *testing.T) {
	candidates := []Proxy{
		{IP: "192.0.2.1", Port: "1080", Protocol: "socks5", SourceName: "alpha"},
		{IP: "192.0.2.2", Port: "1080", Protocol: "socks5", SourceName: "alpha"},
		{IP: "192.0.2.3", Port: "8080", Protocol: "http", SourceName: "beta"},
		{IP: "192.0.2.4", Port: "8080", Protocol: "http", SourceName: "beta"},
		{IP: "192.0.2.5", Port: "3128", Protocol: "http", SourceName: "gamma"},
	}
	selected := newCandidateSampler("").selectCandidates(candidates, nil, 3)
	if len(selected) != 3 {
		t.Fatalf("selection length = %d, want 3", len(selected))
	}
	seen := make(map[string]bool)
	for _, px := range selected {
		seen[candidateBucketKey(px)] = true
	}
	for _, bucket := range []string{"alpha\x00socks5", "beta\x00http", "gamma\x00http"} {
		if !seen[bucket] {
			t.Errorf("bucket %q was starved: selected=%#v", bucket, selected)
		}
	}
}

func TestCandidateSamplerUsesKnownOnlyAfterUnseenCandidates(t *testing.T) {
	candidates := []Proxy{
		{IP: "192.0.2.1", Port: "8080", Protocol: "http", SourceName: "alpha"},
		{IP: "192.0.2.2", Port: "8080", Protocol: "http", SourceName: "alpha"},
		{IP: "192.0.2.3", Port: "8080", Protocol: "http", SourceName: "alpha"},
	}
	known := map[string]bool{
		candidates[0].Key(): true,
		candidates[2].Key(): true,
	}
	selected := newCandidateSampler("").selectCandidates(candidates, known, 3)
	if len(selected) != 3 {
		t.Fatalf("selection length = %d, want 3", len(selected))
	}
	if selected[0].Key() != candidates[1].Key() {
		t.Fatalf("first selection = %q, want unseen %q", selected[0].Key(), candidates[1].Key())
	}
	if !known[selected[1].Key()] || !known[selected[2].Key()] {
		t.Fatalf("known fallback = %#v, want only known entries after unseen first", selected)
	}
}

func TestCandidateSamplerDoesNotRescanExhaustedKnownBucket(t *testing.T) {
	// The "known" bucket is deliberately much larger than the cap. Each
	// selection must move on to the unseen bucket after it has established
	// that known is exhausted for the discovery phase; re-scanning it for all
	// 250 slots turns this into O(limit*n) work in real large feeds.
	const knownCount = 100_000
	const unseenCount = 250
	candidates := make([]Proxy, 0, knownCount+unseenCount)
	known := make(map[string]bool, knownCount)
	for i := 0; i < knownCount; i++ {
		px := Proxy{
			IP:         fmt.Sprintf("198.18.%d.%d", i/250, i%250+1),
			Port:       "8080",
			Protocol:   "http",
			SourceName: "known",
		}
		candidates = append(candidates, px)
		known[px.Key()] = true
	}
	for i := 0; i < unseenCount; i++ {
		candidates = append(candidates, Proxy{
			IP:         fmt.Sprintf("203.0.113.%d", i%250+1),
			Port:       fmt.Sprintf("%d", 10000+i),
			Protocol:   "socks5",
			SourceName: "unseen",
		})
	}

	knownBucket := candidateBucketKey(candidates[0])
	unseenBucket := candidateBucketKey(candidates[knownCount])
	buckets := map[string][]int{
		knownBucket:  make([]int, knownCount),
		unseenBucket: make([]int, unseenCount),
	}
	for i := range buckets[knownBucket] {
		buckets[knownBucket][i] = i
	}
	for i := range buckets[unseenBucket] {
		buckets[unseenBucket][i] = knownCount + i
	}
	for _, bucket := range buckets {
		sort.Slice(bucket, func(i, j int) bool { return candidateCursorLess(candidates[bucket[i]], candidates[bucket[j]]) })
	}
	keys := []string{knownBucket, unseenBucket}
	sort.Strings(keys)

	sampler := newCandidateSampler("")
	exhausted := make(map[string]bool)
	selected := make([]Proxy, 0, unseenCount)
	sampler.selectPhase(candidates, keys, buckets, known, make(map[string]bool), unseenCount, false, exhausted, &selected)
	if len(selected) != unseenCount {
		t.Fatalf("selection length = %d, want %d", len(selected), unseenCount)
	}
	if !exhausted[knownBucket] {
		t.Fatal("all-known bucket was not marked exhausted for the rest of this refresh phase")
	}
	for _, px := range selected {
		if known[px.Key()] {
			t.Fatalf("known candidate selected before unseen pool exhausted: %#v", px)
		}
	}
}

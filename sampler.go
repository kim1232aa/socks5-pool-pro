package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// candidateSamplerState is deliberately small: source feeds themselves can
// contain hundreds of thousands of entries, but a cursor per source/protocol
// bucket is enough to visit them all across refreshes.  It is kept separate
// from pool_cache.json because candidates that have not yet passed a health
// check must not be retained as live proxy state.
type candidateSamplerState struct {
	Version    int               `json:"version"`
	LastBucket string            `json:"last_bucket,omitempty"`
	Cursors    map[string]string `json:"cursors"`
}

// candidateSampler selects a bounded, deterministic slice of a much larger
// source inventory.  Its cursors point at the last *examined* candidate, not
// merely the last successful one: repeatedly failing new entries therefore do
// not monopolize every subsequent refresh.
type candidateSampler struct {
	path  string
	state candidateSamplerState
}

const candidateSamplerStateFile = "candidate_sampler.json"

const (
	maxCandidateSamplerBytes   = 1 << 20
	maxCandidateSamplerCursors = 1024
)

func newCandidateSampler(dataDir string) *candidateSampler {
	s := &candidateSampler{
		state: candidateSamplerState{
			Version: 2,
			Cursors: make(map[string]string),
		},
	}
	if dataDir == "" {
		// Tests and callers that intentionally use an in-memory configuration
		// still get deterministic rotation for this refresh, just no restart
		// persistence.
		return s
	}

	s.path = filepath.Join(dataDir, candidateSamplerStateFile)
	data, err := readPrivateRegularFile(s.path, maxCandidateSamplerBytes)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[sampler] read state failed: %v", err)
		}
		return s
	}
	saved, err := decodeCandidateSamplerState(data)
	if err != nil {
		log.Printf("[sampler] parse state failed: %v", err)
		return s
	}
	if saved.Cursors == nil {
		saved.Cursors = make(map[string]string)
	}
	// Version 2 cursor values use allocation-light IP\x00port ordering rather
	// than Proxy.Key ordering. Old cursors are safe to discard: this only
	// restarts discovery rotation once and does not affect pool state.
	if saved.Version != 2 {
		saved.Version = 2
		saved.LastBucket = ""
		saved.Cursors = make(map[string]string)
	}
	s.state = saved
	return s
}

// selectCandidates returns at most limit candidates. It spends the first slots
// on unseen entries; already-known nodes are skipped during that pass while
// still moving the cursor past them. If the inventory has fewer unseen entries
// than the cap, a second pass fills the spare slots with known ones so a
// refresh does not leave useful checking capacity idle. Buckets are
// source+protocol scoped and chosen round-robin, so a large feed cannot starve
// smaller feeds and a limit smaller than the number of buckets remains fair
// across refreshes.
func (s *candidateSampler) selectCandidates(candidates []Proxy, known map[string]bool, limit int) []Proxy {
	if limit <= 0 || len(candidates) == 0 {
		return nil
	}

	buckets := make(map[string][]int)
	bucketKeyCache := make(map[string]map[string]string)
	for i, px := range candidates {
		name := px.SourceName
		if name == "" {
			name = "unknown"
		}
		byProtocol := bucketKeyCache[name]
		if byProtocol == nil {
			byProtocol = make(map[string]string)
			bucketKeyCache[name] = byProtocol
		}
		key, ok := byProtocol[px.Protocol]
		if !ok {
			key = name + "\x00" + px.Protocol
			byProtocol[px.Protocol] = key
		}
		buckets[key] = append(buckets[key], i)
	}
	keys := make([]string, 0, len(buckets))
	for key, bucket := range buckets {
		if !sort.SliceIsSorted(bucket, func(i, j int) bool {
			return candidateCursorLess(candidates[bucket[i]], candidates[bucket[j]])
		}) {
			sort.Slice(bucket, func(i, j int) bool {
				return candidateCursorLess(candidates[bucket[i]], candidates[bucket[j]])
			})
		}
		buckets[key] = bucket
		keys = append(keys, key)
	}
	sort.Strings(keys)
	s.pruneCursors(keys)

	selected := make(map[string]bool, limit)
	out := make([]Proxy, 0, limit)
	s.selectPhase(candidates, keys, buckets, known, selected, limit, false, make(map[string]bool, len(keys)), &out)
	// Keep the original "unseen first" behavior without wasting capacity
	// when a feed is almost entirely already represented in the pool.
	s.selectPhase(candidates, keys, buckets, known, selected, limit, true, make(map[string]bool, len(keys)), &out)

	if err := s.save(); err != nil {
		log.Printf("[sampler] save state failed: %v", err)
	}
	return out
}

// selectPhase does one round-robin pass category at a time. knownPhase=false
// selects only candidates absent from known; knownPhase=true selects only
// already-known candidates after discovery has exhausted all unseen choices.
func (s *candidateSampler) selectPhase(candidates []Proxy, keys []string, buckets map[string][]int, known, selected map[string]bool, limit int, knownPhase bool, exhausted map[string]bool, out *[]Proxy) {
	// A bucket that has no candidate for this phase stays empty until the
	// next refresh. Remember that fact locally: otherwise a 100k-entry bucket
	// full of already-known nodes would be walked again for every slot filled
	// from another bucket (O(limit*n) work).
	for len(*out) < limit {
		start := firstBucketAfter(keys, s.state.LastBucket)
		picked := false
		for offset := 0; offset < len(keys); offset++ {
			bucketKey := keys[(start+offset)%len(keys)]
			if exhausted[bucketKey] {
				continue
			}
			px, ok := s.nextMatching(candidates, bucketKey, buckets[bucketKey], known, selected, knownPhase)
			if !ok {
				exhausted[bucketKey] = true
				continue
			}
			*out = append(*out, px)
			selected[px.Key()] = true
			s.state.LastBucket = bucketKey
			picked = true
			break
		}
		if !picked {
			return
		}
	}
}

func candidateCursorLess(a, b Proxy) bool {
	if a.IP != b.IP {
		return a.IP < b.IP
	}
	return a.Port < b.Port
}

func candidateCursorKey(px Proxy) string { return px.IP + "\x00" + px.Port }

func candidateBucketKey(px Proxy) string {
	name := px.SourceName
	if name == "" {
		name = "unknown"
	}
	return name + "\x00" + px.Protocol
}

func firstBucketAfter(keys []string, last string) int {
	if len(keys) == 0 {
		return 0
	}
	idx := sort.Search(len(keys), func(i int) bool { return keys[i] > last })
	if idx == len(keys) {
		return 0
	}
	return idx
}

// nextMatching starts strictly after the saved cursor and advances the cursor
// for every entry it examines. selected prevents the same failing-but-unseen
// node from being selected twice in a single refresh.
func (s *candidateSampler) nextMatching(candidates []Proxy, bucketKey string, bucket []int, known, selected map[string]bool, knownPhase bool) (Proxy, bool) {
	if len(bucket) == 0 {
		return Proxy{}, false
	}
	start := sort.Search(len(bucket), func(i int) bool {
		return candidateCursorKey(candidates[bucket[i]]) > s.state.Cursors[bucketKey]
	})
	if start == len(bucket) {
		start = 0
	}
	for i := 0; i < len(bucket); i++ {
		px := candidates[bucket[(start+i)%len(bucket)]]
		key := px.Key()
		s.state.Cursors[bucketKey] = candidateCursorKey(px)
		if selected[key] || known[key] != knownPhase {
			continue
		}
		return px, true
	}
	return Proxy{}, false
}

func (s *candidateSampler) pruneCursors(keys []string) {
	if len(s.state.Cursors) == 0 {
		return
	}
	active := make(map[string]bool, len(keys))
	for _, key := range keys {
		active[key] = true
	}
	for key := range s.state.Cursors {
		if !active[key] {
			delete(s.state.Cursors, key)
		}
	}
}

func (s *candidateSampler) save() error {
	if s.path == "" {
		return nil
	}
	if err := validateCandidateSamplerState(s.state); err != nil {
		return err
	}
	data, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	if len(data) > maxCandidateSamplerBytes {
		return fmt.Errorf("sampler state exceeds %d bytes", maxCandidateSamplerBytes)
	}
	return writePrivateFileAtomic(s.path, data)
}

func decodeCandidateSamplerState(data []byte) (candidateSamplerState, error) {
	state := candidateSamplerState{Cursors: make(map[string]string)}
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return state, err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return state, fmt.Errorf("expected an object")
	}
	seenFields := make(map[string]bool, 3)
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return state, err
		}
		field, ok := fieldToken.(string)
		if !ok {
			return state, fmt.Errorf("field name is not a string")
		}
		if seenFields[field] {
			return state, fmt.Errorf("duplicate field %q", field)
		}
		seenFields[field] = true
		switch field {
		case "version":
			err = decoder.Decode(&state.Version)
		case "last_bucket":
			err = decoder.Decode(&state.LastBucket)
		case "cursors":
			state.Cursors, err = decodeCandidateSamplerCursors(decoder)
		default:
			err = skipJSONValue(decoder)
		}
		if err != nil {
			return state, fmt.Errorf("field %q: %w", field, err)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return state, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return state, err
	}
	if state.Cursors == nil {
		state.Cursors = make(map[string]string)
	}
	if err := validateCandidateSamplerState(state); err != nil {
		return state, err
	}
	return state, nil
}

func decodeCandidateSamplerCursors(decoder *json.Decoder) (map[string]string, error) {
	opening, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if opening == nil {
		return make(map[string]string), nil
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("expected an object")
	}
	out := make(map[string]string)
	records := 0
	for decoder.More() {
		records++
		if records > maxCandidateSamplerCursors {
			return nil, fmt.Errorf("cursor count exceeds %d", maxCandidateSamplerCursors)
		}
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("cursor key is not a string")
		}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if _, exists := out[key]; exists {
			return nil, fmt.Errorf("duplicate cursor %q", key)
		}
		out[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

func validateCandidateSamplerState(state candidateSamplerState) error {
	if len(state.Cursors) > maxCandidateSamplerCursors {
		return fmt.Errorf("cursor count exceeds %d", maxCandidateSamplerCursors)
	}
	if state.LastBucket != "" {
		_, protocol, ok := splitSamplerComposite(state.LastBucket, maxSourceNameBytes, 32)
		if !ok || !isProxyProtocol(protocol) {
			return fmt.Errorf("invalid last bucket")
		}
	}
	for bucket, cursor := range state.Cursors {
		_, protocol, ok := splitSamplerComposite(bucket, maxSourceNameBytes, 32)
		if !ok || !isProxyProtocol(protocol) {
			return fmt.Errorf("invalid cursor bucket")
		}
		host, port, ok := splitSamplerComposite(cursor, maxSourceAddressBytes, maxSourcePortBytes)
		if !ok {
			return fmt.Errorf("invalid cursor value")
		}
		if _, valid := normalizeProxy(Proxy{IP: host, Port: port, Protocol: protocol}); !valid {
			return fmt.Errorf("invalid cursor address")
		}
	}
	return nil
}

func splitSamplerComposite(value string, maxLeft, maxRight int) (string, string, bool) {
	if strings.Count(value, "\x00") != 1 {
		return "", "", false
	}
	left, right, _ := strings.Cut(value, "\x00")
	if left == "" || right == "" || len(left) > maxLeft || len(right) > maxRight {
		return "", "", false
	}
	for _, part := range []string{left, right} {
		for _, r := range part {
			if r < 0x20 || r == 0x7f {
				return "", "", false
			}
		}
	}
	return left, right, true
}

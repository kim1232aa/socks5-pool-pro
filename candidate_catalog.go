package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CandidateStatus describes where an address from the latest source inventory
// sits in the discovery pipeline. Candidates deliberately live outside
// ProxyPool: hundreds of thousands of unverified addresses must never become
// routable merely because a source advertised them.
type CandidateStatus uint8

const (
	candidateDeferred CandidateStatus = iota
	candidateCheckedFailed
	candidatePolicyFiltered
	candidateKnownAvailable
	candidateKnownUnavailable
	candidateResource
)

func (s CandidateStatus) String() string {
	switch s {
	case candidateCheckedFailed:
		return "checked_failed"
	case candidatePolicyFiltered:
		return "policy_filtered"
	case candidateKnownAvailable:
		return "known_available"
	case candidateKnownUnavailable:
		return "known_unavailable"
	case candidateResource:
		return "resource"
	default:
		return "deferred"
	}
}

// candidateRecord keeps per-candidate connection data compact while retaining
// the exact upstream credentials needed by API, dashboard, and cache consumers.
// Repeated source/protocol/country/city values are interned in snapshot-level
// dictionaries, while multi-source attribution uses one flat uint32 array.
type candidateRecord struct {
	addr         string
	username     string
	password     string
	sourceOffset uint32
	countryID    uint32
	cityID       uint32
	protocolID   uint16
	sourceCount  uint16
	continent    uint8
	status       CandidateStatus
	hasAuth      bool
	seenUnix     int64
	checkedUnix  int64

	credentialAlternateOffset uint32
	credentialAlternateCount  uint8
}

type CandidateFailureKind uint8

const (
	candidateFailureUnreachable CandidateFailureKind = iota + 1
	candidateFailurePolicyFiltered
)

func (k CandidateFailureKind) String() string {
	if k == candidateFailurePolicyFiltered {
		return "policy_filtered"
	}
	return "unreachable"
}

type candidateFailureRecord struct {
	candidateRecord
	kind      CandidateFailureKind
	lastError string
}

type candidateSnapshot struct {
	mu sync.RWMutex

	records                  []candidateRecord
	failedRecords            []candidateFailureRecord
	credentialAlternateTable []ProxyCredential
	sourceRefs               []uint32
	sourceKeys               []string // stable Source.ID (or a legacy synthetic key)
	sources                  []string // display names parallel to sourceKeys
	protocols                []string
	countries                []string // index 0 is always unknown
	cities                   []string // index 0 is always empty
	sourceTotals             []int
	sourceFacetValues        []string
	sourceFacetTotals        []int
	protocolTotals           []int

	generation     uint64
	revision       uint64 // changes when complete mutates phase/check outcomes in-place
	phase          string
	sourceErrors   int
	seenAt         time.Time
	refreshAttempt time.Time
	completedAt    time.Time
}

// CandidateCatalog atomically swaps complete inventory generations. A small
// per-snapshot RWMutex protects only sparse health outcomes (at most the
// bounded checked set), avoiding a full 500k-record copy at completion while
// ensuring page readers never observe a half-applied result set.
type CandidateCatalog struct {
	nextGeneration atomic.Uint64
	snapshot       atomic.Pointer[candidateSnapshot]
	publicationMu  sync.RWMutex
	cacheMu        sync.RWMutex
	cache          *candidateCatalogCache
	persistMu      sync.Mutex
	removalMu      sync.Mutex
	removing       map[string]struct{} // guarded by publicationMu

	leaseMu            sync.Mutex
	nextLeaseToken     uint64
	pendingLeases      map[string]candidateLeaseState
	failedLeases       map[string]candidateLeaseState
	pendingLeaseCursor int

	// Deterministic test seams around persistence and removal publication.
	persistLocked      func()
	removeBeforeCommit func()
	removeAfterPersist func()
	beginBeforePublish func()
}

type CandidateLease struct {
	Token      uint64
	Key        string
	Proxy      Proxy
	Kind       string
	Snapshot   *candidateSnapshot
	Generation uint64
	Revision   uint64
}

type candidateLeaseState struct {
	token       uint64
	fingerprint string
}

type candidatePromotionLease struct {
	snapshot   *candidateSnapshot
	generation uint64
	revision   uint64
	key        string
}

// FindByKey returns the exact protocol-aware candidate declaration. It is
// intentionally inventory-only: callers must still validate before admitting a
// candidate to the forwarding pool.
func (c *CandidateCatalog) FindByKey(key string) (Proxy, bool) {
	snapshot := c.snapshot.Load()
	if snapshot == nil {
		return Proxy{}, false
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	return candidateProxyByKeyLocked(snapshot, key)
}

// leaseByKey captures the catalog identity that authorized asynchronous
// candidate verification. Publication must later use withPromotionLease rather
// than a fresh lookup: finding the same key in a replacement snapshot does not
// prove that the tested declaration is still current.
func (c *CandidateCatalog) leaseByKey(key string) (Proxy, candidatePromotionLease, bool) {
	c.publicationMu.RLock()
	defer c.publicationMu.RUnlock()
	snapshot := c.snapshot.Load()
	if snapshot == nil {
		return Proxy{}, candidatePromotionLease{}, false
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	px, ok := candidateProxyByKeyLocked(snapshot, key)
	if !ok {
		return Proxy{}, candidatePromotionLease{}, false
	}
	return px, candidatePromotionLease{
		snapshot: snapshot, generation: snapshot.generation, revision: snapshot.revision, key: key,
	}, true
}

// withPromotionLease validates and holds one catalog lease through the pool
// publication callback. Candidate removal, snapshot replacement, and revision
// changes are excluded until the callback returns, closing the final lookup to
// promotion TOCTOU window.
func (c *CandidateCatalog) withPromotionLease(lease candidatePromotionLease, promote func(Proxy) bool) bool {
	if lease.snapshot == nil || promote == nil {
		return false
	}
	c.publicationMu.RLock()
	defer c.publicationMu.RUnlock()
	if c.snapshot.Load() != lease.snapshot {
		return false
	}
	if _, pending := c.removing[lease.key]; pending {
		return false
	}
	lease.snapshot.mu.RLock()
	defer lease.snapshot.mu.RUnlock()
	if lease.snapshot.generation != lease.generation || lease.snapshot.revision != lease.revision {
		return false
	}
	px, ok := candidateProxyByKeyLocked(lease.snapshot, lease.key)
	return ok && promote(px)
}

func candidateProxyByKeyLocked(snapshot *candidateSnapshot, key string) (Proxy, bool) {
	protocol, addr, ok := strings.Cut(key, "://")
	if !ok || protocol == "" || addr == "" {
		return Proxy{}, false
	}
	protocol = strings.ToLower(protocol)
	index := snapshot.find(protocol, addr)
	if index < 0 {
		return Proxy{}, false
	}
	return candidateProxyFromRecordLocked(snapshot, snapshot.records[index])
}

func failedCandidateProxyByKeyLocked(snapshot *candidateSnapshot, key string) (Proxy, bool) {
	protocol, addr, ok := strings.Cut(key, "://")
	if !ok || protocol == "" || addr == "" {
		return Proxy{}, false
	}
	index := snapshot.findFailed(strings.ToLower(protocol), addr)
	if index < 0 {
		return Proxy{}, false
	}
	return candidateProxyFromRecordLocked(snapshot, snapshot.failedRecords[index].candidateRecord)
}

func candidateProxyFromRecordLocked(snapshot *candidateSnapshot, record candidateRecord) (Proxy, bool) {
	if snapshot.protocols[record.protocolID] == "proxyip" {
		return Proxy{}, false
	}
	host, port, err := net.SplitHostPort(record.addr)
	if err != nil {
		return Proxy{}, false
	}
	sourceNames := make([]string, 0, record.sourceCount)
	sourceIDs := make([]string, 0, record.sourceCount)
	for i := uint32(0); i < uint32(record.sourceCount); i++ {
		ref := snapshot.sourceRefs[record.sourceOffset+i]
		sourceKey, name := snapshot.sourceKeys[ref], snapshot.sources[ref]
		if strings.TrimSpace(name) != "" && !strings.EqualFold(name, "Unknown") {
			sourceNames = append(sourceNames, name)
		}
		if !strings.HasPrefix(sourceKey, "legacy-name:") {
			sourceIDs = append(sourceIDs, sourceKey)
		}
	}
	sort.Strings(sourceNames)
	sort.Strings(sourceIDs)
	px := Proxy{
		IP: host, Port: port, Protocol: snapshot.protocols[record.protocolID],
		Username: record.username, Password: record.password,
		Country: snapshot.countries[record.countryID], City: snapshot.cities[record.cityID],
		Continent: decodeContinent(record.continent), SourceNames: sourceNames, SourceIDs: sourceIDs,
	}
	if len(sourceNames) > 0 {
		px.SourceName = sourceNames[0]
	}
	if record.credentialAlternateCount > 0 {
		start := record.credentialAlternateOffset
		end := start + uint32(record.credentialAlternateCount)
		if end <= uint32(len(snapshot.credentialAlternateTable)) {
			px.CredentialAlternates = append([]ProxyCredential(nil), snapshot.credentialAlternateTable[start:end]...)
		}
	}
	return px, true
}

func candidateLeaseFingerprint(px Proxy) string {
	var builder strings.Builder
	builder.WriteString(px.Key())
	builder.WriteByte(0)
	builder.WriteString(px.Username)
	builder.WriteByte(0)
	builder.WriteString(px.Password)
	for _, credential := range px.CredentialAlternates {
		builder.WriteByte(0)
		builder.WriteString(credential.Username)
		builder.WriteByte(0)
		builder.WriteString(credential.Password)
	}
	return builder.String()
}

func (c *CandidateCatalog) newLeaseLocked(snapshot *candidateSnapshot, key, kind string, px Proxy, leases map[string]candidateLeaseState) CandidateLease {
	c.nextLeaseToken++
	if c.nextLeaseToken == 0 {
		c.nextLeaseToken++
	}
	state := candidateLeaseState{token: c.nextLeaseToken, fingerprint: candidateLeaseFingerprint(px)}
	leases[key] = state
	return CandidateLease{
		Token: state.token, Key: key, Proxy: px, Kind: kind,
		Snapshot: snapshot, Generation: snapshot.generation, Revision: snapshot.revision,
	}
}

func (c *CandidateCatalog) LeasePending(limit int, known candidateKnownIndex) []CandidateLease {
	if limit <= 0 {
		return nil
	}
	snapshot := c.snapshot.Load()
	if snapshot == nil {
		return nil
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()
	if c.pendingLeases == nil {
		c.pendingLeases = make(map[string]candidateLeaseState)
	}
	result := make([]CandidateLease, 0, limit)
	if len(snapshot.records) == 0 {
		return result
	}
	start := c.pendingLeaseCursor % len(snapshot.records)
	for scanned := 0; scanned < len(snapshot.records) && len(result) < limit; scanned++ {
		index := (start + scanned) % len(snapshot.records)
		record := snapshot.records[index]
		if !candidatePendingRecord(snapshot, record, known) {
			continue
		}
		key := snapshot.protocols[record.protocolID] + "://" + record.addr
		if _, leased := c.pendingLeases[key]; leased {
			continue
		}
		px, ok := candidateProxyFromRecordLocked(snapshot, record)
		if !ok {
			continue
		}
		result = append(result, c.newLeaseLocked(snapshot, key, "candidate", px, c.pendingLeases))
		c.pendingLeaseCursor = (index + 1) % len(snapshot.records)
	}
	return result
}

func (c *CandidateCatalog) LeasePendingKeys(keys []string, known candidateKnownIndex) []CandidateLease {
	if len(keys) == 0 {
		return nil
	}
	snapshot := c.snapshot.Load()
	if snapshot == nil {
		return nil
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()
	if c.pendingLeases == nil {
		c.pendingLeases = make(map[string]candidateLeaseState)
	}
	result := make([]CandidateLease, 0, len(keys))
	for _, key := range keys {
		if _, leased := c.pendingLeases[key]; leased {
			continue
		}
		protocol, addr, ok := strings.Cut(key, "://")
		if !ok {
			continue
		}
		index := snapshot.find(strings.ToLower(protocol), addr)
		if index < 0 || !candidatePendingRecord(snapshot, snapshot.records[index], known) {
			continue
		}
		canonicalKey := snapshot.protocols[snapshot.records[index].protocolID] + "://" + snapshot.records[index].addr
		if key != canonicalKey {
			continue
		}
		px, ok := candidateProxyFromRecordLocked(snapshot, snapshot.records[index])
		if ok {
			result = append(result, c.newLeaseLocked(snapshot, key, "candidate", px, c.pendingLeases))
		}
	}
	return result
}

// FilterPendingCandidates compacts one source-derived work slice to the exact
// entries still owned by the pending catalog. Failed records, ProxyIP resources,
// and every key already owned by ProxyPool are removed before MaxCandidates is
// applied, so they cannot consume a bounded automatic-check slot.
func (c *CandidateCatalog) FilterPendingCandidates(candidates []Proxy, known candidateKnownIndex) []Proxy {
	snapshot := c.snapshot.Load()
	if snapshot == nil || len(candidates) == 0 {
		return nil
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	write := 0
	for _, px := range candidates {
		index := snapshot.find(strings.ToLower(px.Protocol), px.Addr())
		if index < 0 || !candidatePendingRecord(snapshot, snapshot.records[index], known) {
			continue
		}
		candidates[write] = px
		write++
	}
	for i := write; i < len(candidates); i++ {
		candidates[i] = Proxy{}
	}
	return candidates[:write:write]
}

// ValidateFailedKeys performs the read-only, all-or-nothing validation used by
// the failed-retry API before it creates an operation. Exact canonical keys are
// required so a case-folded or otherwise rewritten key cannot select a different
// catalog record than the one the administrator saw.
func (c *CandidateCatalog) ValidateFailedKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	snapshot := c.snapshot.Load()
	if snapshot == nil {
		return append([]string(nil), keys...)
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	missing := make([]string, 0)
	for _, key := range keys {
		protocol, addr, ok := strings.Cut(key, "://")
		if !ok {
			missing = append(missing, key)
			continue
		}
		index := snapshot.findFailed(strings.ToLower(protocol), addr)
		if index < 0 {
			missing = append(missing, key)
			continue
		}
		canonicalKey := snapshot.protocols[snapshot.failedRecords[index].protocolID] + "://" + snapshot.failedRecords[index].addr
		if key != canonicalKey {
			missing = append(missing, key)
		}
	}
	return missing
}

func (c *CandidateCatalog) LeaseFailed(keys []string) ([]CandidateLease, []string) {
	if len(keys) == 0 {
		return nil, nil
	}
	snapshot := c.snapshot.Load()
	if snapshot == nil {
		return nil, append([]string(nil), keys...)
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()
	if c.failedLeases == nil {
		c.failedLeases = make(map[string]candidateLeaseState)
	}
	missing := make([]string, 0)
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, leased := c.failedLeases[key]; leased {
			missing = append(missing, key)
			continue
		}
		protocol, addr, ok := strings.Cut(key, "://")
		if !ok {
			missing = append(missing, key)
			continue
		}
		index := snapshot.findFailed(strings.ToLower(protocol), addr)
		if index < 0 {
			missing = append(missing, key)
			continue
		}
		canonicalKey := snapshot.protocols[snapshot.failedRecords[index].protocolID] + "://" + snapshot.failedRecords[index].addr
		if key != canonicalKey {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, missing
	}
	result := make([]CandidateLease, 0, len(seen))
	seen = make(map[string]bool, len(keys))
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		px, _ := failedCandidateProxyByKeyLocked(snapshot, key)
		result = append(result, c.newLeaseLocked(snapshot, key, "failed", px, c.failedLeases))
	}
	return result, nil
}

func (c *CandidateCatalog) ReleaseLeases(leases []CandidateLease) {
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()
	for _, lease := range leases {
		states := c.pendingLeases
		if lease.Kind == "failed" {
			states = c.failedLeases
		}
		if state, ok := states[lease.Key]; ok && state.token == lease.Token {
			delete(states, lease.Key)
		}
	}
}

// withCandidateLease keeps one pending declaration and its runtime lease
// current through a pool-promotion callback. It is the single-entry equivalent
// of CommitLeaseOutcomes' validation boundary, without changing catalog state.
func (c *CandidateCatalog) withCandidateLease(lease CandidateLease, promote func(Proxy) bool) bool {
	if lease.Kind != "candidate" || promote == nil {
		return false
	}
	c.publicationMu.RLock()
	defer c.publicationMu.RUnlock()
	current := c.snapshot.Load()
	if current == nil || current != lease.Snapshot {
		return false
	}
	current.mu.RLock()
	defer current.mu.RUnlock()
	if current.generation != lease.Generation || current.revision != lease.Revision {
		return false
	}
	declared, ok := candidateProxyByKeyLocked(current, lease.Key)
	if !ok || candidateLeaseFingerprint(declared) != candidateLeaseFingerprint(lease.Proxy) {
		return false
	}
	c.leaseMu.Lock()
	state, currentLease := c.pendingLeases[lease.Key]
	valid := currentLease && state.token == lease.Token && state.fingerprint == candidateLeaseFingerprint(declared)
	c.leaseMu.Unlock()
	return valid && promote(declared)
}

func (c *CandidateCatalog) CommitLeaseOutcomes(leases []CandidateLease, outcomes map[string]candidateCheckOutcome) error {
	if len(leases) == 0 {
		return nil
	}
	defer c.ReleaseLeases(leases)
	c.publicationMu.Lock()
	defer c.publicationMu.Unlock()
	current := c.snapshot.Load()
	if current == nil {
		return fmt.Errorf("candidate catalog is empty")
	}
	current.mu.RLock()
	baseRevision := current.revision
	c.leaseMu.Lock()
	for _, lease := range leases {
		states := c.pendingLeases
		lookup := candidateProxyByKeyLocked
		if lease.Kind == "failed" {
			states = c.failedLeases
			lookup = failedCandidateProxyByKeyLocked
		} else if lease.Kind != "candidate" {
			c.leaseMu.Unlock()
			current.mu.RUnlock()
			return fmt.Errorf("candidate lease %q has invalid kind %q", lease.Key, lease.Kind)
		}
		state, ok := states[lease.Key]
		if !ok || state.token != lease.Token {
			c.leaseMu.Unlock()
			current.mu.RUnlock()
			return fmt.Errorf("candidate lease %q is no longer current", lease.Key)
		}
		declared, ok := lookup(current, lease.Key)
		if !ok || state.fingerprint != candidateLeaseFingerprint(declared) || state.fingerprint != candidateLeaseFingerprint(lease.Proxy) {
			c.leaseMu.Unlock()
			current.mu.RUnlock()
			return fmt.Errorf("candidate lease %q declaration changed", lease.Key)
		}
		if outcome, exists := outcomes[lease.Key]; exists && outcome.Key != "" && outcome.Key != lease.Key {
			c.leaseMu.Unlock()
			current.mu.RUnlock()
			return fmt.Errorf("candidate lease %q outcome key mismatch", lease.Key)
		}
	}
	c.leaseMu.Unlock()

	next := cloneCandidateSnapshotLocked(current)
	current.mu.RUnlock()
	checkedAt := time.Now().Unix()
	changed := false
	for _, lease := range leases {
		outcome, hasOutcome := outcomes[lease.Key]
		if !hasOutcome || outcome.Kind == candidateCheckNoResult || outcome.Kind == candidateCheckAlive && lease.Kind == "candidate" {
			continue
		}
		if lease.Kind == "candidate" {
			index := next.find(lease.Proxy.Protocol, lease.Proxy.Addr())
			if index < 0 || outcome.Kind != candidateCheckUnreachable && outcome.Kind != candidateCheckPolicyFiltered {
				continue
			}
			record := next.records[index]
			record.checkedUnix = checkedAt
			kind := candidateFailureUnreachable
			if outcome.Kind == candidateCheckPolicyFiltered {
				kind = candidateFailurePolicyFiltered
			}
			next.records = append(next.records[:index], next.records[index+1:]...)
			next.failedRecords = append(next.failedRecords, candidateFailureRecord{
				candidateRecord: record, kind: kind, lastError: sanitizeCandidateFailureError(outcome.Error),
			})
			sort.Slice(next.failedRecords, func(i, j int) bool {
				return compareCandidateRecords(next, next.failedRecords[i].candidateRecord, next, next.failedRecords[j].candidateRecord) < 0
			})
			changed = true
			continue
		}
		index := next.findFailed(lease.Proxy.Protocol, lease.Proxy.Addr())
		if index < 0 {
			continue
		}
		switch outcome.Kind {
		case candidateCheckAlive:
			next.failedRecords = append(next.failedRecords[:index], next.failedRecords[index+1:]...)
			changed = true
		case candidateCheckUnreachable, candidateCheckPolicyFiltered:
			record := &next.failedRecords[index]
			record.checkedUnix = checkedAt
			record.lastError = sanitizeCandidateFailureError(outcome.Error)
			record.kind = candidateFailureUnreachable
			if outcome.Kind == candidateCheckPolicyFiltered {
				record.kind = candidateFailurePolicyFiltered
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	next.revision = baseRevision + 1
	rebuildCandidateSourceFacets(next)
	cachePhase := next.phase
	if cachePhase == "checking" {
		if next.sourceErrors > 0 {
			next.phase = "partial"
		} else {
			next.phase = "complete"
		}
	}
	if err := c.persistImmutableSnapshot(next, false); err != nil {
		return fmt.Errorf("persist candidate lease outcomes: %w", err)
	}
	next.phase = cachePhase
	current.mu.RLock()
	consistent := c.snapshot.Load() == current && current.revision == baseRevision
	current.mu.RUnlock()
	if !consistent || !c.snapshot.CompareAndSwap(current, next) {
		if err := c.restoreLiveSnapshot(); err != nil {
			return fmt.Errorf("candidate lease outcome publication conflicted; restore live snapshot: %w", err)
		}
		return fmt.Errorf("candidate lease outcome publication conflicted")
	}
	return nil
}

func cloneCandidateSnapshotLocked(snapshot *candidateSnapshot) *candidateSnapshot {
	return &candidateSnapshot{
		records: append([]candidateRecord(nil), snapshot.records...), failedRecords: append([]candidateFailureRecord(nil), snapshot.failedRecords...),
		credentialAlternateTable: append([]ProxyCredential(nil), snapshot.credentialAlternateTable...),
		sourceRefs:               append([]uint32(nil), snapshot.sourceRefs...), sourceKeys: append([]string(nil), snapshot.sourceKeys...),
		sources: append([]string(nil), snapshot.sources...), protocols: append([]string(nil), snapshot.protocols...),
		countries: append([]string(nil), snapshot.countries...), cities: append([]string(nil), snapshot.cities...),
		sourceTotals: append([]int(nil), snapshot.sourceTotals...), sourceFacetValues: append([]string(nil), snapshot.sourceFacetValues...),
		sourceFacetTotals: append([]int(nil), snapshot.sourceFacetTotals...), protocolTotals: append([]int(nil), snapshot.protocolTotals...),
		generation: snapshot.generation, revision: snapshot.revision, phase: snapshot.phase, sourceErrors: snapshot.sourceErrors,
		seenAt: snapshot.seenAt, refreshAttempt: snapshot.refreshAttempt, completedAt: snapshot.completedAt,
	}
}

const maxCandidateFailureErrorLength = 512

func sanitizeCandidateFailureError(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > maxCandidateFailureErrorLength {
		value = value[:maxCandidateFailureErrorLength]
	}
	return value
}

const candidateRemovalMaxAttempts = 4

// RemoveKeys explicitly removes candidate inventory entries. It persists each
// proposed snapshot before publishing it, then commits only while the source
// pointer and revision still match the image that was copied. Concurrent source
// replacement or health outcomes therefore force a bounded rebuild/retry rather
// than being overwritten by stale status data.
func (c *CandidateCatalog) RemoveKeys(keys []string) (removed, notFound []string, persistErr error) {
	if len(keys) == 0 {
		return nil, nil, nil
	}
	c.removalMu.Lock()
	defer c.removalMu.Unlock()

	wanted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wanted[key] = struct{}{}
	}
	c.publicationMu.Lock()
	if c.removing == nil {
		c.removing = make(map[string]struct{}, len(wanted))
	}
	for key := range wanted {
		c.removing[key] = struct{}{}
	}
	c.publicationMu.Unlock()
	defer func() {
		c.publicationMu.Lock()
		for key := range wanted {
			delete(c.removing, key)
		}
		c.publicationMu.Unlock()
	}()

	for attempt := 0; attempt < candidateRemovalMaxAttempts; attempt++ {
		current := c.snapshot.Load()
		if current == nil {
			return nil, keys, nil
		}
		current.mu.RLock()
		builder := newCandidateSnapshotBuilder(len(current.records))
		found := make(map[string]bool, len(keys))
		for _, record := range current.records {
			key := current.protocols[record.protocolID] + "://" + record.addr
			if _, remove := wanted[key]; remove {
				found[key] = true
				continue
			}
			builder.appendRecord(current, record, nil)
		}
		for _, failure := range current.failedRecords {
			builder.appendFailure(current, failure)
		}
		next := builder.snapshot
		builder.finalizeCredentialAlternates()
		baseRevision := current.revision
		next.generation = current.generation
		next.revision = baseRevision + 1
		next.phase = current.phase
		next.sourceErrors = current.sourceErrors
		next.seenAt = current.seenAt
		next.refreshAttempt = current.refreshAttempt
		next.completedAt = current.completedAt
		rebuildCandidateSourceFacets(next)
		current.mu.RUnlock()

		removed = nil
		notFound = nil
		for _, key := range keys {
			if found[key] {
				removed = append(removed, key)
			} else {
				notFound = append(notFound, key)
			}
		}
		if len(removed) == 0 {
			return nil, notFound, nil
		}

		if c.removeBeforeCommit != nil {
			c.removeBeforeCommit()
		}

		// Reject an already-stale proposal before touching disk. The short lock
		// only validates the base token; persistence below holds neither catalog
		// publication nor live-snapshot locks.
		c.publicationMu.Lock()
		current.mu.Lock()
		consistent := c.snapshot.Load() == current && current.revision == baseRevision
		current.mu.Unlock()
		c.publicationMu.Unlock()
		if !consistent {
			continue
		}

		// Persist the detached immutable image without taking its snapshot lock.
		// Map an in-progress phase only in the cache image; publication below keeps
		// "checking" so the live refresh lifecycle remains accurate.
		cachePhase := next.phase
		if cachePhase == "checking" {
			if next.sourceErrors > 0 {
				next.phase = "partial"
			} else {
				next.phase = "complete"
			}
		}
		persistErr = c.persistImmutableSnapshot(next, false)
		next.phase = cachePhase
		if c.removeAfterPersist != nil {
			c.removeAfterPersist()
		}
		if persistErr != nil {
			if restoreErr := c.restoreLiveSnapshot(); restoreErr != nil {
				return nil, nil, fmt.Errorf("persist candidate removal: %w; restore live snapshot: %v", persistErr, restoreErr)
			}
			return nil, nil, persistErr
		}

		// Publish only if the exact base pointer/revision is still live. A conflict
		// means the deletion written above was never committed, so restore a current
		// live image before rebuilding and retrying.
		c.publicationMu.Lock()
		current.mu.Lock()
		consistent = c.snapshot.Load() == current && current.revision == baseRevision
		if consistent {
			consistent = c.snapshot.CompareAndSwap(current, next)
		}
		current.mu.Unlock()
		c.publicationMu.Unlock()
		if consistent {
			return removed, notFound, nil
		}
		if restoreErr := c.restoreLiveSnapshot(); restoreErr != nil {
			return nil, nil, fmt.Errorf("candidate removal lost publication guard; restore live snapshot: %w", restoreErr)
		}
	}
	return nil, nil, fmt.Errorf("candidate removal could not commit after retries: concurrent catalog contention")
}

// ResetHealthOutcomes invalidates criterion-dependent candidate annotations
// while retaining the full source inventory. Candidate cache format v1 did not
// persist the CheckURL, so cached checked_failed/policy_filtered labels cannot
// be trusted after a restart; the same reset is used immediately when an
// operator changes the URL. Live pool membership is still overlaid at read
// time, and later checks repopulate these annotations under the new standard.
func (c *CandidateCatalog) ResetHealthOutcomes() int {
	c.publicationMu.RLock()
	defer c.publicationMu.RUnlock()
	snapshot := c.snapshot.Load()
	if snapshot == nil {
		return 0
	}
	snapshot.mu.Lock()
	changed := 0
	for i := range snapshot.records {
		record := &snapshot.records[i]
		if snapshot.protocols[record.protocolID] == "proxyip" {
			continue
		}
		if record.status != candidateDeferred || record.checkedUnix != 0 {
			record.status = candidateDeferred
			record.checkedUnix = 0
			changed++
		}
	}
	if changed > 0 || snapshot.phase != "restored" {
		snapshot.phase = "restored"
		snapshot.completedAt = time.Time{}
		snapshot.revision++
	}
	snapshot.mu.Unlock()
	return changed
}

// ApplyHealthOutcomes keeps the candidate view consistent with periodic and
// exhaustive retained-pool rechecks. It deliberately does not alter source
// inventory or phase; those belong to the scrape lifecycle. Results are
// process-local (startup resets criterion-dependent cache annotations), so a
// five-minute recheck does not recompress the entire large catalog on disk.
func (c *CandidateCatalog) ApplyHealthOutcomes(_ []Proxy, _, _ map[string]bool) int {
	// Retained-pool health checks own only ProxyPool state. Candidate failures are
	// created exclusively by checks that hold candidate/failed catalog leases.
	return 0
}

func (c *CandidateCatalog) protocolTotal(protocol string) (int, bool) {
	snapshot := c.snapshot.Load()
	if snapshot == nil {
		return 0, false
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	total := 0
	for _, record := range snapshot.records {
		if strings.EqualFold(snapshot.protocols[record.protocolID], protocol) {
			total++
		}
	}
	return total, true
}

type candidateRefresh struct {
	generation uint64
}

type candidateKnownIndex map[string]map[string]bool // protocol -> addr -> available

// candidateKnownIndex returns a small, point-in-time availability overlay for
// current pool members. It lets periodic rechecks/manual verification show up
// immediately in candidate pages without copying availability into 500k rows.
func (p *ProxyPool) candidateKnownSnapshot() (candidateKnownIndex, uint64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	index := make(candidateKnownIndex, 4)
	type overlayEntry struct {
		protocol  string
		addr      string
		available bool
	}
	add := func(px Proxy) {
		byAddr := index[px.Protocol]
		if byAddr == nil {
			byAddr = make(map[string]bool)
			index[px.Protocol] = byAddr
		}
		byAddr[px.Addr()] = px.Available
	}
	for _, px := range p.proxies {
		add(px)
	}
	entries := make([]overlayEntry, 0, len(p.proxies))
	for protocol, byAddr := range index {
		for addr, available := range byAddr {
			entries = append(entries, overlayEntry{protocol: protocol, addr: addr, available: available})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].protocol != entries[j].protocol {
			return entries[i].protocol < entries[j].protocol
		}
		return entries[i].addr < entries[j].addr
	})
	// FNV-1a is sufficient here: the token is an optimistic consistency marker,
	// not an authentication primitive. It changes only when the candidate page's
	// known-membership/availability overlay changes, unlike cacheGeneration,
	// which also changes for every ordinary connection reliability statistic.
	hash := uint64(14695981039346656037)
	writeHash := func(value string) {
		for i := 0; i < len(value); i++ {
			hash ^= uint64(value[i])
			hash *= 1099511628211
		}
		hash ^= 0
		hash *= 1099511628211
	}
	for _, entry := range entries {
		writeHash(entry.protocol)
		writeHash(entry.addr)
		if entry.available {
			hash ^= 1
		}
		hash *= 1099511628211
	}
	return index, hash
}

func knownCandidateStatus(known candidateKnownIndex, protocol, addr string) (CandidateStatus, bool) {
	byAddr := known[protocol]
	available, ok := byAddr[addr]
	if !ok {
		return candidateDeferred, false
	}
	if available {
		return candidateKnownAvailable, true
	}
	return candidateKnownUnavailable, true
}

// begin publishes the source inventory immediately after deduplication, before
// health checks finish. A partial source cycle selectively retains attribution
// from failed feeds so a transient outage cannot make their entries disappear.
func (c *CandidateCatalog) begin(candidates []Proxy, sourceLabels map[string]string, failedSources map[string]bool, sourceErrors int) candidateRefresh {
	refresh := candidateRefresh{generation: c.nextGeneration.Add(1)}
	now := time.Now()

	// Retry against the current base instead of a blind Store: RemoveKeys can
	// publish a deletion (it holds no lock in common with this scrape cycle)
	// while the merge below is still running against an older snapshot. A
	// blind Store would silently resurrect what was just deleted, both in
	// memory and in the persisted image the end of this cycle writes to disk.
	for {
		previous := c.snapshot.Load()
		snapshot := buildCandidateSnapshot(candidates, sourceLabels)
		for i := range snapshot.records {
			snapshot.records[i].seenUnix = now.Unix()
		}
		// A partial scrape is merged with the previous inventory by source:
		// attribution from failed feeds remains visible, while successful feeds
		// are replaced by exactly what they advertised this cycle. This preserves
		// failures without turning removed entries from healthy feeds immortal.
		if previous != nil {
			previous.mu.RLock()
			if len(previous.records) > 0 {
				if len(failedSources) > 0 {
					snapshot = mergeCandidateSnapshots(previous, snapshot, failedSources)
				} else {
					carryCandidateHistory(previous, snapshot)
				}
			}
			snapshot = reconcileCandidateFailures(previous, snapshot)
			previous.mu.RUnlock()
		}
		snapshot.generation = refresh.generation
		snapshot.revision = 1
		snapshot.phase = "checking"
		snapshot.sourceErrors = sourceErrors
		snapshot.seenAt = now
		snapshot.refreshAttempt = now

		if c.beginBeforePublish != nil {
			c.beginBeforePublish()
		}
		c.publicationMu.Lock()
		published := c.snapshot.CompareAndSwap(previous, snapshot)
		c.publicationMu.Unlock()
		if published {
			return refresh
		}
	}
}

// carryCandidateHistory keeps inventory/source metadata authoritative to a
// fully successful current scrape while copying sparse check outcomes for the
// key intersection in place. It avoids allocating a second full output
// snapshot during the normal all-sources-success path.
func carryCandidateHistory(previous, current *candidateSnapshot) {
	i, j := 0, 0
	for i < len(previous.records) && j < len(current.records) {
		oldRecord, newRecord := previous.records[i], &current.records[j]
		switch compareCandidateRecords(previous, oldRecord, current, *newRecord) {
		case -1:
			i++
		case 1:
			j++
		default:
			if current.protocols[newRecord.protocolID] != "proxyip" {
				newRecord.status = oldRecord.status
				newRecord.checkedUnix = oldRecord.checkedUnix
			}
			i++
			j++
		}
	}
	current.completedAt = previous.completedAt
}

// reconcileCandidateFailures carries the durable failure collection across
// source generations. A rediscovered failed key is removed from pending and its
// current declaration is merged into the existing failure without changing the
// last checked conclusion. Failures not rediscovered remain retained.
func reconcileCandidateFailures(previous, current *candidateSnapshot) *candidateSnapshot {
	if len(previous.failedRecords) == 0 {
		return current
	}
	builder := newCandidateSnapshotBuilder(len(current.records) + len(previous.failedRecords))
	failed := make(map[string]candidateFailureRecord, len(previous.failedRecords))
	for _, failure := range previous.failedRecords {
		key := previous.protocols[failure.protocolID] + "://" + failure.addr
		failed[key] = failure
	}
	for _, record := range current.records {
		key := current.protocols[record.protocolID] + "://" + record.addr
		failure, exists := failed[key]
		if !exists {
			builder.appendRecord(current, record, nil)
			continue
		}
		merged := record
		merged.checkedUnix = failure.checkedUnix
		alts := copyCredentialAlternates(current, record)
		alts = mergeCredentialAlternates(alts, merged, previous, failure.candidateRecord)
		merged.hasAuth = merged.hasAuth || failure.hasAuth || credentialAlternatesHaveAuth(alts)
		translated, _ := builder.translateRecord(current, merged, nil)
		currentSourceEnd := len(builder.snapshot.sourceRefs)
		for i := uint32(0); i < uint32(failure.sourceCount); i++ {
			ref := previous.sourceRefs[failure.sourceOffset+i]
			key, label := previous.sourceKeys[ref], previous.sources[ref]
			duplicate := false
			for offset := int(translated.sourceOffset); offset < currentSourceEnd; offset++ {
				if builder.snapshot.sourceKeys[builder.snapshot.sourceRefs[offset]] == key {
					duplicate = true
					break
				}
			}
			if !duplicate {
				id := builder.internSource(key, label)
				builder.snapshot.sourceRefs = append(builder.snapshot.sourceRefs, id)
				builder.snapshot.sourceTotals[id]++
				translated.sourceCount++
			}
		}
		// The retained failure attributions can sort before the current
		// declaration's sources (the rediscovered feed list dropped the
		// alphabetically earlier feeds), so the appended segment is not
		// inherently ordered. Re-sort it or the cache encoder rejects the
		// snapshot as "source references are not strictly sorted".
		sourceStart := int(translated.sourceOffset)
		segment := builder.snapshot.sourceRefs[sourceStart : sourceStart+int(translated.sourceCount)]
		sort.Slice(segment, func(i, j int) bool {
			return builder.snapshot.sourceKeys[segment[i]] < builder.snapshot.sourceKeys[segment[j]]
		})
		builder.snapshot.failedRecords = append(builder.snapshot.failedRecords, candidateFailureRecord{
			candidateRecord: translated, kind: failure.kind, lastError: failure.lastError,
		})
		builder.perFailedAlts = append(builder.perFailedAlts, alts)
		delete(failed, key)
	}
	for _, failure := range previous.failedRecords {
		key := previous.protocols[failure.protocolID] + "://" + failure.addr
		if _, retained := failed[key]; !retained {
			continue
		}
		translated, alts := builder.translateRecord(previous, failure.candidateRecord, nil)
		builder.snapshot.failedRecords = append(builder.snapshot.failedRecords, candidateFailureRecord{
			candidateRecord: translated, kind: failure.kind, lastError: failure.lastError,
		})
		builder.perFailedAlts = append(builder.perFailedAlts, alts)
	}
	// Rediscovered failures were appended in current-record order and retained
	// failures in previous-failure order, so the concatenation is not globally
	// sorted. Re-sort while keeping perFailedAlts aligned, or the cache
	// encoder rejects the snapshot as "failed records are not strictly
	// sorted" and finalizeCredentialAlternates would bind the wrong
	// credentials to each record.
	failures := builder.snapshot.failedRecords
	order := make([]int, len(failures))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		return compareCandidateRecords(builder.snapshot, failures[order[i]].candidateRecord, builder.snapshot, failures[order[j]].candidateRecord) < 0
	})
	sortedFailures := make([]candidateFailureRecord, len(failures))
	sortedAlts := make([][]ProxyCredential, len(failures))
	for newPos, oldPos := range order {
		sortedFailures[newPos] = failures[oldPos]
		sortedAlts[newPos] = builder.perFailedAlts[oldPos]
	}
	builder.snapshot.failedRecords = sortedFailures
	builder.perFailedAlts = sortedAlts
	builder.finalizeCredentialAlternates()
	result := builder.snapshot
	result.completedAt = previous.completedAt
	rebuildCandidateSourceFacets(result)
	return result
}

func buildCandidateSnapshot(candidates []Proxy, sourceLabels map[string]string) *candidateSnapshot {
	snapshot := &candidateSnapshot{
		records:    make([]candidateRecord, 0, len(candidates)),
		sourceRefs: make([]uint32, 0, len(candidates)),
		countries:  []string{""},
		cities:     []string{""},
	}
	sourceIDs := make(map[string]uint32)
	protocolIDs := make(map[string]uint16)
	countryIDs := map[string]uint32{"": 0}
	cityIDs := map[string]uint32{"": 0}
	perRecordAlts := make([][]ProxyCredential, 0, len(candidates))

	internSource := func(key, label string) uint32 {
		if key == "" {
			key = legacySourceKey(label)
		}
		if label == "" {
			label = "Unknown"
		}
		if id, ok := sourceIDs[key]; ok {
			return id
		}
		id := uint32(len(snapshot.sources))
		sourceIDs[key] = id
		snapshot.sourceKeys = append(snapshot.sourceKeys, key)
		snapshot.sources = append(snapshot.sources, label)
		snapshot.sourceTotals = append(snapshot.sourceTotals, 0)
		return id
	}
	internProtocol := func(value string) uint16 {
		value = strings.ToLower(strings.TrimSpace(value))
		if id, ok := protocolIDs[value]; ok {
			return id
		}
		id := uint16(len(snapshot.protocols))
		protocolIDs[value] = id
		snapshot.protocols = append(snapshot.protocols, value)
		snapshot.protocolTotals = append(snapshot.protocolTotals, 0)
		return id
	}
	internCountry := func(value string) uint32 {
		value = normalizedCandidateCountry(value)
		if id, ok := countryIDs[value]; ok {
			return id
		}
		id := uint32(len(snapshot.countries))
		countryIDs[value] = id
		snapshot.countries = append(snapshot.countries, value)
		return id
	}
	internCity := func(value string) uint32 {
		value = strings.TrimSpace(value)
		if id, ok := cityIDs[value]; ok {
			return id
		}
		id := uint32(len(snapshot.cities))
		cityIDs[value] = id
		snapshot.cities = append(snapshot.cities, value)
		return id
	}

	for _, px := range candidates {
		addr := px.Addr()
		protocolID := internProtocol(px.Protocol)
		hasAuth := px.Username != "" || px.Password != ""
		if !hasAuth {
			for _, credential := range px.CredentialAlternates {
				if credential.Username != "" || credential.Password != "" {
					hasAuth = true
					break
				}
			}
		}
		record := candidateRecord{
			addr:       addr,
			username:   px.Username,
			password:   px.Password,
			protocolID: protocolID,
			countryID:  internCountry(px.Country),
			cityID:     internCity(px.City),
			continent:  encodeContinent(px.Continent),
			status:     candidateDeferred,
			hasAuth:    hasAuth,
		}
		if px.Protocol == "proxyip" {
			record.status = candidateResource
		}

		record.sourceOffset = uint32(len(snapshot.sourceRefs))
		lastSource := "\x00"
		appendSourceValue := func(sourceValue string) {
			sourceValue = strings.TrimSpace(sourceValue)
			if sourceValue == lastSource {
				return
			}
			lastSource = sourceValue
			key, label := sourceValue, sourceLabels[sourceValue]
			if label == "" {
				key, label = legacySourceKey(sourceValue), sourceValue
			}
			sourceID := internSource(key, label)
			snapshot.sourceRefs = append(snapshot.sourceRefs, sourceID)
			snapshot.sourceTotals[sourceID]++
			record.sourceCount++
		}
		if len(px.SourceNames) == 0 {
			appendSourceValue(px.SourceName)
		} else {
			for _, sourceValue := range px.SourceNames {
				appendSourceValue(sourceValue)
			}
		}
		if record.sourceCount == 0 {
			sourceID := internSource(legacySourceKey(""), "")
			snapshot.sourceRefs = append(snapshot.sourceRefs, sourceID)
			snapshot.sourceTotals[sourceID]++
			record.sourceCount = 1
		}
		snapshot.protocolTotals[protocolID]++
		snapshot.records = append(snapshot.records, record)
		perRecordAlts = append(perRecordAlts, px.CredentialAlternates)
	}

	// Main's dedupe output is already Key-sorted, but keeping the catalog's
	// invariant here makes direct tests and future callers safe too.
	type candidateWithAlts struct {
		record candidateRecord
		alts   []ProxyCredential
	}
	items := make([]candidateWithAlts, len(snapshot.records))
	for i := range snapshot.records {
		items[i] = candidateWithAlts{record: snapshot.records[i], alts: perRecordAlts[i]}
	}
	sort.SliceStable(items, func(i, j int) bool {
		ap, bp := snapshot.protocols[items[i].record.protocolID], snapshot.protocols[items[j].record.protocolID]
		if ap != bp {
			return ap < bp
		}
		return items[i].record.addr < items[j].record.addr
	})
	var credentialAlternates []ProxyCredential
	for i := range items {
		alts := items[i].alts
		if len(alts) > maxAlternatesPerCandidate {
			log.Printf("[candidate-cache] truncating %d credential alternates for %s://%s to %d", len(alts), snapshot.protocols[items[i].record.protocolID], items[i].record.addr, maxAlternatesPerCandidate)
			alts = alts[:maxAlternatesPerCandidate]
		}
		items[i].record.credentialAlternateOffset = uint32(len(credentialAlternates))
		items[i].record.credentialAlternateCount = uint8(len(alts))
		credentialAlternates = append(credentialAlternates, alts...)
		snapshot.records[i] = items[i].record
	}
	snapshot.credentialAlternateTable = credentialAlternates
	rebuildCandidateSourceFacets(snapshot)
	return snapshot
}

func legacySourceKey(name string) string { return "legacy-name:" + strings.TrimSpace(name) }

func rebuildCandidateSourceFacets(snapshot *candidateSnapshot) {
	totals := make(map[string]int, len(snapshot.sources))
	displays := make(map[string]string, len(snapshot.sources))
	foldedSources := make([]string, len(snapshot.sources))
	foldedIDs := make([]uint32, len(snapshot.sources))
	foldedIndex := make(map[string]uint32, len(snapshot.sources))
	for i, display := range snapshot.sources {
		folded := strings.ToLower(display)
		foldedSources[i] = folded
		id, ok := foldedIndex[folded]
		if !ok {
			id = uint32(len(foldedIndex))
			foldedIndex[folded] = id
		}
		foldedIDs[i] = id
	}
	// A source display may occur more than once on one candidate (for example,
	// two separately configured feeds with the same name). Epoch markers keep
	// that per-record de-duplication linear without allocating a map per row.
	seenFolded := make([]uint32, len(foldedIndex))
	epoch := uint32(0)
	countRecord := func(record candidateRecord) {
		epoch++
		for i := uint32(0); i < uint32(record.sourceCount); i++ {
			ref := snapshot.sourceRefs[record.sourceOffset+i]
			display := snapshot.sources[ref]
			folded := foldedSources[ref]
			foldedID := foldedIDs[ref]
			if seenFolded[foldedID] == epoch {
				continue
			}
			seenFolded[foldedID] = epoch
			totals[folded]++
			if previous := displays[folded]; previous == "" || display < previous {
				displays[folded] = display
			}
		}
	}
	for _, record := range snapshot.records {
		countRecord(record)
	}
	for _, failure := range snapshot.failedRecords {
		countRecord(failure.candidateRecord)
	}
	keys := make([]string, 0, len(totals))
	for key := range totals {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if totals[keys[i]] != totals[keys[j]] {
			return totals[keys[i]] > totals[keys[j]]
		}
		return displays[keys[i]] < displays[keys[j]]
	})
	snapshot.sourceFacetValues = make([]string, 0, len(keys))
	snapshot.sourceFacetTotals = make([]int, 0, len(keys))
	for _, key := range keys {
		snapshot.sourceFacetValues = append(snapshot.sourceFacetValues, displays[key])
		snapshot.sourceFacetTotals = append(snapshot.sourceFacetTotals, totals[key])
	}
}

type candidateSnapshotBuilder struct {
	snapshot      *candidateSnapshot
	sourceIDs     map[string]uint32
	protocolIDs   map[string]uint16
	countryIDs    map[string]uint32
	cityIDs       map[string]uint32
	perRecordAlts [][]ProxyCredential
	perFailedAlts [][]ProxyCredential
}

func newCandidateSnapshotBuilder(capacity int) *candidateSnapshotBuilder {
	return &candidateSnapshotBuilder{
		snapshot: &candidateSnapshot{
			records:    make([]candidateRecord, 0, capacity),
			sourceRefs: make([]uint32, 0, capacity),
			countries:  []string{""}, cities: []string{""},
		},
		sourceIDs: make(map[string]uint32), protocolIDs: make(map[string]uint16),
		countryIDs: map[string]uint32{"": 0}, cityIDs: map[string]uint32{"": 0},
	}
}

func (b *candidateSnapshotBuilder) internSource(key, label string) uint32 {
	if key == "" {
		key = legacySourceKey(label)
	}
	if label == "" {
		label = "Unknown"
	}
	if id, ok := b.sourceIDs[key]; ok {
		return id
	}
	id := uint32(len(b.snapshot.sources))
	b.sourceIDs[key] = id
	b.snapshot.sourceKeys = append(b.snapshot.sourceKeys, key)
	b.snapshot.sources = append(b.snapshot.sources, label)
	b.snapshot.sourceTotals = append(b.snapshot.sourceTotals, 0)
	return id
}

func (b *candidateSnapshotBuilder) internProtocol(value string) uint16 {
	if id, ok := b.protocolIDs[value]; ok {
		return id
	}
	id := uint16(len(b.snapshot.protocols))
	b.protocolIDs[value] = id
	b.snapshot.protocols = append(b.snapshot.protocols, value)
	b.snapshot.protocolTotals = append(b.snapshot.protocolTotals, 0)
	return id
}

func (b *candidateSnapshotBuilder) internCountry(value string) uint32 {
	if id, ok := b.countryIDs[value]; ok {
		return id
	}
	id := uint32(len(b.snapshot.countries))
	b.countryIDs[value] = id
	b.snapshot.countries = append(b.snapshot.countries, value)
	return id
}

func (b *candidateSnapshotBuilder) internCity(value string) uint32 {
	if id, ok := b.cityIDs[value]; ok {
		return id
	}
	id := uint32(len(b.snapshot.cities))
	b.cityIDs[value] = id
	b.snapshot.cities = append(b.snapshot.cities, value)
	return id
}

func copyCredentialAlternates(source *candidateSnapshot, record candidateRecord) []ProxyCredential {
	if record.credentialAlternateCount == 0 {
		return nil
	}
	start := record.credentialAlternateOffset
	end := start + uint32(record.credentialAlternateCount)
	if end > uint32(len(source.credentialAlternateTable)) {
		return nil
	}
	return append([]ProxyCredential(nil), source.credentialAlternateTable[start:end]...)
}

func mergeCredentialAlternates(primary []ProxyCredential, primaryRecord candidateRecord, fallback *candidateSnapshot, fallbackRecord candidateRecord) []ProxyCredential {
	seen := make(map[ProxyCredential]bool, len(primary)+1+int(fallbackRecord.credentialAlternateCount))
	seen[ProxyCredential{Username: primaryRecord.username, Password: primaryRecord.password}] = true
	merged := make([]ProxyCredential, 0, min(maxAlternatesPerCandidate, len(primary)+1+int(fallbackRecord.credentialAlternateCount)))
	appendCredential := func(credential ProxyCredential) {
		if seen[credential] || len(merged) >= maxAlternatesPerCandidate {
			return
		}
		seen[credential] = true
		merged = append(merged, credential)
	}
	for _, credential := range primary {
		appendCredential(credential)
	}
	appendCredential(ProxyCredential{Username: fallbackRecord.username, Password: fallbackRecord.password})
	for _, credential := range copyCredentialAlternates(fallback, fallbackRecord) {
		appendCredential(credential)
	}
	return merged
}

func credentialAlternatesHaveAuth(alternates []ProxyCredential) bool {
	for _, credential := range alternates {
		if credential.Username != "" || credential.Password != "" {
			return true
		}
	}
	return false
}

func (b *candidateSnapshotBuilder) finalizeCredentialAlternates() {
	var table []ProxyCredential
	appendAlternates := func(record *candidateRecord, alts []ProxyCredential) {
		if len(alts) > maxAlternatesPerCandidate {
			alts = alts[:maxAlternatesPerCandidate]
		}
		record.credentialAlternateOffset = uint32(len(table))
		record.credentialAlternateCount = uint8(len(alts))
		table = append(table, alts...)
	}
	for i := range b.snapshot.records {
		appendAlternates(&b.snapshot.records[i], b.perRecordAlts[i])
	}
	for i := range b.snapshot.failedRecords {
		appendAlternates(&b.snapshot.failedRecords[i].candidateRecord, b.perFailedAlts[i])
	}
	b.snapshot.credentialAlternateTable = table
}

func (b *candidateSnapshotBuilder) translateRecord(source *candidateSnapshot, record candidateRecord, sourceNames []string) (candidateRecord, []ProxyCredential) {
	protocol := source.protocols[record.protocolID]
	country := source.countries[record.countryID]
	city := source.cities[record.cityID]
	alts := copyCredentialAlternates(source, record)
	originalSourceOffset, originalSourceCount := record.sourceOffset, record.sourceCount
	record.protocolID = b.internProtocol(protocol)
	record.countryID = b.internCountry(country)
	record.cityID = b.internCity(city)
	record.sourceOffset = uint32(len(b.snapshot.sourceRefs))
	record.sourceCount = 0
	appendSource := func(key, label string) {
		id := b.internSource(key, label)
		b.snapshot.sourceRefs = append(b.snapshot.sourceRefs, id)
		b.snapshot.sourceTotals[id]++
		record.sourceCount++
	}
	if sourceNames == nil {
		for i := uint32(0); i < uint32(originalSourceCount); i++ {
			ref := source.sourceRefs[originalSourceOffset+i]
			appendSource(source.sourceKeys[ref], source.sources[ref])
		}
	} else {
		for _, name := range sourceNames {
			appendSource(legacySourceKey(name), name)
		}
	}
	if record.sourceCount == 0 {
		id := b.internSource(legacySourceKey(""), "")
		b.snapshot.sourceRefs = append(b.snapshot.sourceRefs, id)
		b.snapshot.sourceTotals[id]++
		record.sourceCount = 1
	}
	b.snapshot.protocolTotals[record.protocolID]++
	return record, alts
}

func (b *candidateSnapshotBuilder) appendRecord(source *candidateSnapshot, record candidateRecord, sourceNames []string) {
	record, alts := b.translateRecord(source, record, sourceNames)
	b.snapshot.records = append(b.snapshot.records, record)
	b.perRecordAlts = append(b.perRecordAlts, alts)
}

func (b *candidateSnapshotBuilder) appendFailure(source *candidateSnapshot, failure candidateFailureRecord) {
	record, alts := b.translateRecord(source, failure.candidateRecord, nil)
	b.snapshot.failedRecords = append(b.snapshot.failedRecords, candidateFailureRecord{
		candidateRecord: record,
		kind:            failure.kind,
		lastError:       failure.lastError,
	})
	b.perFailedAlts = append(b.perFailedAlts, alts)
}

func recordHasSourceIn(snapshot *candidateSnapshot, record candidateRecord, allowed map[string]bool) bool {
	for i := uint32(0); i < uint32(record.sourceCount); i++ {
		if allowed[snapshot.sourceKeys[snapshot.sourceRefs[record.sourceOffset+i]]] {
			return true
		}
	}
	return false
}

// appendFilteredRecord carries an old-only record into a partial snapshot only
// when at least one of its source attributions failed this cycle. Attribution
// from successful or disabled sources is deliberately removed.
func (b *candidateSnapshotBuilder) appendFilteredRecord(source *candidateSnapshot, record candidateRecord, allowed map[string]bool) bool {
	if !recordHasSourceIn(source, record, allowed) {
		return false
	}
	alts := copyCredentialAlternates(source, record)
	originalOffset, originalCount := record.sourceOffset, record.sourceCount
	protocol := source.protocols[record.protocolID]
	country := source.countries[record.countryID]
	city := source.cities[record.cityID]
	record.protocolID = b.internProtocol(protocol)
	record.countryID = b.internCountry(country)
	record.cityID = b.internCity(city)
	record.sourceOffset = uint32(len(b.snapshot.sourceRefs))
	record.sourceCount = 0
	for i := uint32(0); i < uint32(originalCount); i++ {
		ref := source.sourceRefs[originalOffset+i]
		key, label := source.sourceKeys[ref], source.sources[ref]
		if !allowed[key] {
			continue
		}
		id := b.internSource(key, label)
		b.snapshot.sourceRefs = append(b.snapshot.sourceRefs, id)
		b.snapshot.sourceTotals[id]++
		record.sourceCount++
	}
	b.snapshot.protocolTotals[record.protocolID]++
	b.snapshot.records = append(b.snapshot.records, record)
	b.perRecordAlts = append(b.perRecordAlts, alts)
	return true
}

func (b *candidateSnapshotBuilder) appendMergedRecord(metadata *candidateSnapshot, record candidateRecord, aSnapshot *candidateSnapshot, a candidateRecord, retainedFromA map[string]bool, bSnapshot *candidateSnapshot, other candidateRecord) {
	alts := copyCredentialAlternates(bSnapshot, other)
	if recordHasSourceIn(aSnapshot, a, retainedFromA) {
		alts = mergeCredentialAlternates(alts, record, aSnapshot, a)
		record.hasAuth = record.hasAuth || a.hasAuth
	}
	record.hasAuth = record.hasAuth || record.username != "" || record.password != "" || credentialAlternatesHaveAuth(alts)
	protocol := metadata.protocols[record.protocolID]
	country := metadata.countries[record.countryID]
	city := metadata.cities[record.cityID]
	record.protocolID = b.internProtocol(protocol)
	record.countryID = b.internCountry(country)
	record.cityID = b.internCity(city)
	record.sourceOffset = uint32(len(b.snapshot.sourceRefs))
	record.sourceCount = 0
	appendSource := func(key, label string) {
		id := b.internSource(key, label)
		b.snapshot.sourceRefs = append(b.snapshot.sourceRefs, id)
		b.snapshot.sourceTotals[id]++
		record.sourceCount++
	}
	// Both source lists are sorted. Merge current attributions with only the
	// failed-source subset of the old attributions, without allocating a
	// []string for each of hundreds of thousands rows.
	ai, bi := uint32(0), uint32(0)
	for ai < uint32(a.sourceCount) || bi < uint32(other.sourceCount) {
		for ai < uint32(a.sourceCount) {
			ref := aSnapshot.sourceRefs[a.sourceOffset+ai]
			if retainedFromA[aSnapshot.sourceKeys[ref]] {
				break
			}
			ai++
		}
		if ai >= uint32(a.sourceCount) && bi >= uint32(other.sourceCount) {
			break
		}
		var key, label string
		if ai >= uint32(a.sourceCount) {
			ref := bSnapshot.sourceRefs[other.sourceOffset+bi]
			key, label = bSnapshot.sourceKeys[ref], bSnapshot.sources[ref]
			bi++
		} else if bi >= uint32(other.sourceCount) {
			ref := aSnapshot.sourceRefs[a.sourceOffset+ai]
			key, label = aSnapshot.sourceKeys[ref], aSnapshot.sources[ref]
			ai++
		} else {
			ar := aSnapshot.sourceRefs[a.sourceOffset+ai]
			br := bSnapshot.sourceRefs[other.sourceOffset+bi]
			ak, bk := aSnapshot.sourceKeys[ar], bSnapshot.sourceKeys[br]
			if ak < bk {
				key, label, ai = ak, aSnapshot.sources[ar], ai+1
			} else if bk < ak {
				key, label, bi = bk, bSnapshot.sources[br], bi+1
			} else {
				key, label, ai, bi = ak, bSnapshot.sources[br], ai+1, bi+1
			}
		}
		if record.sourceCount == 0 || b.snapshot.sourceKeys[b.snapshot.sourceRefs[len(b.snapshot.sourceRefs)-1]] != key {
			appendSource(key, label)
		}
	}
	b.snapshot.protocolTotals[record.protocolID]++
	b.snapshot.records = append(b.snapshot.records, record)
	b.perRecordAlts = append(b.perRecordAlts, alts)
}

func compareCandidateRecords(aSnapshot *candidateSnapshot, a candidateRecord, bSnapshot *candidateSnapshot, b candidateRecord) int {
	ap, bp := aSnapshot.protocols[a.protocolID], bSnapshot.protocols[b.protocolID]
	if ap < bp {
		return -1
	}
	if ap > bp {
		return 1
	}
	if a.addr < b.addr {
		return -1
	}
	if a.addr > b.addr {
		return 1
	}
	return 0
}

// mergeCandidateSnapshots replaces every successful source's attribution with
// its current inventory while retaining only attribution belonging to failed
// sources. Records left with no source disappear; current records are always
// admitted. This is a source-granular partial refresh, not a whole-catalog
// append-only union.
func mergeCandidateSnapshots(previous, current *candidateSnapshot, failedSources map[string]bool) *candidateSnapshot {
	builder := newCandidateSnapshotBuilder(candidateMergedSize(previous, current, failedSources))
	i, j := 0, 0
	for i < len(previous.records) || j < len(current.records) {
		if i >= len(previous.records) {
			record := current.records[j]
			builder.appendRecord(current, record, nil)
			j++
			continue
		}
		if j >= len(current.records) {
			record := previous.records[i]
			builder.appendFilteredRecord(previous, record, failedSources)
			i++
			continue
		}
		oldRecord, newRecord := previous.records[i], current.records[j]
		switch compareCandidateRecords(previous, oldRecord, current, newRecord) {
		case -1:
			builder.appendFilteredRecord(previous, oldRecord, failedSources)
			i++
		case 1:
			builder.appendRecord(current, newRecord, nil)
			j++
		default:
			// The address was seen this cycle, so carry its new last-seen time
			// and source attribution. Preserve a prior check outcome until this
			// cycle's bounded checker reaches it. If a failed old source remains
			// attributed and the current declarations lack credentials, retain the
			// old connection-ready credential pair with its authentication flag.
			merged := newRecord
			merged.status = oldRecord.status
			merged.checkedUnix = oldRecord.checkedUnix
			oldAttributionRetained := recordHasSourceIn(previous, oldRecord, failedSources)
			if oldAttributionRetained && merged.username == "" && oldRecord.username != "" {
				merged.username = oldRecord.username
				merged.password = oldRecord.password
			}
			merged.hasAuth = merged.username != "" || merged.password != ""
			if oldAttributionRetained && newRecord.countryID == 0 && oldRecord.countryID != 0 {
				merged.countryID = oldRecord.countryID
				merged.continent = oldRecord.continent
			}
			if oldAttributionRetained && newRecord.cityID == 0 && oldRecord.cityID != 0 {
				merged.cityID = oldRecord.cityID
			}
			// appendRecord needs the snapshot that owns the metadata IDs. If
			// either fallback above selected an old dictionary ID, translate
			// it through a shallow metadata carrier first.
			metadata := candidateSnapshot{
				protocols: current.protocols,
				countries: current.countries,
				cities:    current.cities,
			}
			if merged.countryID == oldRecord.countryID && newRecord.countryID == 0 && oldRecord.countryID != 0 {
				metadata.countries = previous.countries
			}
			if merged.cityID == oldRecord.cityID && newRecord.cityID == 0 && oldRecord.cityID != 0 {
				metadata.cities = previous.cities
			}
			builder.appendMergedRecord(&metadata, merged, previous, oldRecord, failedSources, current, newRecord)
			i++
			j++
		}
	}
	merged := builder.snapshot
	builder.finalizeCredentialAlternates()
	merged.seenAt = current.seenAt
	merged.refreshAttempt = current.refreshAttempt
	merged.completedAt = previous.completedAt
	rebuildCandidateSourceFacets(merged)
	return merged
}

func candidateMergedSize(a, b *candidateSnapshot, failedSources map[string]bool) int {
	i, j, total := 0, 0, 0
	for i < len(a.records) && j < len(b.records) {
		switch compareCandidateRecords(a, a.records[i], b, b.records[j]) {
		case -1:
			if recordHasSourceIn(a, a.records[i], failedSources) {
				total++
			}
			i++
		case 1:
			total++
			j++
		default:
			total++
			i++
			j++
		}
	}
	for ; i < len(a.records); i++ {
		if recordHasSourceIn(a, a.records[i], failedSources) {
			total++
		}
	}
	return total + len(b.records) - j
}

// complete applies one bounded source-check cycle. Failed checks move out of
// pending inventory into the separate failure collection; successful checks
// remain represented only by ProxyPool ownership at read and lease time.
func (c *CandidateCatalog) complete(refresh candidateRefresh, checked, alive []Proxy, policyFiltered map[string]bool) {
	current := c.snapshot.Load()
	if current == nil {
		return
	}
	current.mu.Lock()
	// Removal can replace the snapshot while completion was waiting for its
	// write lock. Never mutate or persist a detached old snapshot.
	if c.snapshot.Load() != current || current.generation != refresh.generation {
		current.mu.Unlock()
		return
	}
	checkedAt := time.Now().Unix()
	aliveKeys := make(map[string]bool, len(alive))
	for _, px := range alive {
		aliveKeys[px.Key()] = true
	}
	failureKinds := make(map[string]CandidateFailureKind, len(checked))
	for _, px := range checked {
		if px.Protocol == "proxyip" || aliveKeys[px.Key()] {
			continue
		}
		kind := candidateFailureUnreachable
		if policyFiltered[px.Key()] {
			kind = candidateFailurePolicyFiltered
		}
		failureKinds[px.Key()] = kind
	}
	if len(failureKinds) > 0 {
		pending := current.records[:0]
		for _, record := range current.records {
			key := current.protocols[record.protocolID] + "://" + record.addr
			kind, failed := failureKinds[key]
			if !failed {
				pending = append(pending, record)
				continue
			}
			record.status = candidateDeferred
			record.checkedUnix = checkedAt
			current.failedRecords = append(current.failedRecords, candidateFailureRecord{candidateRecord: record, kind: kind})
		}
		current.records = pending
		sort.Slice(current.failedRecords, func(i, j int) bool {
			return compareCandidateRecords(current, current.failedRecords[i].candidateRecord, current, current.failedRecords[j].candidateRecord) < 0
		})
	}
	current.completedAt = time.Unix(checkedAt, 0)
	if current.sourceErrors > 0 {
		current.phase = "partial"
	} else {
		current.phase = "complete"
	}
	current.revision++
	current.mu.Unlock()

	// Disk compression may take noticeable time for a 500k-row inventory. Keep
	// it outside the snapshot write lock so API readers never wait on filesystem
	// IO; the cache takes its own RLock while encoding one consistent image.
	_ = c.persistCompletedSnapshot(current)
}

func (s *candidateSnapshot) find(protocol, addr string) int {
	idx := sort.Search(len(s.records), func(i int) bool {
		record := s.records[i]
		recordProtocol := s.protocols[record.protocolID]
		return recordProtocol > protocol || recordProtocol == protocol && record.addr >= addr
	})
	if idx < len(s.records) {
		record := s.records[idx]
		if s.protocols[record.protocolID] == protocol && record.addr == addr {
			return idx
		}
	}
	return -1
}

func (s *candidateSnapshot) findFailed(protocol, addr string) int {
	idx := sort.Search(len(s.failedRecords), func(i int) bool {
		record := s.failedRecords[i]
		recordProtocol := s.protocols[record.protocolID]
		return recordProtocol > protocol || recordProtocol == protocol && record.addr >= addr
	})
	if idx < len(s.failedRecords) {
		record := s.failedRecords[idx]
		if s.protocols[record.protocolID] == protocol && record.addr == addr {
			return idx
		}
	}
	return -1
}

func normalizedCandidateCountry(value string) string {
	value = strings.TrimSpace(value)
	if len(value) != 2 {
		return ""
	}
	for i := 0; i < len(value); i++ {
		if value[i] >= 'a' && value[i] <= 'z' {
			continue
		}
		if value[i] < 'A' || value[i] > 'Z' {
			return ""
		}
	}
	return strings.ToUpper(value)
}

func encodeContinent(value string) uint8 {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "AS":
		return 1
	case "NA":
		return 2
	case "EU":
		return 3
	case "AF":
		return 4
	case "SA":
		return 5
	case "OC":
		return 6
	case "AN":
		return 7
	default:
		return 0
	}
}

func decodeContinent(value uint8) string {
	switch value {
	case 1:
		return "AS"
	case 2:
		return "NA"
	case 3:
		return "EU"
	case 4:
		return "AF"
	case 5:
		return "SA"
	case 6:
		return "OC"
	case 7:
		return "AN"
	default:
		return ""
	}
}

type CandidateView struct {
	Key             string   `json:"key"`
	Addr            string   `json:"addr"`
	Protocol        string   `json:"protocol"`
	ProxyURL        string   `json:"proxy_url"`
	Username        string   `json:"username"`
	Password        string   `json:"password"`
	Source          string   `json:"source"`
	SourceNames     []string `json:"source_names"`
	Country         string   `json:"country"`
	City            string   `json:"city"`
	Continent       string   `json:"continent"`
	SourceCountry   string   `json:"source_country"`
	SourceCity      string   `json:"source_city"`
	SourceContinent string   `json:"source_continent"`
	Status          string   `json:"status"`
	Known           bool     `json:"known"`
	Available       bool     `json:"available"`
	Routable        bool     `json:"routable"`
	HasAuth         bool     `json:"has_auth"`
	LastSeen        string   `json:"last_seen,omitempty"`
	LastChecked     string   `json:"last_checked,omitempty"`
}

type CandidateFacet struct {
	Value string `json:"value"`
	Total int    `json:"total"`
}

type CandidateCountryFacet struct {
	Country   string `json:"country"`
	Continent string `json:"continent,omitempty"`
	Total     int    `json:"total"`
}

type CandidatePageResponse struct {
	Candidates          []CandidateView         `json:"candidates"`
	SnapshotID          string                  `json:"snapshot_id"`
	Page                int                     `json:"page"`
	PageSize            int                     `json:"page_size"`
	PageCount           int                     `json:"page_count"`
	HasNext             bool                    `json:"has_next"`
	FilteredTotal       int                     `json:"filtered_total"`
	CandidateTotal      int                     `json:"candidate_total"`
	Phase               string                  `json:"phase"`
	UpdatedAt           string                  `json:"updated_at,omitempty"`
	RefreshAttemptedAt  string                  `json:"refresh_attempted_at,omitempty"`
	SourceErrors        int                     `json:"source_errors"`
	Sources             []CandidateFacet        `json:"sources"`
	Protocols           []CandidateFacet        `json:"protocols"`
	Countries           []CandidateCountryFacet `json:"countries"`
	CountryUnknownTotal int                     `json:"country_unknown_total"`
}

type FailedCandidateView struct {
	CandidateView
	FailureType string `json:"failure_type"`
	LastError   string `json:"last_error"`
}

type FailedCandidatePageResponse struct {
	FailedCandidates []FailedCandidateView `json:"failed_candidates"`
	SnapshotID       string                `json:"snapshot_id"`
	Page             int                   `json:"page"`
	PageSize         int                   `json:"page_size"`
	PageCount        int                   `json:"page_count"`
	HasNext          bool                  `json:"has_next"`
	FilteredTotal    int                   `json:"filtered_total"`
	FailedTotal      int                   `json:"failed_total"`
	Sources          []CandidateFacet      `json:"sources"`
	Protocols        []CandidateFacet      `json:"protocols"`
	FailureTypes     []CandidateFacet      `json:"failure_types"`
}

type ProxyIPPageResponse struct {
	ProxyIPs            []CandidateView         `json:"proxyips"`
	SnapshotID          string                  `json:"snapshot_id"`
	Page                int                     `json:"page"`
	PageSize            int                     `json:"page_size"`
	PageCount           int                     `json:"page_count"`
	HasNext             bool                    `json:"has_next"`
	FilteredTotal       int                     `json:"filtered_total"`
	ProxyIPTotal        int                     `json:"proxyip_total"`
	Sources             []CandidateFacet        `json:"sources"`
	Countries           []CandidateCountryFacet `json:"countries"`
	CountryUnknownTotal int                     `json:"country_unknown_total"`
}

const (
	defaultCandidatePageSize = 50
	maxCandidatePageSize     = 100
)

func (s *StatusServer) handleCandidatesPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	if err := validateCountryQuery(r); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_country", err)
		return
	}
	page := s.buildCandidatePage(r)
	w.Header().Set("X-Snapshot-ID", page.SnapshotID)
	if requested := strings.TrimSpace(r.URL.Query().Get("snapshot_id")); requested != "" && requested != page.SnapshotID {
		writeErrCode(w, http.StatusConflict, "snapshot_changed", fmt.Errorf("requested snapshot %q is no longer current", requested))
		return
	}
	writeJSON(w, page)
}

func (s *StatusServer) buildCandidatePage(r *http.Request) CandidatePageResponse {
	snapshot := s.pool.candidates.snapshot.Load()
	if snapshot == nil {
		_, overlayHash := s.pool.candidateKnownSnapshot()
		return CandidatePageResponse{
			Candidates: []CandidateView{}, SnapshotID: formatCandidateSnapshotID(0, 0, overlayHash), Page: 1, PageSize: defaultCandidatePageSize,
			PageCount: 1,
			Phase:     "loading", Sources: []CandidateFacet{},
			Protocols: []CandidateFacet{}, Countries: []CandidateCountryFacet{},
		}
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	page, pageSize := candidatePageParams(r)
	filter := newCandidateFilter(r, snapshot)
	known, overlayHash := s.pool.candidateKnownSnapshot()
	snapshotID := formatCandidateSnapshotID(snapshot.generation, snapshot.revision, overlayHash)

	countryCounts := make(map[uint32]int)
	countryContinents := make(map[uint32]string)
	unknownCountryTotal := 0
	filteredTotal := 0
	candidateTotal := 0
	sourceCounts := make(map[string]int)
	protocolCounts := make(map[string]int)
	start := (page - 1) * pageSize
	pageRows := make([]CandidateView, 0, pageSize)
	for i := range snapshot.records {
		record := snapshot.records[i]
		if !candidatePendingRecord(snapshot, record, known) {
			continue
		}
		candidateTotal++
		protocolCounts[snapshot.protocols[record.protocolID]]++
		countCandidateRecordSources(snapshot, record, sourceCounts)
		if !filter.matchesBase(snapshot, record) {
			continue
		}
		if record.countryID == 0 {
			unknownCountryTotal++
		} else {
			countryCounts[record.countryID]++
			if countryContinents[record.countryID] == "" {
				countryContinents[record.countryID] = decodeContinent(record.continent)
			}
		}
		if !filter.matchesCountry(record) {
			continue
		}
		if filteredTotal >= start && len(pageRows) < pageSize {
			pageRows = append(pageRows, snapshot.view(record, candidateDeferred))
		}
		filteredTotal++
	}

	pageCount := (filteredTotal + pageSize - 1) / pageSize
	if pageCount < 1 {
		pageCount = 1
	}
	if page > pageCount {
		page = pageCount
		start = (page - 1) * pageSize
		pageRows = pageRows[:0]
		matched := 0
		for i := range snapshot.records {
			record := snapshot.records[i]
			if !candidatePendingRecord(snapshot, record, known) || !filter.matchesBase(snapshot, record) || !filter.matchesCountry(record) {
				continue
			}
			if matched >= start && len(pageRows) < pageSize {
				pageRows = append(pageRows, snapshot.view(record, candidateDeferred))
			}
			matched++
			if len(pageRows) == pageSize {
				break
			}
		}
	}

	return CandidatePageResponse{
		Candidates: pageRows, SnapshotID: snapshotID, Page: page, PageSize: pageSize,
		PageCount: pageCount, HasNext: page < pageCount,
		FilteredTotal: filteredTotal, CandidateTotal: candidateTotal,
		Phase: snapshot.phase, UpdatedAt: formatCandidateTime(snapshot.seenAt),
		RefreshAttemptedAt: formatCandidateTime(snapshot.refreshAttempt), SourceErrors: snapshot.sourceErrors,
		Sources: candidateMapFacets(sourceCounts), Protocols: candidateMapFacets(protocolCounts),
		Countries: snapshot.countryFacets(countryCounts, countryContinents), CountryUnknownTotal: unknownCountryTotal,
	}
}

func (s *StatusServer) handleFailedCandidatesPage(w http.ResponseWriter, r *http.Request) {
	page := s.buildFailedCandidatePage(r)
	w.Header().Set("X-Snapshot-ID", page.SnapshotID)
	if requested := strings.TrimSpace(r.URL.Query().Get("snapshot_id")); requested != "" && requested != page.SnapshotID {
		writeErrCode(w, http.StatusConflict, "snapshot_changed", fmt.Errorf("requested snapshot %q is no longer current", requested))
		return
	}
	writeJSON(w, page)
}

func (s *StatusServer) buildFailedCandidatePage(r *http.Request) FailedCandidatePageResponse {
	page, pageSize := candidatePageParams(r)
	snapshot := s.pool.candidates.snapshot.Load()
	if snapshot == nil {
		return FailedCandidatePageResponse{
			FailedCandidates: []FailedCandidateView{}, SnapshotID: formatCandidateSnapshotID(0, 0, 0),
			Page: 1, PageSize: pageSize, PageCount: 1,
			Sources: []CandidateFacet{}, Protocols: []CandidateFacet{}, FailureTypes: []CandidateFacet{},
		}
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	filter := newCandidateFilter(r, snapshot)
	failureType := strings.TrimSpace(r.URL.Query().Get("failure_type"))
	snapshotID := formatCandidateSnapshotID(snapshot.generation, snapshot.revision, 0)
	filteredTotal := 0
	failedTotal := len(snapshot.failedRecords)
	sourceCounts := make(map[string]int)
	protocolCounts := make(map[string]int)
	failureTypeCounts := make(map[string]int)
	start := (page - 1) * pageSize
	rows := make([]FailedCandidateView, 0, pageSize)
	for _, failure := range snapshot.failedRecords {
		kind := failure.kind.String()
		protocolCounts[snapshot.protocols[failure.protocolID]]++
		failureTypeCounts[kind]++
		countCandidateRecordSources(snapshot, failure.candidateRecord, sourceCounts)
		if !filter.matchesBase(snapshot, failure.candidateRecord) || failureType != "" && !strings.EqualFold(failureType, kind) {
			continue
		}
		if filteredTotal >= start && len(rows) < pageSize {
			rows = append(rows, FailedCandidateView{
				CandidateView: snapshot.view(failure.candidateRecord, candidateDeferred),
				FailureType:   kind, LastError: failure.lastError,
			})
		}
		filteredTotal++
	}
	pageCount := pageCountForTotal(filteredTotal, pageSize)
	if page > pageCount {
		page = pageCount
		start = (page - 1) * pageSize
		rows = rows[:0]
		matched := 0
		for _, failure := range snapshot.failedRecords {
			kind := failure.kind.String()
			if !filter.matchesBase(snapshot, failure.candidateRecord) || failureType != "" && !strings.EqualFold(failureType, kind) {
				continue
			}
			if matched >= start && len(rows) < pageSize {
				rows = append(rows, FailedCandidateView{
					CandidateView: snapshot.view(failure.candidateRecord, candidateDeferred),
					FailureType:   kind, LastError: failure.lastError,
				})
			}
			matched++
			if len(rows) == pageSize {
				break
			}
		}
	}
	return FailedCandidatePageResponse{
		FailedCandidates: rows, SnapshotID: snapshotID, Page: page, PageSize: pageSize,
		PageCount: pageCount, HasNext: page < pageCount, FilteredTotal: filteredTotal, FailedTotal: failedTotal,
		Sources: candidateMapFacets(sourceCounts), Protocols: candidateMapFacets(protocolCounts),
		FailureTypes: candidateMapFacets(failureTypeCounts),
	}
}

func (s *StatusServer) handleProxyIPPage(w http.ResponseWriter, r *http.Request) {
	if err := validateCountryQuery(r); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_country", err)
		return
	}
	page := s.buildProxyIPPage(r)
	w.Header().Set("X-Snapshot-ID", page.SnapshotID)
	if requested := strings.TrimSpace(r.URL.Query().Get("snapshot_id")); requested != "" && requested != page.SnapshotID {
		writeErrCode(w, http.StatusConflict, "snapshot_changed", fmt.Errorf("requested snapshot %q is no longer current", requested))
		return
	}
	writeJSON(w, page)
}

func (s *StatusServer) buildProxyIPPage(r *http.Request) ProxyIPPageResponse {
	page, pageSize := candidatePageParams(r)
	snapshot := s.pool.candidates.snapshot.Load()
	if snapshot == nil {
		return ProxyIPPageResponse{
			ProxyIPs: []CandidateView{}, SnapshotID: formatCandidateSnapshotID(0, 0, 0),
			Page: 1, PageSize: pageSize, PageCount: 1,
			Sources: []CandidateFacet{}, Countries: []CandidateCountryFacet{},
		}
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	filter := newCandidateFilter(r, snapshot)
	snapshotID := formatCandidateSnapshotID(snapshot.generation, snapshot.revision, 0)
	filteredTotal := 0
	proxyIPTotal := 0
	unknownCountryTotal := 0
	sourceCounts := make(map[string]int)
	countryCounts := make(map[uint32]int)
	countryContinents := make(map[uint32]string)
	start := (page - 1) * pageSize
	rows := make([]CandidateView, 0, pageSize)
	for _, record := range snapshot.records {
		if snapshot.protocols[record.protocolID] != "proxyip" {
			continue
		}
		proxyIPTotal++
		countCandidateRecordSources(snapshot, record, sourceCounts)
		if !filter.matchesBase(snapshot, record) {
			continue
		}
		if record.countryID == 0 {
			unknownCountryTotal++
		} else {
			countryCounts[record.countryID]++
			countryContinents[record.countryID] = decodeContinent(record.continent)
		}
		if !filter.matchesCountry(record) {
			continue
		}
		if filteredTotal >= start && len(rows) < pageSize {
			rows = append(rows, snapshot.view(record, candidateResource))
		}
		filteredTotal++
	}
	pageCount := pageCountForTotal(filteredTotal, pageSize)
	if page > pageCount {
		page = pageCount
		start = (page - 1) * pageSize
		rows = rows[:0]
		matched := 0
		for _, record := range snapshot.records {
			if snapshot.protocols[record.protocolID] != "proxyip" || !filter.matchesBase(snapshot, record) || !filter.matchesCountry(record) {
				continue
			}
			if matched >= start && len(rows) < pageSize {
				rows = append(rows, snapshot.view(record, candidateResource))
			}
			matched++
			if len(rows) == pageSize {
				break
			}
		}
	}
	return ProxyIPPageResponse{
		ProxyIPs: rows, SnapshotID: snapshotID, Page: page, PageSize: pageSize,
		PageCount: pageCount, HasNext: page < pageCount, FilteredTotal: filteredTotal, ProxyIPTotal: proxyIPTotal,
		Sources: candidateMapFacets(sourceCounts), Countries: snapshot.countryFacets(countryCounts, countryContinents),
		CountryUnknownTotal: unknownCountryTotal,
	}
}

func candidatePendingRecord(snapshot *candidateSnapshot, record candidateRecord, known candidateKnownIndex) bool {
	if record.status != candidateDeferred || snapshot.protocols[record.protocolID] == "proxyip" {
		return false
	}
	_, exists := knownCandidateStatus(known, snapshot.protocols[record.protocolID], record.addr)
	return !exists
}

func candidateRecordStatus(snapshot *candidateSnapshot, record candidateRecord, known candidateKnownIndex) CandidateStatus {
	protocol := snapshot.protocols[record.protocolID]
	if protocol == "proxyip" {
		return candidateResource
	}
	// Policy exclusions and definitive check failures are stronger than stale
	// pool membership. Pool cleanup can happen independently, but the catalog
	// must never relabel a candidate that just failed require-ip-change or a
	// reachability check as healthy merely because it was known from an earlier
	// cycle.
	if record.status == candidatePolicyFiltered || record.status == candidateCheckedFailed {
		return record.status
	}
	if status, ok := knownCandidateStatus(known, protocol, record.addr); ok {
		return status
	}
	return record.status
}

func (s *candidateSnapshot) view(record candidateRecord, status CandidateStatus) CandidateView {
	protocol := s.protocols[record.protocolID]
	sources := make([]string, 0, record.sourceCount)
	for i := uint32(0); i < uint32(record.sourceCount); i++ {
		display := s.sources[s.sourceRefs[record.sourceOffset+i]]
		duplicate := false
		for _, existing := range sources {
			if strings.EqualFold(existing, display) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			sources = append(sources, display)
		}
	}
	sort.Strings(sources)
	country := "Unknown"
	if record.countryID != 0 {
		country = s.countries[record.countryID]
	}
	city := s.cities[record.cityID]
	continent := decodeContinent(record.continent)
	host, port, _ := net.SplitHostPort(record.addr)
	proxyURL := (Proxy{IP: host, Port: port, Protocol: protocol, Username: record.username, Password: record.password}).ConsumerURL()
	view := CandidateView{
		Key: protocol + "://" + record.addr, Addr: record.addr, Protocol: protocol, ProxyURL: proxyURL,
		Username: record.username, Password: record.password,
		SourceNames: sources, Country: country, City: city, Continent: continent,
		SourceCountry: country, SourceCity: city, SourceContinent: continent,
		Status: status.String(), Known: status == candidateKnownAvailable || status == candidateKnownUnavailable,
		Available: status == candidateKnownAvailable, Routable: protocol != "proxyip", HasAuth: record.hasAuth,
		LastSeen: formatCandidateTime(s.seenAt),
	}
	if len(sources) > 0 {
		view.Source = sources[0]
	}
	if record.seenUnix > 0 {
		view.LastSeen = time.Unix(record.seenUnix, 0).UTC().Format(time.RFC3339)
	}
	if record.checkedUnix > 0 {
		view.LastChecked = time.Unix(record.checkedUnix, 0).UTC().Format(time.RFC3339)
	}
	return view
}

func formatCandidateTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

type candidateFilter struct {
	search         string
	protocolID     int
	source         string
	countryID      int
	unknownCountry bool
}

func newCandidateFilter(r *http.Request, snapshot *candidateSnapshot) candidateFilter {
	query := r.URL.Query()
	filter := candidateFilter{search: strings.TrimSpace(query.Get("search")), protocolID: -1, countryID: -1}
	if value := strings.TrimSpace(query.Get("protocol")); value != "" {
		filter.protocolID = findFold(snapshot.protocols, value)
	}
	filter.source = strings.TrimSpace(query.Get("source"))
	filter.unknownCountry = nodeQueryEnabled(query.Get("country_unknown")) || strings.EqualFold(strings.TrimSpace(query.Get("country")), "__unknown__")
	if !filter.unknownCountry {
		if raw := strings.TrimSpace(query.Get("country")); raw != "" {
			if value := normalizedCandidateCountry(raw); value != "" {
				filter.countryID = findFold(snapshot.countries, value)
			} else {
				filter.countryID = -2
			}
		}
	}
	return filter
}

func (f candidateFilter) matchesBase(snapshot *candidateSnapshot, record candidateRecord) bool {
	if f.protocolID >= 0 && int(record.protocolID) != f.protocolID {
		return false
	}
	if f.protocolID == -2 {
		return false
	}
	if f.source != "" {
		found := false
		for i := uint32(0); i < uint32(record.sourceCount); i++ {
			if strings.EqualFold(snapshot.sources[snapshot.sourceRefs[record.sourceOffset+i]], f.source) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return f.search == "" || snapshot.recordContains(record, f.search)
}

func (f candidateFilter) matchesCountry(record candidateRecord) bool {
	if f.unknownCountry {
		return record.countryID == 0
	}
	if f.countryID >= 0 {
		return int(record.countryID) == f.countryID
	}
	if f.countryID == -2 {
		return false
	}
	return true
}

func (s *candidateSnapshot) recordContains(record candidateRecord, query string) bool {
	protocol := s.protocols[record.protocolID]
	if candidateContainsFold(record.addr, query) || candidateContainsFold(protocol, query) {
		return true
	}
	if schemeEnd := strings.Index(query, "://"); schemeEnd >= 0 &&
		strings.EqualFold(protocol, query[:schemeEnd]) && candidateContainsFold(record.addr, query[schemeEnd+3:]) {
		return true
	}
	if record.countryID != 0 && candidateContainsFold(s.countries[record.countryID], query) {
		return true
	}
	if record.cityID != 0 && candidateContainsFold(s.cities[record.cityID], query) {
		return true
	}
	for i := uint32(0); i < uint32(record.sourceCount); i++ {
		if candidateContainsFold(s.sources[s.sourceRefs[record.sourceOffset+i]], query) {
			return true
		}
	}
	return false
}

// containsFold is allocation-free for the overwhelmingly ASCII inventory.
func candidateContainsFold(value, query string) bool {
	if query == "" || strings.Contains(value, query) {
		return true
	}
	if len(query) > len(value) {
		return false
	}
	for i := 0; i+len(query) <= len(value); i++ {
		if strings.EqualFold(value[i:i+len(query)], query) {
			return true
		}
	}
	return false
}

func findFold(values []string, query string) int {
	for i, value := range values {
		if strings.EqualFold(value, query) {
			return i
		}
	}
	return -2 // explicitly requested but absent
}

func candidatePageParams(r *http.Request) (page, pageSize int) {
	page, pageSize = 1, defaultCandidatePageSize
	if parsed, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && parsed > 0 {
		page = parsed
	}
	if parsed, err := strconv.Atoi(r.URL.Query().Get("page_size")); err == nil && parsed > 0 {
		pageSize = parsed
	}
	if pageSize > maxCandidatePageSize {
		pageSize = maxCandidatePageSize
	}
	return page, pageSize
}

func countCandidateRecordSources(snapshot *candidateSnapshot, record candidateRecord, counts map[string]int) {
	seen := make(map[string]bool, record.sourceCount)
	for i := uint32(0); i < uint32(record.sourceCount); i++ {
		value := snapshot.sources[snapshot.sourceRefs[record.sourceOffset+i]]
		folded := strings.ToLower(value)
		if seen[folded] {
			continue
		}
		seen[folded] = true
		counts[value]++
	}
}

func candidateMapFacets(counts map[string]int) []CandidateFacet {
	out := make([]CandidateFacet, 0, len(counts))
	for value, total := range counts {
		out = append(out, CandidateFacet{Value: value, Total: total})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Value < out[j].Value
	})
	return out
}

func candidateDictionaryFacets(values []string, totals []int) []CandidateFacet {
	out := make([]CandidateFacet, 0, len(values))
	for i, value := range values {
		out = append(out, CandidateFacet{Value: value, Total: totals[i]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Value < out[j].Value
	})
	return out
}

func (s *candidateSnapshot) countryFacets(counts map[uint32]int, continents map[uint32]string) []CandidateCountryFacet {
	out := make([]CandidateCountryFacet, 0, len(counts))
	for id, total := range counts {
		out = append(out, CandidateCountryFacet{Country: s.countries[id], Continent: continents[id], Total: total})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Continent != out[j].Continent {
			return out[i].Continent < out[j].Continent
		}
		return out[i].Country < out[j].Country
	})
	return out
}

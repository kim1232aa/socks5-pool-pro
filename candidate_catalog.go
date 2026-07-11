package main

import (
	"fmt"
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

// candidateRecord is intentionally compact. At roughly 56 bytes on amd64 plus the
// address string, 500k records remain comfortably below the memory cost of
// retaining 500k full Proxy values (which contain many strings and a slice).
// Repeated source/protocol/country/city values are interned in snapshot-level
// dictionaries, while multi-source attribution uses one flat uint32 array.
type candidateRecord struct {
	addr         string
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
}

type candidateSnapshot struct {
	mu sync.RWMutex

	records           []candidateRecord
	sourceRefs        []uint32
	sourceKeys        []string // stable Source.ID (or a legacy synthetic key)
	sources           []string // display names parallel to sourceKeys
	protocols         []string
	countries         []string // index 0 is always unknown
	cities            []string // index 0 is always empty
	sourceTotals      []int
	sourceFacetValues []string
	sourceFacetTotals []int
	protocolTotals    []int

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
	cacheMu        sync.RWMutex
	cache          *candidateCatalogCache
	persistMu      sync.Mutex
}

func (c *CandidateCatalog) protocolTotal(protocol string) (int, bool) {
	snapshot := c.snapshot.Load()
	if snapshot == nil {
		return 0, false
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	for i, value := range snapshot.protocols {
		if strings.EqualFold(value, protocol) {
			return snapshot.protocolTotals[i], true
		}
	}
	return 0, true
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
	snapshot := buildCandidateSnapshot(candidates, sourceLabels)
	for i := range snapshot.records {
		snapshot.records[i].seenUnix = now.Unix()
	}
	// A partial scrape is merged with the previous inventory by source:
	// attribution from failed feeds remains visible, while successful feeds
	// are replaced by exactly what they advertised this cycle. This preserves
	// failures without turning removed entries from healthy feeds immortal.
	if previous := c.snapshot.Load(); previous != nil && len(previous.records) > 0 {
		previous.mu.RLock()
		if sourceErrors > 0 {
			snapshot = mergeCandidateSnapshots(previous, snapshot, failedSources)
		} else {
			carryCandidateHistory(previous, snapshot)
		}
		previous.mu.RUnlock()
	}
	snapshot.generation = refresh.generation
	snapshot.revision = 1
	snapshot.phase = "checking"
	snapshot.sourceErrors = sourceErrors
	snapshot.seenAt = now
	snapshot.refreshAttempt = now
	c.snapshot.Store(snapshot)
	return refresh
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
		record := candidateRecord{
			addr:       addr,
			protocolID: protocolID,
			countryID:  internCountry(px.Country),
			cityID:     internCity(px.City),
			continent:  encodeContinent(px.Continent),
			status:     candidateDeferred,
			hasAuth:    px.Username != "" || px.Password != "",
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
	}

	// Main's dedupe output is already Key-sorted, but keeping the catalog's
	// invariant here makes direct tests and future callers safe too.
	sort.SliceStable(snapshot.records, func(i, j int) bool {
		a, b := snapshot.records[i], snapshot.records[j]
		ap, bp := snapshot.protocols[a.protocolID], snapshot.protocols[b.protocolID]
		if ap != bp {
			return ap < bp
		}
		return a.addr < b.addr
	})
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
	for recordIndex, record := range snapshot.records {
		epoch := uint32(recordIndex + 1)
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
	snapshot    *candidateSnapshot
	sourceIDs   map[string]uint32
	protocolIDs map[string]uint16
	countryIDs  map[string]uint32
	cityIDs     map[string]uint32
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

func (b *candidateSnapshotBuilder) appendRecord(source *candidateSnapshot, record candidateRecord, sourceNames []string) {
	protocol := source.protocols[record.protocolID]
	country := source.countries[record.countryID]
	city := source.cities[record.cityID]
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
	b.snapshot.records = append(b.snapshot.records, record)
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
	return true
}

func (b *candidateSnapshotBuilder) appendMergedRecord(metadata *candidateSnapshot, record candidateRecord, aSnapshot *candidateSnapshot, a candidateRecord, retainedFromA map[string]bool, bSnapshot *candidateSnapshot, other candidateRecord) {
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
			// The address was seen this cycle, so carry its new last-seen time,
			// authentication flag and source attribution. Preserve a prior
			// check outcome until this cycle's bounded checker reaches it.
			merged := newRecord
			merged.status = oldRecord.status
			merged.checkedUnix = oldRecord.checkedUnix
			oldAttributionRetained := recordHasSourceIn(previous, oldRecord, failedSources)
			// Authentication cannot be attributed to one source in the compact
			// record. Be conservative only while a failed old attribution is
			// retained; otherwise the current successful-source declaration wins.
			merged.hasAuth = newRecord.hasAuth || oldAttributionRetained && oldRecord.hasAuth
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

// complete applies the bounded check results to a detached copy and atomically
// publishes it. Source-declared country/city are intentionally not replaced by
// exit-geo observations: catalog country facets describe source inventory,
// while /api/nodes/page describes verified routable exits.
func (c *CandidateCatalog) complete(refresh candidateRefresh, checked, alive []Proxy, policyFiltered map[string]bool) {
	current := c.snapshot.Load()
	if current == nil {
		return
	}
	current.mu.Lock()
	if current.generation != refresh.generation {
		current.mu.Unlock()
		return
	}
	checkedAt := time.Now().Unix()
	aliveKeys := make(map[string]bool, len(alive))
	for _, px := range alive {
		aliveKeys[px.Key()] = true
	}
	for _, px := range checked {
		if px.Protocol == "proxyip" {
			continue
		}
		idx := current.find(px.Protocol, px.Addr())
		if idx < 0 {
			continue
		}
		record := &current.records[idx]
		record.checkedUnix = checkedAt
		if policyFiltered[px.Key()] {
			record.status = candidatePolicyFiltered
		} else if aliveKeys[px.Key()] {
			// Membership/availability is overlaid from ProxyPool at read time.
			// Keeping only the discovery outcome here means an explicit later
			// pool removal does not leave a stale "known" label behind.
			record.status = candidateDeferred
		} else {
			record.status = candidateCheckedFailed
		}
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
	c.persistCompletedSnapshot(current)
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

type CandidateStatusFacet struct {
	Status string `json:"status"`
	Total  int    `json:"total"`
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
	Statuses            []CandidateStatusFacet  `json:"statuses"`
	Sources             []CandidateFacet        `json:"sources"`
	Protocols           []CandidateFacet        `json:"protocols"`
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
			Phase:     "loading", Statuses: candidateStatusFacets(nil), Sources: []CandidateFacet{},
			Protocols: []CandidateFacet{}, Countries: []CandidateCountryFacet{},
		}
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	page, pageSize := candidatePageParams(r)
	filter := newCandidateFilter(r, snapshot)
	known, overlayHash := s.pool.candidateKnownSnapshot()
	snapshotID := formatCandidateSnapshotID(snapshot.generation, snapshot.revision, overlayHash)

	statusCounts := make(map[CandidateStatus]int, 6)
	countryCounts := make(map[uint32]int)
	countryContinents := make(map[uint32]string)
	unknownCountryTotal := 0
	filteredTotal := 0
	start := (page - 1) * pageSize
	pageRows := make([]CandidateView, 0, pageSize)
	for i := range snapshot.records {
		record := snapshot.records[i]
		status := candidateRecordStatus(snapshot, record, known)
		statusCounts[status]++
		if !filter.matchesNonCountry(snapshot, record, status) {
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
			pageRows = append(pageRows, snapshot.view(record, status))
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
			status := candidateRecordStatus(snapshot, record, known)
			if !filter.matchesNonCountry(snapshot, record, status) || !filter.matchesCountry(record) {
				continue
			}
			if matched >= start && len(pageRows) < pageSize {
				pageRows = append(pageRows, snapshot.view(record, status))
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
		FilteredTotal: filteredTotal, CandidateTotal: len(snapshot.records),
		Phase: snapshot.phase, UpdatedAt: formatCandidateTime(snapshot.seenAt),
		RefreshAttemptedAt: formatCandidateTime(snapshot.refreshAttempt), SourceErrors: snapshot.sourceErrors,
		Statuses:  candidateStatusFacets(statusCounts),
		Sources:   candidateDictionaryFacets(snapshot.sourceFacetValues, snapshot.sourceFacetTotals),
		Protocols: candidateDictionaryFacets(snapshot.protocols, snapshot.protocolTotals),
		Countries: snapshot.countryFacets(countryCounts, countryContinents), CountryUnknownTotal: unknownCountryTotal,
	}
}

func candidateRecordStatus(snapshot *candidateSnapshot, record candidateRecord, known candidateKnownIndex) CandidateStatus {
	protocol := snapshot.protocols[record.protocolID]
	if protocol == "proxyip" {
		return candidateResource
	}
	// A policy exclusion is stronger than stale pool membership. Pool cleanup
	// can happen independently, but the catalog must never relabel a candidate
	// that just failed require-ip-change as healthy merely because it was known
	// from an earlier cycle.
	if record.status == candidatePolicyFiltered {
		return candidatePolicyFiltered
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
	view := CandidateView{
		Key: protocol + "://" + record.addr, Addr: record.addr, Protocol: protocol,
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
	status         CandidateStatus
	statusSet      bool
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
	if value := strings.TrimSpace(query.Get("status")); value != "" {
		filter.status, filter.statusSet = parseCandidateStatus(value)
		if !filter.statusSet {
			filter.status = 255
			filter.statusSet = true
		}
	}
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

func parseCandidateStatus(value string) (CandidateStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "deferred":
		return candidateDeferred, true
	case "checked_failed", "failed":
		return candidateCheckedFailed, true
	case "policy_filtered", "policy_excluded", "excluded":
		return candidatePolicyFiltered, true
	case "known_available", "available":
		return candidateKnownAvailable, true
	case "known_unavailable", "unavailable":
		return candidateKnownUnavailable, true
	case "resource", "non_routable":
		return candidateResource, true
	default:
		return candidateDeferred, false
	}
}

func (f candidateFilter) matchesNonCountry(snapshot *candidateSnapshot, record candidateRecord, status CandidateStatus) bool {
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
	if f.statusSet && status != f.status {
		return false
	}
	if f.search != "" && !snapshot.recordContains(record, f.search) {
		return false
	}
	return true
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

func candidateStatusFacets(counts map[CandidateStatus]int) []CandidateStatusFacet {
	statuses := []CandidateStatus{candidateDeferred, candidateCheckedFailed, candidatePolicyFiltered, candidateKnownAvailable, candidateKnownUnavailable, candidateResource}
	out := make([]CandidateStatusFacet, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, CandidateStatusFacet{Status: status.String(), Total: counts[status]})
	}
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

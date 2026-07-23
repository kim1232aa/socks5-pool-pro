package main

import (
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	candidateCacheFilename                  = "candidate_catalog.v1.bin.gz"
	candidateCacheMagic                     = "SPCAND01"
	candidateCacheVersion            uint16 = 2
	maxCandidateCacheCompressedBytes        = 96 << 20
	maxCandidateCacheDecodedBytes           = 256 << 20
	maxCandidateCacheStringBytes            = 96 << 20
	maxCandidateCacheStringLength           = 16 << 10
	maxCandidateCacheRecords                = 1_200_000
	maxCandidateCacheSourceRefs             = 4_800_000
	maxCandidateCacheSources                = 16_384
	maxCandidateCacheProtocols              = 64
	maxCandidateCacheCountries              = 1_024
	maxCandidateCacheCities                 = 600_000
)

type candidateCatalogCache struct {
	path            string
	savedGeneration uint64 // guarded by CandidateCatalog.persistMu
	savedRevision   uint64 // guarded by CandidateCatalog.persistMu
}

func newCandidateCatalogCache(dataDir string) *candidateCatalogCache {
	return &candidateCatalogCache{path: filepath.Join(dataDir, candidateCacheFilename)}
}

func (c *CandidateCatalog) SetDiskCache(cache *candidateCatalogCache) {
	c.cacheMu.Lock()
	c.cache = cache
	c.cacheMu.Unlock()
}

// LoadDiskCache publishes a fully validated snapshot before the first network
// refresh. A caller treats an error as a soft startup warning; no partial state
// is ever installed.
func (c *CandidateCatalog) LoadDiskCache() (bool, error) {
	c.cacheMu.RLock()
	cache := c.cache
	c.cacheMu.RUnlock()
	if cache == nil {
		return false, nil
	}
	snapshot, err := cache.load()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Snapshot IDs already include a per-process boot nonce, so a persisted
	// generation has no cross-process identity value. Start each process from a
	// small, known generation instead of trusting a cache value close to uint64
	// wraparound, which could otherwise make later saves look older forever.
	snapshot.generation = 1
	c.nextGeneration.Store(snapshot.generation)
	c.snapshot.Store(snapshot)
	c.persistMu.Lock()
	cache.savedGeneration = snapshot.generation
	cache.savedRevision = snapshot.revision
	c.persistMu.Unlock()
	return true, nil
}

func (c *CandidateCatalog) persistCompletedSnapshot(snapshot *candidateSnapshot) error {
	c.cacheMu.RLock()
	cache := c.cache
	c.cacheMu.RUnlock()
	if cache == nil || snapshot == nil {
		return nil
	}

	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	snapshot.mu.RLock()
	generation, revision := snapshot.generation, snapshot.revision
	snapshot.mu.RUnlock()
	if generation < cache.savedGeneration || generation == cache.savedGeneration && revision <= cache.savedRevision {
		return nil
	}
	if err := cache.save(snapshot); err != nil {
		log.Printf("[candidate-cache] save failed: %v", err)
		return err
	}
	cache.savedGeneration, cache.savedRevision = generation, revision
	return nil
}

func (cache *candidateCatalogCache) load() (*candidateSnapshot, error) {
	info, err := os.Lstat(cache.path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("candidate cache is not a regular file")
	}
	if info.Size() < 1 || info.Size() > maxCandidateCacheCompressedBytes {
		return nil, fmt.Errorf("candidate cache compressed size %d exceeds limit %d", info.Size(), maxCandidateCacheCompressedBytes)
	}

	file, err := os.Open(cache.path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened candidate cache: %w", err)
	}
	// Re-check the opened descriptor. The data directory may be mounted from
	// the host, so do not trust a path-only Lstat that could be swapped for a
	// symlink or a different/oversized file between inspection and Open.
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("candidate cache changed while opening or is not a regular file")
	}
	if openedInfo.Size() < 1 || openedInfo.Size() > maxCandidateCacheCompressedBytes {
		return nil, fmt.Errorf("opened candidate cache compressed size %d exceeds limit %d", openedInfo.Size(), maxCandidateCacheCompressedBytes)
	}
	gz, err := gzip.NewReader(io.LimitReader(file, maxCandidateCacheCompressedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("open candidate cache gzip: %w", err)
	}
	limited := &io.LimitedReader{R: gz, N: maxCandidateCacheDecodedBytes + 1}
	decoder := candidateCacheDecoder{reader: limited}
	snapshot, decodeErr := decoder.decode()
	if decodeErr == nil {
		var trailing [1]byte
		n, trailingErr := limited.Read(trailing[:])
		if n != 0 || trailingErr == nil {
			decodeErr = fmt.Errorf("candidate cache has trailing or over-limit data")
		} else if !errors.Is(trailingErr, io.EOF) {
			decodeErr = trailingErr
		}
	}
	closeErr := gz.Close()
	if decodeErr != nil {
		return nil, fmt.Errorf("decode candidate cache: %w", decodeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close candidate cache gzip: %w", closeErr)
	}
	if limited.N <= 0 {
		return nil, fmt.Errorf("candidate cache decoded size exceeds limit %d", maxCandidateCacheDecodedBytes)
	}
	if err := validateAndRebuildCandidateSnapshot(snapshot); err != nil {
		return nil, fmt.Errorf("validate candidate cache: %w", err)
	}
	return snapshot, nil
}

func (cache *candidateCatalogCache) save(snapshot *candidateSnapshot) (returnErr error) {
	if snapshot == nil {
		return fmt.Errorf("nil candidate snapshot")
	}
	if err := os.MkdirAll(filepath.Dir(cache.path), 0o700); err != nil {
		return fmt.Errorf("create candidate cache directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(cache.path), ".candidate-cache-*.tmp")
	if err != nil {
		return fmt.Errorf("create candidate cache temp file: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		if returnErr != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod candidate cache temp file: %w", err)
	}

	compressed := &boundedCacheWriter{writer: temp, remaining: maxCandidateCacheCompressedBytes}
	gz, err := gzip.NewWriterLevel(compressed, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("create candidate cache gzip: %w", err)
	}
	gz.Header.ModTime = time.Time{}
	gz.Header.OS = 255
	uncompressed := &boundedCacheWriter{writer: gz, remaining: maxCandidateCacheDecodedBytes}
	encoder := candidateCacheEncoder{writer: uncompressed}

	snapshot.mu.RLock()
	encodeErr := validateCandidateSnapshot(snapshot)
	if encodeErr == nil {
		encodeErr = encoder.encode(snapshot)
	}
	snapshot.mu.RUnlock()
	if encodeErr != nil {
		_ = gz.Close()
		return fmt.Errorf("encode candidate cache: %w", encodeErr)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("close candidate cache gzip: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync candidate cache temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close candidate cache temp file: %w", err)
	}
	if err := os.Rename(tempPath, cache.path); err != nil {
		return fmt.Errorf("replace candidate cache: %w", err)
	}
	if err := syncCandidateCacheDirectory(filepath.Dir(cache.path)); err != nil {
		return fmt.Errorf("sync candidate cache directory: %w", err)
	}
	return nil
}

func syncCandidateCacheDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

type boundedCacheWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *boundedCacheWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("candidate cache exceeds byte limit")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

type candidateCacheEncoder struct {
	writer      io.Writer
	stringBytes uint64
}

func (e *candidateCacheEncoder) encode(snapshot *candidateSnapshot) error {
	if _, err := io.WriteString(e.writer, candidateCacheMagic); err != nil {
		return err
	}
	if err := e.uint16(candidateCacheVersion); err != nil {
		return err
	}
	if err := e.uint16(0); err != nil { // reserved flags
		return err
	}
	if err := e.uint64(snapshot.generation); err != nil {
		return err
	}
	if err := e.uint64(snapshot.revision); err != nil {
		return err
	}
	if err := e.string(snapshot.phase, 32); err != nil {
		return err
	}
	if err := e.uint32(uint32(snapshot.sourceErrors)); err != nil {
		return err
	}
	for _, value := range []time.Time{snapshot.seenAt, snapshot.refreshAttempt, snapshot.completedAt} {
		encoded := int64(0)
		if !value.IsZero() {
			encoded = value.UnixNano()
		}
		if err := e.int64(encoded); err != nil {
			return err
		}
	}
	for _, values := range [][]string{snapshot.sourceKeys, snapshot.sources, snapshot.protocols, snapshot.countries, snapshot.cities} {
		if err := e.strings(values); err != nil {
			return err
		}
	}
	if err := e.uint32(uint32(len(snapshot.sourceRefs))); err != nil {
		return err
	}
	for _, value := range snapshot.sourceRefs {
		if err := e.uint32(value); err != nil {
			return err
		}
	}
	if err := e.uint32(uint32(len(snapshot.records))); err != nil {
		return err
	}
	for _, record := range snapshot.records {
		if err := e.string(record.addr, maxCandidateCacheStringLength); err != nil {
			return err
		}
		if err := e.string(record.username, maxCandidateCacheStringLength); err != nil {
			return err
		}
		if err := e.string(record.password, maxCandidateCacheStringLength); err != nil {
			return err
		}
		for _, value := range []uint32{record.sourceOffset, record.countryID, record.cityID} {
			if err := e.uint32(value); err != nil {
				return err
			}
		}
		if err := e.uint16(record.protocolID); err != nil {
			return err
		}
		if err := e.uint16(record.sourceCount); err != nil {
			return err
		}
		for _, value := range []byte{record.continent, byte(record.status), boolByte(record.hasAuth)} {
			if err := e.uint8(value); err != nil {
				return err
			}
		}
		if err := e.int64(record.seenUnix); err != nil {
			return err
		}
		if err := e.int64(record.checkedUnix); err != nil {
			return err
		}
	}
	return nil
}

func boolByte(value bool) byte {
	if value {
		return 1
	}
	return 0
}

func (e *candidateCacheEncoder) strings(values []string) error {
	if len(values) > int(^uint32(0)) {
		return fmt.Errorf("too many candidate cache strings")
	}
	if err := e.uint32(uint32(len(values))); err != nil {
		return err
	}
	for _, value := range values {
		if err := e.string(value, maxCandidateCacheStringLength); err != nil {
			return err
		}
	}
	return nil
}

func (e *candidateCacheEncoder) string(value string, perValueLimit int) error {
	if len(value) > perValueLimit {
		return fmt.Errorf("candidate cache string length %d exceeds limit %d", len(value), perValueLimit)
	}
	e.stringBytes += uint64(len(value))
	if e.stringBytes > maxCandidateCacheStringBytes {
		return fmt.Errorf("candidate cache strings exceed byte limit")
	}
	if err := e.uint32(uint32(len(value))); err != nil {
		return err
	}
	_, err := io.WriteString(e.writer, value)
	return err
}

func (e *candidateCacheEncoder) uint8(value uint8) error {
	_, err := e.writer.Write([]byte{value})
	return err
}

func (e *candidateCacheEncoder) uint16(value uint16) error {
	var buffer [2]byte
	binary.LittleEndian.PutUint16(buffer[:], value)
	_, err := e.writer.Write(buffer[:])
	return err
}

func (e *candidateCacheEncoder) uint32(value uint32) error {
	var buffer [4]byte
	binary.LittleEndian.PutUint32(buffer[:], value)
	_, err := e.writer.Write(buffer[:])
	return err
}

func (e *candidateCacheEncoder) uint64(value uint64) error {
	var buffer [8]byte
	binary.LittleEndian.PutUint64(buffer[:], value)
	_, err := e.writer.Write(buffer[:])
	return err
}

func (e *candidateCacheEncoder) int64(value int64) error { return e.uint64(uint64(value)) }

type candidateCacheDecoder struct {
	reader      io.Reader
	stringBytes uint64
}

func (d *candidateCacheDecoder) decode() (*candidateSnapshot, error) {
	magic := make([]byte, len(candidateCacheMagic))
	if _, err := io.ReadFull(d.reader, magic); err != nil {
		return nil, err
	}
	if string(magic) != candidateCacheMagic {
		return nil, fmt.Errorf("candidate cache magic mismatch")
	}
	version, err := d.uint16()
	if err != nil {
		return nil, err
	}
	if version != candidateCacheVersion {
		return nil, fmt.Errorf("unsupported candidate cache version %d", version)
	}
	flags, err := d.uint16()
	if err != nil {
		return nil, err
	}
	if flags != 0 {
		return nil, fmt.Errorf("unsupported candidate cache flags %d", flags)
	}
	snapshot := &candidateSnapshot{}
	if snapshot.generation, err = d.uint64(); err != nil {
		return nil, err
	}
	if snapshot.revision, err = d.uint64(); err != nil {
		return nil, err
	}
	if snapshot.phase, err = d.string(32); err != nil {
		return nil, err
	}
	sourceErrors, err := d.uint32()
	if err != nil {
		return nil, err
	}
	snapshot.sourceErrors = int(sourceErrors)
	times := []*time.Time{&snapshot.seenAt, &snapshot.refreshAttempt, &snapshot.completedAt}
	for _, destination := range times {
		value, readErr := d.int64()
		if readErr != nil {
			return nil, readErr
		}
		if value != 0 {
			*destination = time.Unix(0, value)
		}
	}
	limits := []int{maxCandidateCacheSources, maxCandidateCacheSources, maxCandidateCacheProtocols, maxCandidateCacheCountries, maxCandidateCacheCities}
	destinations := []*[]string{&snapshot.sourceKeys, &snapshot.sources, &snapshot.protocols, &snapshot.countries, &snapshot.cities}
	for i := range destinations {
		values, readErr := d.strings(limits[i])
		if readErr != nil {
			return nil, readErr
		}
		*destinations[i] = values
	}
	refCount, err := d.count(maxCandidateCacheSourceRefs, "source references")
	if err != nil {
		return nil, err
	}
	snapshot.sourceRefs = make([]uint32, refCount)
	for i := range snapshot.sourceRefs {
		if snapshot.sourceRefs[i], err = d.uint32(); err != nil {
			return nil, err
		}
	}
	recordCount, err := d.count(maxCandidateCacheRecords, "records")
	if err != nil {
		return nil, err
	}
	snapshot.records = make([]candidateRecord, recordCount)
	for i := range snapshot.records {
		record := &snapshot.records[i]
		if record.addr, err = d.string(maxCandidateCacheStringLength); err != nil {
			return nil, err
		}
		if record.username, err = d.string(maxCandidateCacheStringLength); err != nil {
			return nil, err
		}
		if record.password, err = d.string(maxCandidateCacheStringLength); err != nil {
			return nil, err
		}
		for _, destination := range []*uint32{&record.sourceOffset, &record.countryID, &record.cityID} {
			if *destination, err = d.uint32(); err != nil {
				return nil, err
			}
		}
		if record.protocolID, err = d.uint16(); err != nil {
			return nil, err
		}
		if record.sourceCount, err = d.uint16(); err != nil {
			return nil, err
		}
		continent, readErr := d.uint8()
		if readErr != nil {
			return nil, readErr
		}
		record.continent = continent
		status, readErr := d.uint8()
		if readErr != nil {
			return nil, readErr
		}
		record.status = CandidateStatus(status)
		hasAuth, readErr := d.uint8()
		if readErr != nil {
			return nil, readErr
		}
		if hasAuth > 1 {
			return nil, fmt.Errorf("invalid has_auth value %d", hasAuth)
		}
		record.hasAuth = hasAuth == 1
		if record.seenUnix, err = d.int64(); err != nil {
			return nil, err
		}
		if record.checkedUnix, err = d.int64(); err != nil {
			return nil, err
		}
	}
	return snapshot, nil
}

func (d *candidateCacheDecoder) strings(maxCount int) ([]string, error) {
	count, err := d.count(maxCount, "strings")
	if err != nil {
		return nil, err
	}
	values := make([]string, count)
	for i := range values {
		if values[i], err = d.string(maxCandidateCacheStringLength); err != nil {
			return nil, err
		}
	}
	return values, nil
}

func (d *candidateCacheDecoder) string(maxLength int) (string, error) {
	length, err := d.count(maxLength, "string bytes")
	if err != nil {
		return "", err
	}
	d.stringBytes += uint64(length)
	if d.stringBytes > maxCandidateCacheStringBytes {
		return "", fmt.Errorf("candidate cache strings exceed byte limit")
	}
	buffer := make([]byte, length)
	if _, err := io.ReadFull(d.reader, buffer); err != nil {
		return "", err
	}
	return string(buffer), nil
}

func (d *candidateCacheDecoder) count(max int, label string) (int, error) {
	value, err := d.uint32()
	if err != nil {
		return 0, err
	}
	if uint64(value) > uint64(max) {
		return 0, fmt.Errorf("candidate cache %s count %d exceeds limit %d", label, value, max)
	}
	return int(value), nil
}

func (d *candidateCacheDecoder) uint8() (uint8, error) {
	var buffer [1]byte
	_, err := io.ReadFull(d.reader, buffer[:])
	return buffer[0], err
}

func (d *candidateCacheDecoder) uint16() (uint16, error) {
	var buffer [2]byte
	_, err := io.ReadFull(d.reader, buffer[:])
	return binary.LittleEndian.Uint16(buffer[:]), err
}

func (d *candidateCacheDecoder) uint32() (uint32, error) {
	var buffer [4]byte
	_, err := io.ReadFull(d.reader, buffer[:])
	return binary.LittleEndian.Uint32(buffer[:]), err
}

func (d *candidateCacheDecoder) uint64() (uint64, error) {
	var buffer [8]byte
	_, err := io.ReadFull(d.reader, buffer[:])
	return binary.LittleEndian.Uint64(buffer[:]), err
}

func (d *candidateCacheDecoder) int64() (int64, error) {
	value, err := d.uint64()
	return int64(value), err
}

func validateAndRebuildCandidateSnapshot(snapshot *candidateSnapshot) error {
	if err := validateCandidateSnapshot(snapshot); err != nil {
		return err
	}
	removed := filterNonPublicCandidateRecords(snapshot)
	if removed > 0 {
		log.Printf("[candidate-cache] skipped %d non-public or invalid ProxyIP endpoint(s)", removed)
	}
	snapshot.sourceTotals = make([]int, len(snapshot.sources))
	snapshot.protocolTotals = make([]int, len(snapshot.protocols))
	for _, record := range snapshot.records {
		snapshot.protocolTotals[record.protocolID]++
		for i := uint32(0); i < uint32(record.sourceCount); i++ {
			snapshot.sourceTotals[snapshot.sourceRefs[record.sourceOffset+i]]++
		}
	}
	rebuildCandidateSourceFacets(snapshot)
	return nil
}

// filterNonPublicCandidateRecords is an upgrade boundary for catalogs written
// by versions that admitted private and special-use literals. Keep valid
// records from the same snapshot (including hostname upstreams) instead of
// rejecting the entire last-good catalog because a large feed contained a few
// bad rows. The compact source-reference array may retain harmless unused
// entries until the next successful network snapshot; all visible facets are
// rebuilt from retained records only.
func filterNonPublicCandidateRecords(snapshot *candidateSnapshot) int {
	retained := snapshot.records[:0]
	for _, record := range snapshot.records {
		host, port, _ := net.SplitHostPort(record.addr)
		ip := net.ParseIP(host)
		protocol := snapshot.protocols[record.protocolID]
		if (ip != nil && !isPublicInternetIP(ip)) || (protocol == "proxyip" && (ip == nil || port != "443")) {
			continue
		}
		retained = append(retained, record)
	}
	removed := len(snapshot.records) - len(retained)
	snapshot.records = retained
	return removed
}

func validateCandidateSnapshot(snapshot *candidateSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("nil snapshot")
	}
	if snapshot.generation == 0 || snapshot.generation == ^uint64(0) || snapshot.revision == 0 {
		return fmt.Errorf("invalid generation/revision %d/%d", snapshot.generation, snapshot.revision)
	}
	if snapshot.phase != "complete" && snapshot.phase != "partial" {
		return fmt.Errorf("cache phase %q is not complete or partial", snapshot.phase)
	}
	if snapshot.sourceErrors < 0 || snapshot.sourceErrors > maxCandidateCacheSources {
		return fmt.Errorf("invalid source error count %d", snapshot.sourceErrors)
	}
	if len(snapshot.records) > maxCandidateCacheRecords || len(snapshot.sourceRefs) > maxCandidateCacheSourceRefs {
		return fmt.Errorf("candidate snapshot exceeds record/reference limits")
	}
	if len(snapshot.sourceKeys) != len(snapshot.sources) || len(snapshot.sources) > maxCandidateCacheSources {
		return fmt.Errorf("candidate source dictionaries do not align")
	}
	if len(snapshot.protocols) > maxCandidateCacheProtocols || len(snapshot.records) > 0 && len(snapshot.protocols) == 0 {
		return fmt.Errorf("invalid protocol dictionary size %d", len(snapshot.protocols))
	}
	if len(snapshot.countries) == 0 || len(snapshot.countries) > maxCandidateCacheCountries || snapshot.countries[0] != "" {
		return fmt.Errorf("invalid country dictionary")
	}
	if len(snapshot.cities) == 0 || len(snapshot.cities) > maxCandidateCacheCities || snapshot.cities[0] != "" {
		return fmt.Errorf("invalid city dictionary")
	}
	for i, key := range snapshot.sourceKeys {
		if strings.TrimSpace(key) == "" || len(key) > maxCandidateCacheStringLength || len(snapshot.sources[i]) > maxCandidateCacheStringLength {
			return fmt.Errorf("invalid source dictionary entry %d", i)
		}
	}
	for i, protocol := range snapshot.protocols {
		switch protocol {
		case "socks5", "http", "https", "proxyip":
		default:
			return fmt.Errorf("invalid protocol dictionary entry %d: %q", i, protocol)
		}
	}
	for _, ref := range snapshot.sourceRefs {
		if uint64(ref) >= uint64(len(snapshot.sources)) {
			return fmt.Errorf("source reference %d is out of range", ref)
		}
	}
	var previous *candidateRecord
	for i := range snapshot.records {
		record := &snapshot.records[i]
		if len(record.addr) == 0 || len(record.addr) > maxCandidateCacheStringLength {
			return fmt.Errorf("invalid address length at record %d", i)
		}
		if len(record.username) > maxCandidateCacheStringLength || len(record.password) > maxCandidateCacheStringLength {
			return fmt.Errorf("invalid credential length at record %d", i)
		}
		if record.hasAuth != (record.username != "" || record.password != "") {
			return fmt.Errorf("authentication flag does not match credentials at record %d", i)
		}
		host, port, err := net.SplitHostPort(record.addr)
		if err != nil || host == "" {
			return fmt.Errorf("invalid address at record %d", i)
		}
		portNumber, err := strconv.ParseUint(port, 10, 16)
		if err != nil || portNumber == 0 {
			return fmt.Errorf("invalid port at record %d", i)
		}
		if int(record.protocolID) >= len(snapshot.protocols) || int(record.countryID) >= len(snapshot.countries) || int(record.cityID) >= len(snapshot.cities) {
			return fmt.Errorf("dictionary reference out of range at record %d", i)
		}
		end := uint64(record.sourceOffset) + uint64(record.sourceCount)
		if record.sourceCount == 0 || int(record.sourceCount) > len(snapshot.sources) || end > uint64(len(snapshot.sourceRefs)) {
			return fmt.Errorf("source range out of bounds at record %d", i)
		}
		previousSourceKey := ""
		for offset := uint64(record.sourceOffset); offset < end; offset++ {
			ref := snapshot.sourceRefs[offset]
			sourceKey := snapshot.sourceKeys[ref]
			if offset > uint64(record.sourceOffset) && sourceKey <= previousSourceKey {
				return fmt.Errorf("source references are not strictly sorted at record %d", i)
			}
			previousSourceKey = sourceKey
		}
		if record.continent > 7 || record.status > candidateResource {
			return fmt.Errorf("invalid state at record %d", i)
		}
		if previous != nil && compareCandidateRecords(snapshot, *previous, snapshot, *record) >= 0 {
			return fmt.Errorf("candidate records are not strictly sorted at record %d", i)
		}
		previous = record
	}
	return nil
}

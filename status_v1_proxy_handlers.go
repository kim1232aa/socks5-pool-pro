package main

import (
	"fmt"
	"net/http"
	"strings"
)

// v1ProxyLight is the minimal retained form of a healthy proxy row: its pool
// index (for late materialization of the full V1ProxyView) plus the score
// needed for /pick. Only the smaller sorted prefix or suffix is retained.
type v1ProxyLight struct {
	index int
	key   string
	score float64
}

func v1ProxyLightLess(a, b v1ProxyLight) bool {
	if a.key != b.key {
		return a.key < b.key
	}
	return a.index < b.index
}

// handleV1Proxies serves a bounded, key-stable page of healthy proxies. The
// snapshot token is computed incrementally over every healthy proxy. Rows are
// counted before collection so out-of-range pages return without allocating a
// collector; valid pages retain only the smaller sorted prefix or suffix.
func (s *StatusServer) handleV1Proxies(w http.ResponseWriter, r *http.Request) {
	if err := validateCountryQuery(r); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_country", err)
		return
	}
	protocol, err := validatedV1ProxyProtocol(r.URL.Query().Get("protocol"))
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_protocol", err)
		return
	}
	page, pageSize, err := strictV1PageParams(r)
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_pagination", err)
		return
	}

	query := r.URL.Query()
	countryRaw := strings.TrimSpace(query.Get("country"))
	unknownCountry := strings.EqualFold(countryRaw, "__unknown__") || nodeQueryEnabled(query.Get("country_unknown"))
	country := ""
	if !unknownCountry {
		country = normalizedNodeCountry(countryRaw)
	}

	matches := func(px Proxy) bool {
		if protocol != "" && px.Protocol != protocol {
			return false
		}
		proxyCountry := normalizedNodeCountry(px.Country)
		if unknownCountry && proxyCountry != "" {
			return false
		}
		return unknownCountry || country == "" || proxyCountry == country
	}
	less := v1ProxyLightLess
	hasher := newV1ProxySnapshotHasher(apiBootNonce)
	availableTotal := 0
	filteredTotal := 0

	s.pool.mu.RLock()
	for _, px := range s.pool.proxies {
		if !px.Available || !proxyHardRoutable(px) {
			continue
		}
		switch px.Protocol {
		case "socks5", "http", "https":
		default:
			continue
		}
		availableTotal++
		// Feed every healthy proxy into the snapshot digest so the token still
		// reflects the full healthy set; score is omitted from the page token.
		hasher.feedProxy(px, false, 0)
		if matches(px) {
			filteredTotal++
		}
	}
	snapshotID := hasher.finish()
	w.Header().Set("X-Snapshot-ID", snapshotID)
	if requested := strings.TrimSpace(query.Get("snapshot_id")); requested != "" && requested != snapshotID {
		s.pool.mu.RUnlock()
		writeErrCode(w, http.StatusConflict, "snapshot_changed", fmt.Errorf("requested snapshot %q is no longer current", requested))
		return
	}
	pageCount := pageCountForTotal(filteredTotal, pageSize)
	if page > pageCount {
		s.pool.mu.RUnlock()
		writeErrCode(w, http.StatusBadRequest, "page_out_of_range", fmt.Errorf("page %d exceeds page_count %d", page, pageCount))
		return
	}

	start := pageWindowLimit(page-1, pageSize)
	pageEnd := min(pageWindowLimit(page, pageSize), filteredTotal)
	limit, reverse := pageCollectorWindow(filteredTotal, start, pageEnd)
	collectorLess := less
	if reverse {
		collectorLess = func(a, b v1ProxyLight) bool { return less(b, a) }
	}
	collector := newBoundedCollector[v1ProxyLight](limit, collectorLess)
	for i, px := range s.pool.proxies {
		if !px.Available || !proxyHardRoutable(px) {
			continue
		}
		switch px.Protocol {
		case "socks5", "http", "https":
		default:
			continue
		}
		if !matches(px) {
			continue
		}
		key := px.Key()
		collector.add(v1ProxyLight{index: i, key: key, score: scoreWithStats(px, s.pool.stats[key])})
	}
	ordered := collector.sortFinal(less)
	retainedStart := start
	if reverse {
		retainedStart = 0
	}
	retainedEnd := retainedStart + pageEnd - start
	rows := make([]V1ProxyView, 0, retainedEnd-retainedStart)
	for _, entry := range ordered[retainedStart:retainedEnd] {
		px := s.pool.proxies[entry.index]
		proxyURL := px.ConsumerURL()
		view := V1ProxyView{
			ProxyURL: proxyURL, Username: px.Username, Password: px.Password,
			Key: entry.key, Protocol: px.Protocol,
			Country: normalizedNodeCountry(px.Country), City: px.City,
			Latency: px.LatencyMs, Speed: px.SpeedKbps, score: entry.score,
		}
		if px.Protocol == "socks5" {
			view.SocksURL = proxyURL
		}
		rows = append(rows, view)
	}
	s.pool.mu.RUnlock()

	writeJSON(w, V1ProxyPage{
		APIVersion: "v1", SnapshotID: snapshotID, Proxies: rows,
		Page: page, PageSize: pageSize, PageCount: pageCount, HasNext: page < pageCount,
		FilteredTotal: filteredTotal, AvailableTotal: availableTotal,
	})
}

// handleV1ProxyPick returns the single best healthy proxy matching the filters.
// It streams the snapshot hash (score-aware, like the historical pick token)
// while keeping only the current best candidate, never materializing the full
// healthy list.
func (s *StatusServer) handleV1ProxyPick(w http.ResponseWriter, r *http.Request) {
	if err := validateCountryQuery(r); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_country", err)
		return
	}
	protocol, err := validatedV1ProxyProtocol(r.URL.Query().Get("protocol"))
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_protocol", err)
		return
	}

	query := r.URL.Query()
	countryRaw := strings.TrimSpace(query.Get("country"))
	unknownCountry := strings.EqualFold(countryRaw, "__unknown__") || nodeQueryEnabled(query.Get("country_unknown"))
	country := ""
	if !unknownCountry {
		country = normalizedNodeCountry(countryRaw)
	}

	hasher := newV1ProxySnapshotHasher(apiBootNonce)
	var selectedIndex int = -1
	var selectedKey string
	var selectedScore float64
	hasSelected := false

	s.pool.mu.RLock()
	for i, px := range s.pool.proxies {
		if !px.Available || !proxyHardRoutable(px) {
			continue
		}
		switch px.Protocol {
		case "socks5", "http", "https":
		default:
			continue
		}
		key := px.Key()
		score := scoreWithStats(px, s.pool.stats[key])
		// Score-aware digest: /pick must invalidate when the best node changes.
		hasher.feedProxy(px, true, score)

		if protocol != "" && px.Protocol != protocol {
			continue
		}
		proxyCountry := normalizedNodeCountry(px.Country)
		if unknownCountry && proxyCountry != "" {
			continue
		}
		if !unknownCountry && country != "" && proxyCountry != country {
			continue
		}
		if !hasSelected || score > selectedScore || (score == selectedScore && key < selectedKey) {
			selectedIndex = i
			selectedKey = key
			selectedScore = score
			hasSelected = true
		}
	}
	var selected V1ProxyView
	if hasSelected {
		px := s.pool.proxies[selectedIndex]
		proxyURL := px.ConsumerURL()
		selected = V1ProxyView{
			ProxyURL: proxyURL, Username: px.Username, Password: px.Password,
			Key: selectedKey, Protocol: px.Protocol,
			Country: normalizedNodeCountry(px.Country), City: px.City,
			Latency: px.LatencyMs, Speed: px.SpeedKbps, score: selectedScore,
		}
		if px.Protocol == "socks5" {
			selected.SocksURL = proxyURL
		}
	}
	s.pool.mu.RUnlock()

	snapshotID := hasher.finishPick()
	w.Header().Set("X-Snapshot-ID", snapshotID)
	if requested := strings.TrimSpace(r.URL.Query().Get("snapshot_id")); requested != "" && requested != snapshotID {
		writeErrCode(w, http.StatusConflict, "snapshot_changed", fmt.Errorf("requested snapshot %q is no longer current", requested))
		return
	}
	if !hasSelected {
		writeErrCode(w, http.StatusNotFound, "proxy_not_found", fmt.Errorf("no healthy proxy matches the requested filters"))
		return
	}
	writeJSON(w, V1ProxyPickResponse{APIVersion: "v1", SnapshotID: snapshotID, Proxy: selected})
}

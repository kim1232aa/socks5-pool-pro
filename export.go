package main

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// exportNode is a Proxy plus its computed quality stats, used only for
// building export files.
type exportNode struct {
	Proxy
	Score     float64
	Successes int
	Failures  int
}

// collectExport gathers the live forwarding nodes, applies the same
// filters the dashboard offers, and sorts by latency ascending (unknown
// latency sinks to the bottom).
func (s *StatusServer) collectExport(r *http.Request) []exportNode {
	q := r.URL.Query()
	country := q.Get("country")
	protocol := q.Get("protocol")
	onlyChanged := q.Get("only_changed") == "1"
	onlyAvailable := q.Get("available") == "1"
	search := strings.ToLower(strings.TrimSpace(q.Get("search")))

	var out []exportNode
	for _, px := range s.pool.All() {
		if country != "" && px.Country != country {
			continue
		}
		if protocol != "" && px.Protocol != protocol {
			continue
		}
		if onlyChanged && (!px.IPChangeKnown || !px.IPChanged) {
			continue
		}
		if onlyAvailable && !px.Available {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(px.Addr()+" "+px.ExitIP), search) {
			continue
		}
		succ, fail := s.pool.StatsOf(px.Key())
		out = append(out, exportNode{Proxy: px, Score: s.pool.Score(px), Successes: succ, Failures: fail})
	}

	sort.SliceStable(out, func(i, j int) bool {
		li, lj := out[i].LatencyMs, out[j].LatencyMs
		if li <= 0 {
			li = 1 << 62
		}
		if lj <= 0 {
			lj = 1 << 62
		}
		return li < lj
	})
	return out
}

func (s *StatusServer) handleNodeExport(w http.ResponseWriter, r *http.Request) {
	nodes := s.collectExport(r)
	switch r.URL.Query().Get("format") {
	case "tme":
		exportTME(w, nodes)
	default:
		exportCSV(w, nodes)
	}
}

// exportCSV writes a UTF-8 (BOM-prefixed, so Excel opens it without
// mojibake) spreadsheet of all nodes, sorted by latency.
func exportCSV(w http.ResponseWriter, nodes []exportNode) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="proxies-by-latency.csv"`)
	w.Write([]byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM for Excel

	cw := csv.NewWriter(w)
	defer cw.Flush()

	cw.Write([]string{
		"协议", "地址", "IP", "端口", "用户名", "密码",
		"出口IP", "是否改IP", "匿名", "大洲", "国家", "城市",
		"评分", "延迟ms", "速度kbps", "成功", "失败", "来源", "t.me链接",
	})

	for _, n := range nodes {
		latency := ""
		if n.LatencyMs > 0 {
			latency = strconv.FormatInt(n.LatencyMs, 10)
		}
		speed := ""
		if n.SpeedKbps > 0 {
			speed = strconv.FormatFloat(n.SpeedKbps, 'f', 0, 64)
		}
		anon := n.Anonymity
		if anon == "" {
			anon = "unknown"
		}
		changed := "未知"
		if n.IPChangeKnown && n.IPChanged {
			changed = "是"
		} else if n.IPChangeKnown {
			changed = "否"
		}
		sources := n.SourceName
		if len(n.SourceNames) > 0 {
			sources = strings.Join(n.SourceNames, ", ")
		}
		record := []string{
			n.Protocol, n.Addr(), n.IP, n.Port, n.Username, n.Password,
			n.ExitIP, changed, anon, n.Continent, n.Country, n.City,
			strconv.FormatFloat(n.Score, 'f', 1, 64), latency, speed,
			strconv.Itoa(n.Successes), strconv.Itoa(n.Failures),
			sources, tmeLink(n.Proxy),
		}
		for i := range record {
			record[i] = spreadsheetSafeCell(record[i])
		}
		cw.Write(record)
	}
}

// spreadsheetSafeCell prevents data supplied by third-party proxy feeds from
// becoming an Excel/LibreOffice formula when the CSV is opened. CSV quoting is
// not sufficient: spreadsheet applications still evaluate quoted cells that
// begin with =, +, -, @, tab, or a formula after leading whitespace.
func spreadsheetSafeCell(value string) string {
	if value == "" {
		return value
	}
	trimmed := strings.TrimLeftFunc(value, unicode.IsSpace)
	if trimmed == "" {
		return value
	}
	switch trimmed[0] {
	case '=', '+', '-', '@':
		return "'" + value
	}
	switch value[0] {
	case '\t', '\r', '\n':
		return "'" + value
	}
	return value
}

// exportTME writes Telegram SOCKS proxy links, one per line. Only socks5
// nodes are emitted, since the t.me/socks scheme is SOCKS5-only.
func exportTME(w http.ResponseWriter, nodes []exportNode) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="proxies-tme.txt"`)
	for _, n := range nodes {
		if n.Protocol != "socks5" {
			continue
		}
		fmt.Fprintln(w, tmeLink(n.Proxy))
	}
}

// tmeLink builds a https://t.me/socks?server=...&port=...&user=...&pass=...
// link for a proxy, in that exact parameter order. user/pass are included
// only when the proxy has auth. Returns "" for non-socks5 protocols (the
// t.me/socks scheme is SOCKS5-only).
func tmeLink(px Proxy) string {
	if px.Protocol != "socks5" {
		return ""
	}
	s := "https://t.me/socks?server=" + url.QueryEscape(px.IP) + "&port=" + url.QueryEscape(px.Port)
	if px.Username != "" {
		s += "&user=" + url.QueryEscape(px.Username) + "&pass=" + url.QueryEscape(px.Password)
	}
	return s
}

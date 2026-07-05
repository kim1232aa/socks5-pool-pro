# socks5-pool

A self-rotating SOCKS5/HTTP proxy pool with pluggable node sources, rule-based traffic splitting, and multiple load-balancing strategies. Scrapes proxies from a configurable set of sources, verifies real connectivity, filters out CN/HK IPs, and exposes a local SOCKS5 endpoint that automatically rotates or load-balances across upstreams.

## Features

- **Multiple pluggable node sources** - ships with 11 active sources across 3 protocols (SOCKS5/HTTP/HTTPS) and 5 data formats; add/remove/enable/disable your own from the dashboard, no restart required
- Concurrent health checks with real connectivity verification (through the actual upstream tunnel, works for SOCKS5 and HTTP/HTTPS proxies alike)
- Auto-filters China/Hong Kong proxies
- **Rule-based traffic splitting** - Clash-style rules (DOMAIN / DOMAIN-SUFFIX / DOMAIN-KEYWORD / IP-CIDR / MATCH) route different destinations to different node groups, or straight to `DIRECT` (no proxy)
- **Custom groups with independent load-balancing strategies** - filter by country/protocol/source, then pick with `sticky` / `round-robin` / `random` / `latency` / `speed`
- **On-demand speed test** - measure real download throughput per node from the dashboard
- IP auto-rotation every 3-6 minutes (random) for the default group
- Auto-failover: switches to another upstream in the same group on connection failure (up to 3 retries)
- Full web dashboard: nodes, sources, routing rules, and groups - all editable live
- Zero external dependencies (Go stdlib only)

## Quick Start

```bash
# Build
go build -o socks5-pool .

# Run with defaults (SOCKS5 on :1080, dashboard on :8080)
./socks5-pool

# Custom config
./socks5-pool -listen 127.0.0.1:1080 -status 127.0.0.1:8080 -scrape-interval 15m
```

## CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-listen` | `127.0.0.1:1080` | SOCKS5 listen address |
| `-status` | `127.0.0.1:8080` | HTTP dashboard address |
| `-data-dir` | `./data` | directory for persisted sources/rules/groups config (`pool_config.json`) |
| `-scrape-interval` | `20m` | how often all enabled sources are re-fetched and re-checked |
| `-check-timeout` | `10s` | per-proxy health check timeout |
| `-max-concurrent` | `20` | max concurrent health checks |
| `-max-candidates` | `3000` | cap on total candidates checked per refresh cycle |

There's no `-url` flag anymore - node sources are managed entirely through the dashboard/API and persisted to `<data-dir>/pool_config.json`.

### About `-max-candidates`

A few of the built-in sources (particularly the Fyvri feed) return 100k+ raw entries. Health-checking all of them every cycle would take hours and would hammer the free geo-lookup API's rate limit. When the combined, deduplicated candidate list exceeds `-max-candidates`, a random subset of that size is sampled for this cycle - the log line says how many were skipped. Over several refresh cycles this eventually samples across the whole source rather than always checking the same slice.

## Built-in Node Sources

| Source | Protocol(s) | Format | Update cadence | Enabled by default |
|--------|-------------|--------|-----------------|---------------------|
| socks5-proxy.github.io | socks5 | text | - | yes |
| EDT-Pages SOCKS5/HTTP/HTTPS | socks5/http/https | JSON | - | yes |
| Proxifly SOCKS5/HTTP | socks5/http | text | ~5 min | yes |
| Monosans SOCKS5/HTTP | socks5/http | text | hourly | yes |
| Fyvri SOCKS5/HTTP/HTTPS | socks5/http/https | JSON array | hourly | yes |
| ProxyIP (Cloudflare edge) | n/a | JSON | - | **no** (see below) |

Two well-known lists were deliberately **not** added as defaults after checking them: `clarketm/proxy-list` (last updated 2023-03-21) and `jetkai/proxy-list` (last real update 2023-04-15) are both stale snapshots, not live feeds. You can still add either one manually from the dashboard if you want - dead entries just get filtered out during health checks.

### About the "ProxyIP" source

`zip.cm.edu.kg/all.json` is disabled by default because it isn't actually a SOCKS5/HTTP proxy list - it's a list of Cloudflare edge IPs used as the "ProxyIP" parameter in Worker/VLESS/Trojan-style reverse-tunnel tools. These IPs don't speak SOCKS5 or HTTP CONNECT, so they can never be used as a forwarding upstream by this project. If you enable this source anyway, its entries show up in a separate read-only "ProxyIP" panel (for copying elsewhere) and are never selected for actual traffic forwarding.

## Adding Your Own Source

From the **来源 (Sources)** tab, or via API:

```bash
curl -X POST http://localhost:8080/api/sources -d '{
  "name": "my-list",
  "url": "https://example.com/proxies.json",
  "format": "edt-json"
}'
```

Supported `format` values:

| Format | Shape | Needs `protocol` field? |
|--------|-------|--------------------------|
| `text-regex` | free text/HTML, scans for `scheme://ip:port` | no |
| `edt-json` | JSON array of `{proxy, protocol, ip, port, country, city, ...}` | no |
| `proxyip-json` | `{"data":[{"ip","port":[...],"meta":{...}}]}` (Cloudflare ProxyIP) | no |
| `plain-list` | newline-separated `ip:port`, no scheme | **yes** (`socks5`/`http`/`https`) |
| `json-array` | JSON array of `"ip:port"` strings, no scheme | **yes** (`socks5`/`http`/`https`) |

## Routing Rules & Groups

Rules are evaluated top-to-bottom; the first match decides which **group** (or `DIRECT`, meaning "skip the proxy entirely") handles a connection. There's always a trailing `MATCH` rule as the fallback - edit its target group instead of deleting it.

Rule types: `DOMAIN`, `DOMAIN-SUFFIX`, `DOMAIN-KEYWORD`, `IP-CIDR`.

Groups are named, filtered subsets of the live pool (by country / protocol / source name) with their own load-balancing strategy:

| Strategy | Behavior |
|----------|----------|
| `sticky` | stay on one upstream until manually switched or it fails |
| `round-robin` | rotate to the next member on every new connection |
| `random` | pick uniformly at random on every new connection |
| `latency` | prefer the lowest measured health-check latency |
| `speed` | prefer the highest on-demand speed-test result (run speed tests first, or it behaves like picking the first member) |

Example: route Netflix traffic to a US-only group, keep everything else on the default pool:

```bash
curl -X POST http://localhost:8080/api/groups -d '{"name":"US","strategy":"latency","countries":["US"]}'
curl -X POST http://localhost:8080/api/rules  -d '{"type":"DOMAIN-SUFFIX","value":"netflix.com","group":"US"}'
```

## Dashboard

Open `http://localhost:8080` for the web dashboard - four tabs, all backed by a JSON API:

- **节点 (Nodes)** - every live forwarding proxy with protocol/country/source/latency, manual "use this node" switch, on-demand speed test, plus a collapsible read-only ProxyIP panel
- **来源 (Sources)** - enable/disable/delete sources, add new ones
- **分流规则 (Rules)** - ordered rule list, reorder/delete, add new rules, edit the default (MATCH) group
- **分组策略 (Groups)** - custom groups, their filters/strategy, member counts and current pick

### API

```
GET  /api/status                  # summary: counts, scrape times, group states
GET  /api/refresh                 # trigger a pool refresh
POST /api/nodes/switch            # {key} pin a node as the ANY group's active upstream
POST /api/nodes/speedtest         # {key} run an on-demand throughput test

GET  /api/sources                 # list sources
POST /api/sources                 # {name,url,format,protocol?} add a source
POST /api/sources/toggle          # {id,enabled}
POST /api/sources/delete          # {id}

GET  /api/rules                   # list rules (ordered)
POST /api/rules                   # {type,value,group} add a rule
POST /api/rules/delete            # {id}
POST /api/rules/move              # {id,delta} reorder (-1 up, +1 down)
POST /api/rules/default           # {group} change the trailing MATCH rule's target

GET  /api/groups                  # list custom groups
POST /api/groups                  # {name,strategy,countries?,protocols?,sources?} create a group
POST /api/groups/strategy         # {id,strategy} change a group's strategy
POST /api/groups/delete           # {id}
```

## Docker

```bash
docker build -t socks5-pool .
docker run -p 1080:1080 -p 8080:8080 -v $(pwd)/data:/app/data socks5-pool
```

## Deploy to Railway

The project includes `railway.toml` for one-click Railway deployment.

## Known Limitations

- Geo lookups (for sources that don't already supply country/city) go through `ip-api.com`'s free tier, which rate-limits aggressively under load; entries that get rate-limited just show up with an unknown country rather than failing the health check.
- `speed` strategy only has data for nodes you've manually speed-tested at least once; untested nodes fall back to "first candidate."

## Project Structure

```
├── main.go           # Entry point, multi-source refresh & rotation loops
├── config.go         # CLI flag parsing
├── proxy.go          # Proxy struct + proxy-URL parsing
├── parser.go         # Per-format node-list parsers + fetch dispatcher
├── sourcestore.go    # Source/PoolConfig persistence (pool_config.json)
├── rules.go          # Routing rules, groups, rule matcher, Rule/Group CRUD
├── pool.go           # Live pool, group filtering, load-balancing strategies
├── dial.go           # Upstream dialers (SOCKS5 client, HTTP CONNECT)
├── checker.go        # Health checks & geo lookup
├── speedtest.go       # On-demand throughput measurement
├── server.go         # Local SOCKS5 protocol implementation + rule-based routing
├── status.go         # Dashboard HTTP handlers + REST API
├── dashboard_html.go  # Dashboard HTML/CSS/JS template
├── Dockerfile         # Multi-stage Docker build
└── railway.toml       # Railway deployment config
```

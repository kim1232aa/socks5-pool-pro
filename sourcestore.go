package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// defaultCheckURL is the health-check target used until the user sets
// their own via the dashboard - Google's connectivity-check endpoint,
// chosen because it's free of the rate limits a heavier destination (or
// one the user later points at, e.g. an app's own homepage) might impose
// under hundreds of concurrent probes.
const defaultCheckURL = "http://www.google.com/generate_204"

const (
	builtinProxyIPSourceID = "builtin-proxyip"
	maxConfiguredSources   = 64
	maxSourceNameBytes     = 256
	maxSourceURLBytes      = 8 << 10

	legacyProxyIPSourceName = "ProxyIP (Cloudflare edge)"
	legacyProxyIPSourceNote = "这些是 Cloudflare 边缘优选 IP，用于 Worker/VLESS/Trojan 类隧道脚本的反代地址，不支持通用 SOCKS5/HTTP 协议，不会参与本地转发，仅供查看和导出使用"

	currentProxyIPSourceName = "ProxyIP (Cloudflare Worker reverse proxy)"
	currentProxyIPSourceNote = "这些是供 Cloudflare Worker/VLESS/Trojan 类隧道使用的外部反代跳板，不是 Cloudflare 边缘 IP，也不支持通用 SOCKS5/HTTP 协议；不会参与本地转发，仅供目录查看和复制"
)

// Node-list source formats.
const (
	FormatTextRegex   = "text-regex"   // "scheme://ip:port" occurrences in free text
	FormatEDTJSON     = "edt-json"     // JSON array of {proxy,protocol,ip,port,country,city,...}
	FormatProxyIPJSON = "proxyip-json" // {"data":[{"ip","port":[...],"meta":{...}}]} (Cloudflare ProxyIP)
	FormatPlainList   = "plain-list"   // newline-separated "ip:port", protocol from Source.Protocol
	FormatJSONArray   = "json-array"   // JSON array of "ip:port" strings, protocol from Source.Protocol
)

// Source is a single configurable node-list feed. Built-in sources ship
// with the project; users can add/remove their own from the dashboard.
type Source struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Format string `json:"format"`
	// Protocol tags every entry parsed from this source. Required for
	// FormatPlainList/FormatJSONArray, which don't encode a protocol
	// themselves; ignored for other formats (they carry their own).
	Protocol string `json:"protocol,omitempty"`
	Enabled  bool   `json:"enabled"`
	// AllowPrivate is an explicit escape hatch for trusted LAN-hosted feeds.
	// It affects only the source download URL; proxy endpoints advertised by a
	// feed are not removed or rewritten by this setting.
	AllowPrivate bool   `json:"allow_private,omitempty"`
	Builtin      bool   `json:"builtin"`
	Note         string `json:"note,omitempty"`
}

// PoolConfig is the full persisted state: sources, routing rules, custom
// groups, and the health-check target URL.
type PoolConfig struct {
	Sources []Source `json:"sources"`
	Rules   []Rule   `json:"rules"`
	Groups  []Group  `json:"groups"`
	// CheckURL is the sole criterion for "is this node alive": every
	// candidate is dialed through and this URL is fetched - a node is
	// alive if it gets any HTTP response back. Empty means defaultCheckURL
	// (see CheckURL()). User-settable from the dashboard so "alive" can
	// mean whatever the user actually cares about reaching (their own
	// app's URL, a specific streaming service, etc.), not just Google.
	CheckURL string `json:"check_url,omitempty"`
}

// ConfigStore persists PoolConfig to a JSON file on disk, guarding all
// access with a mutex and writing atomically (temp file + rename).
type ConfigStore struct {
	mu   sync.RWMutex
	path string
	cfg  PoolConfig
}

func NewConfigStore(dataDir string) (*ConfigStore, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	cs := &ConfigStore{path: filepath.Join(dataDir, "pool_config.json")}

	data, err := os.ReadFile(cs.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read config: %w", err)
		}
		cs.cfg = defaultPoolConfig()
		if err := cs.writeLocked(); err != nil {
			return nil, err
		}
		return cs, nil
	}

	var cfg PoolConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cs.cfg = cfg
	if migrateProxyIPSourceMetadata(&cs.cfg) {
		if err := cs.writeLocked(); err != nil {
			return nil, fmt.Errorf("persist source metadata migration: %w", err)
		}
	}
	return cs, nil
}

// migrateProxyIPSourceMetadata updates only the two obsolete, known metadata
// strings written by older releases. It intentionally does not reconcile the
// persisted source list with defaultPoolConfig: operators may delete built-ins,
// change their URL/format/enabled state, or give them custom labels. A legacy
// record that predates the Builtin flag is still safe to migrate when one of
// its metadata fields exactly matches the old value.
func migrateProxyIPSourceMetadata(cfg *PoolConfig) bool {
	changed := false
	for i := range cfg.Sources {
		source := &cfg.Sources[i]
		if source.ID != builtinProxyIPSourceID {
			continue
		}
		knownLegacyMetadata := source.Name == legacyProxyIPSourceName || source.Note == legacyProxyIPSourceNote
		if !source.Builtin && !knownLegacyMetadata {
			continue
		}
		if source.Name == legacyProxyIPSourceName {
			source.Name = currentProxyIPSourceName
			changed = true
		}
		if source.Note == legacyProxyIPSourceNote {
			source.Note = currentProxyIPSourceNote
			changed = true
		}
	}
	return changed
}

func defaultPoolConfig() PoolConfig {
	return PoolConfig{
		Sources: []Source{
			{
				ID:      "builtin-socks5-github",
				Name:    "socks5-proxy.github.io",
				URL:     "https://socks5-proxy.github.io/",
				Format:  FormatTextRegex,
				Enabled: true,
				Builtin: true,
			},
			{
				ID:      "builtin-edt-socks5",
				Name:    "EDT-Pages SOCKS5",
				URL:     "https://raw.githubusercontent.com/EDT-Pages/Proxy-List/main/data/socks5.json",
				Format:  FormatEDTJSON,
				Enabled: true,
				Builtin: true,
			},
			{
				ID:      "builtin-edt-http",
				Name:    "EDT-Pages HTTP",
				URL:     "https://raw.githubusercontent.com/EDT-Pages/Proxy-List/main/data/http.json",
				Format:  FormatEDTJSON,
				Enabled: true,
				Builtin: true,
			},
			{
				ID:      "builtin-edt-https",
				Name:    "EDT-Pages HTTPS",
				URL:     "https://raw.githubusercontent.com/EDT-Pages/Proxy-List/main/data/https.json",
				Format:  FormatEDTJSON,
				Enabled: true,
				Builtin: true,
			},
			{
				ID:      "builtin-proxifly-socks5",
				Name:    "Proxifly SOCKS5",
				URL:     "https://cdn.jsdelivr.net/gh/proxifly/free-proxy-list@main/proxies/protocols/socks5/data.txt",
				Format:  FormatTextRegex,
				Enabled: true,
				Builtin: true,
			},
			{
				ID:      "builtin-proxifly-http",
				Name:    "Proxifly HTTP",
				URL:     "https://cdn.jsdelivr.net/gh/proxifly/free-proxy-list@main/proxies/protocols/http/data.txt",
				Format:  FormatTextRegex,
				Enabled: true,
				Builtin: true,
			},
			{
				ID:       "builtin-monosans-socks5",
				Name:     "Monosans SOCKS5",
				URL:      "https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks5.txt",
				Format:   FormatPlainList,
				Protocol: "socks5",
				Enabled:  true,
				Builtin:  true,
			},
			{
				ID:       "builtin-monosans-http",
				Name:     "Monosans HTTP",
				URL:      "https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/http.txt",
				Format:   FormatPlainList,
				Protocol: "http",
				Enabled:  true,
				Builtin:  true,
			},
			{
				ID:       "builtin-fyvri-socks5",
				Name:     "Fyvri SOCKS5",
				URL:      "https://raw.githubusercontent.com/fyvri/fresh-proxy-list/archive/storage/classic/socks5.json",
				Format:   FormatJSONArray,
				Protocol: "socks5",
				Enabled:  true,
				Builtin:  true,
			},
			{
				ID:       "builtin-fyvri-http",
				Name:     "Fyvri HTTP",
				URL:      "https://raw.githubusercontent.com/fyvri/fresh-proxy-list/archive/storage/classic/http.json",
				Format:   FormatJSONArray,
				Protocol: "http",
				Enabled:  true,
				Builtin:  true,
			},
			{
				ID:       "builtin-fyvri-https",
				Name:     "Fyvri HTTPS",
				URL:      "https://raw.githubusercontent.com/fyvri/fresh-proxy-list/archive/storage/classic/https.json",
				Format:   FormatJSONArray,
				Protocol: "https",
				Enabled:  true,
				Builtin:  true,
			},
			{
				ID:      builtinProxyIPSourceID,
				Name:    currentProxyIPSourceName,
				URL:     "https://zip.cm.edu.kg/all.json",
				Format:  FormatProxyIPJSON,
				Enabled: false,
				Builtin: true,
				Note:    currentProxyIPSourceNote,
			},
		},
		Rules: []Rule{
			{ID: "default-match", Type: RuleMatch, Group: GroupAny},
		},
		Groups: []Group{},
	}
}

func (cs *ConfigStore) writeLocked() error {
	data, err := json.MarshalIndent(cs.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := cs.path + ".tmp"
	// Source definitions and future custom upstreams can include credentials;
	// keep the persisted configuration private to the service account.
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, cs.path)
}

// mutate runs fn with exclusive access to the config, then persists it.
func (cs *ConfigStore) mutate(fn func(*PoolConfig) error) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if err := fn(&cs.cfg); err != nil {
		return err
	}
	return cs.writeLocked()
}

func (cs *ConfigStore) Snapshot() PoolConfig {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	out := PoolConfig{
		Sources: make([]Source, len(cs.cfg.Sources)),
		Rules:   make([]Rule, len(cs.cfg.Rules)),
		Groups:  make([]Group, len(cs.cfg.Groups)),
	}
	copy(out.Sources, cs.cfg.Sources)
	copy(out.Rules, cs.cfg.Rules)
	copy(out.Groups, cs.cfg.Groups)
	return out
}

func (cs *ConfigStore) Sources() []Source {
	return cs.Snapshot().Sources
}

func (cs *ConfigStore) EnabledSources() []Source {
	var out []Source
	for _, s := range cs.Sources() {
		if s.Enabled {
			// refreshPool also logs the display name on fetch failure. Sanitize a
			// copied value here so legacy config written before AddSource validation
			// cannot inject control lines or an unbounded label into logs.
			s.Name = safeLogLabel(s.Name)
			out = append(out, s)
		}
	}
	return out
}

func (cs *ConfigStore) AddSource(s Source) (Source, error) {
	s.Name = strings.TrimSpace(s.Name)
	s.URL = strings.TrimSpace(s.URL)
	s.Protocol = strings.ToLower(strings.TrimSpace(s.Protocol))
	if s.Name == "" || s.URL == "" {
		return Source{}, fmt.Errorf("name and url are required")
	}
	if len(s.Name) > maxSourceNameBytes || hasLogControlCharacters(s.Name) {
		return Source{}, fmt.Errorf("source name must be at most %d bytes and contain no control characters", maxSourceNameBytes)
	}
	if len(s.URL) > maxSourceURLBytes {
		return Source{}, fmt.Errorf("source url exceeds %d bytes", maxSourceURLBytes)
	}
	if _, err := validateSourceURL(s.URL, s.AllowPrivate); err != nil {
		return Source{}, err
	}
	switch s.Format {
	case FormatTextRegex, FormatEDTJSON, FormatProxyIPJSON:
	case FormatPlainList, FormatJSONArray:
		if !isForwardingProtocol(s.Protocol) {
			return Source{}, fmt.Errorf("format %q requires protocol socks5, http, or https", s.Format)
		}
	default:
		return Source{}, fmt.Errorf("unknown format: %q", s.Format)
	}
	s.ID = generateID("src")
	s.Builtin = false
	s.Enabled = true // a source you just added is one you want to use now

	err := cs.mutate(func(c *PoolConfig) error {
		if len(c.Sources) >= maxConfiguredSources {
			return fmt.Errorf("source limit reached: at most %d sources are allowed", maxConfiguredSources)
		}
		for _, existing := range c.Sources {
			if existing.URL == s.URL {
				return fmt.Errorf("source with this URL already exists: %s", existing.Name)
			}
		}
		c.Sources = append(c.Sources, s)
		return nil
	})
	return s, err
}

func (cs *ConfigStore) DeleteSource(id string) error {
	return cs.mutate(func(c *PoolConfig) error {
		for i, s := range c.Sources {
			if s.ID == id {
				c.Sources = append(c.Sources[:i], c.Sources[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("source not found: %s", id)
	})
}

func (cs *ConfigStore) ToggleSource(id string, enabled bool) error {
	return cs.mutate(func(c *PoolConfig) error {
		for i, s := range c.Sources {
			if s.ID == id {
				c.Sources[i].Enabled = enabled
				return nil
			}
		}
		return fmt.Errorf("source not found: %s", id)
	})
}

// CheckURL returns the currently configured health-check target,
// defaulting to defaultCheckURL if the user hasn't set one.
func (cs *ConfigStore) CheckURL() string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.cfg.CheckURL == "" {
		return defaultCheckURL
	}
	return cs.cfg.CheckURL
}

// SetCheckURL changes the health-check target. Takes effect on the next
// check cycle - callers typically follow this with TriggerRefresh so it
// applies immediately rather than waiting for the next scheduled scrape.
func (cs *ConfigStore) SetCheckURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("url is required")
	}
	if len(raw) > maxSourceURLBytes {
		return fmt.Errorf("url exceeds %d bytes", maxSourceURLBytes)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid url: must be a full http:// or https:// address")
	}
	// Health checks are sent through untrusted public proxies and the configured
	// target is mentioned in operational diagnostics. Do not accept credentials
	// in userinfo or a never-transmitted fragment where they are easy to leak or
	// misunderstand. Query parameters remain supported for compatibility.
	if u.User != nil {
		return fmt.Errorf("invalid url: embedded username/password is not allowed")
	}
	if u.Fragment != "" {
		return fmt.Errorf("invalid url: fragments are not allowed")
	}
	return cs.mutate(func(c *PoolConfig) error {
		c.CheckURL = raw
		return nil
	})
}

func hasLogControlCharacters(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func generateID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), rand.Intn(1_000_000))
}

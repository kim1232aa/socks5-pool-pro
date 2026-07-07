package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"
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
	Builtin  bool   `json:"builtin"`
	Note     string `json:"note,omitempty"`
}

// PoolConfig is the full persisted state: sources, routing rules, and
// custom groups.
type PoolConfig struct {
	Sources []Source `json:"sources"`
	Rules   []Rule   `json:"rules"`
	Groups  []Group  `json:"groups"`
}

// ConfigStore persists PoolConfig to a JSON file on disk, guarding all
// access with a mutex and writing atomically (temp file + rename).
type ConfigStore struct {
	mu   sync.RWMutex
	path string
	cfg  PoolConfig
}

func NewConfigStore(dataDir string) (*ConfigStore, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
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
	return cs, nil
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
				ID:      "builtin-proxyip",
				Name:    "ProxyIP (Cloudflare edge)",
				URL:     "https://zip.cm.edu.kg/all.json",
				Format:  FormatProxyIPJSON,
				Enabled: false,
				Builtin: true,
				Note:    "这些是 Cloudflare 边缘优选 IP，用于 Worker/VLESS/Trojan 类隧道脚本的反代地址，不支持通用 SOCKS5/HTTP 协议，不会参与本地转发，仅供查看和导出使用",
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
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
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
			out = append(out, s)
		}
	}
	return out
}

func (cs *ConfigStore) AddSource(s Source) (Source, error) {
	if s.Name == "" || s.URL == "" {
		return Source{}, fmt.Errorf("name and url are required")
	}
	switch s.Format {
	case FormatTextRegex, FormatEDTJSON, FormatProxyIPJSON:
	case FormatPlainList, FormatJSONArray:
		if s.Protocol == "" {
			return Source{}, fmt.Errorf("protocol is required for format %q", s.Format)
		}
	default:
		return Source{}, fmt.Errorf("unknown format: %q", s.Format)
	}
	s.ID = generateID("src")
	s.Builtin = false
	s.Enabled = true // a source you just added is one you want to use now

	err := cs.mutate(func(c *PoolConfig) error {
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

func generateID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), rand.Intn(1_000_000))
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultCheckURL is the health-check target used until the user sets
// their own via the dashboard - Google's connectivity-check endpoint,
// chosen because it's free of the rate limits a heavier destination (or
// one the user later points at, e.g. an app's own homepage) might impose
// under hundreds of concurrent probes. HTTPS prevents a transparent HTTP
// intermediary from manufacturing the default probe response. The one exact
// HTTP URL shipped as the historical default is migrated below; arbitrary
// operator-saved HTTP targets remain valid and are preserved.
const defaultCheckURL = "https://www.google.com/generate_204"
const legacyDefaultCheckURL = "http://www.google.com/generate_204"

const (
	builtinProxyIPSourceID = "builtin-proxyip"
	maxConfiguredSources   = 64
	maxSourceNameBytes     = 256
	maxSourceURLBytes      = 8 << 10
	maxPoolConfigBytes     = 8 << 20
	maxConfigRules         = 4096
	maxConfigGroups        = 1024
	maxConfigListValues    = 4096
	maxConfigValueBytes    = 8 << 10
	maxConfigListeners     = 256

	minSourceRefreshIntervalSeconds = 60
	maxSourceRefreshIntervalSeconds = 7 * 24 * 60 * 60

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
	AllowPrivate bool `json:"allow_private,omitempty"`
	// AllowEmpty makes an HTTP 200 response containing no valid proxy records an
	// authoritative empty inventory. It is deliberately opt-in: by default an
	// unexpectedly empty feed is treated as a failed refresh so its last-known
	// good candidates remain available.
	AllowEmpty bool `json:"allow_empty,omitempty"`
	// AutoRefreshEnabled controls scheduled refreshes for this source.
	// RefreshIntervalSeconds is zero when the global scrape interval applies.
	AutoRefreshEnabled     bool   `json:"auto_refresh_enabled"`
	RefreshIntervalSeconds int    `json:"refresh_interval_seconds"`
	Builtin                bool   `json:"builtin"`
	Note                   string `json:"note,omitempty"`
	autoRefreshMissing     bool
}

// UnmarshalJSON preserves an explicit false while defaulting legacy records
// that predate auto_refresh_enabled to automatic refreshes.
func (source *Source) UnmarshalJSON(data []byte) error {
	type sourceJSON Source
	out := sourceJSON{AutoRefreshEnabled: true}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	_, present := fields["auto_refresh_enabled"]
	out.autoRefreshMissing = !present
	*source = Source(out)
	return nil
}

// PoolConfig is the full persisted state: sources, routing rules, custom
// groups, listener bindings, and the health-check target URL.
type PoolConfig struct {
	Sources   []Source          `json:"sources"`
	Rules     []Rule            `json:"rules"`
	Groups    []Group           `json:"groups"`
	Listeners []ListenerBinding `json:"listeners"`
	// CheckURL is the sole criterion for "is this node alive": every
	// candidate is dialed through and this URL is fetched without following
	// redirects. The built-in default must return exactly 204; a custom target
	// must return a 2xx response. Empty means defaultCheckURL (see CheckURL()).
	// User-settable from the dashboard so "alive" can mean whatever the user
	// actually cares about reaching, not just Google.
	CheckURL string `json:"check_url,omitempty"`
}

// UnmarshalJSON bounds the top-level collections while they are decoded. A
// byte cap alone is insufficient because a few MiB of compact `{}`/`""`
// records can expand into much larger Go slices before post-parse validation.
func (cfg *PoolConfig) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("pool config must be an object")
	}
	var out PoolConfig
	seen := make(map[string]bool, 5)
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return err
		}
		field, ok := fieldToken.(string)
		if !ok {
			return fmt.Errorf("pool config field is not a string")
		}
		if seen[field] {
			return fmt.Errorf("duplicate pool config field %q", field)
		}
		seen[field] = true
		switch field {
		case "sources":
			out.Sources, err = decodeBoundedJSONArray[Source](decoder, maxConfiguredSources)
		case "rules":
			out.Rules, err = decodeBoundedJSONArray[Rule](decoder, maxConfigRules)
		case "groups":
			out.Groups, err = decodeBoundedJSONArray[Group](decoder, maxConfigGroups)
		case "listeners":
			out.Listeners, err = decodeBoundedJSONArray[ListenerBinding](decoder, maxConfigListeners)
		case "check_url":
			err = decoder.Decode(&out.CheckURL)
		default:
			err = skipJSONValue(decoder)
		}
		if err != nil {
			return fmt.Errorf("pool config field %q: %w", field, err)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	*cfg = out
	return nil
}

func decodeBoundedJSONArray[T any](decoder *json.Decoder, limit int) ([]T, error) {
	opening, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if opening == nil {
		return nil, nil
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '[' {
		return nil, fmt.Errorf("expected an array")
	}
	out := make([]T, 0)
	for decoder.More() {
		if len(out) >= limit {
			return nil, fmt.Errorf("array exceeds %d entries", limit)
		}
		var value T
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

func (group *Group) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("group must be an object")
	}
	var out Group
	seen := make(map[string]bool, 7)
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return err
		}
		field, ok := fieldToken.(string)
		if !ok {
			return fmt.Errorf("group field is not a string")
		}
		if seen[field] {
			return fmt.Errorf("duplicate group field %q", field)
		}
		seen[field] = true
		switch field {
		case "id":
			err = decoder.Decode(&out.ID)
		case "name":
			err = decoder.Decode(&out.Name)
		case "strategy":
			err = decoder.Decode(&out.Strategy)
		case "countries":
			out.Countries, err = decodeBoundedJSONStringArray(decoder)
		case "protocols":
			out.Protocols, err = decodeBoundedJSONStringArray(decoder)
		case "sources":
			out.Sources, err = decodeBoundedJSONStringArray(decoder)
		case "nodes":
			out.Nodes, err = decodeBoundedJSONStringArray(decoder)
		default:
			err = fmt.Errorf("unknown group field %q", field)
		}
		if err != nil {
			return fmt.Errorf("group field %q: %w", field, err)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	*group = out
	return nil
}

func decodeBoundedJSONStringArray(decoder *json.Decoder) ([]string, error) {
	opening, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if opening == nil {
		return nil, nil
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '[' {
		return nil, fmt.Errorf("expected an array")
	}
	out := make([]string, 0)
	for decoder.More() {
		if len(out) >= maxConfigListValues {
			return nil, fmt.Errorf("array exceeds %d entries", maxConfigListValues)
		}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if len(value) > maxConfigValueBytes || hasLogControlCharacters(value) {
			return nil, fmt.Errorf("array value exceeds limits or contains control characters")
		}
		out = append(out, value)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

// ConfigStore persists PoolConfig to a JSON file on disk, guarding all
// access with a mutex and writing atomically (temp file + rename).
type ConfigStore struct {
	mu   sync.RWMutex
	path string
	cfg  PoolConfig
}

// ConfigPersistenceError marks a failure after a validated configuration has
// reached the filesystem write/rename/fsync stage. API handlers can use
// errors.As to distinguish an internal durability failure (HTTP 500) from
// validation and business-conflict errors (HTTP 400/409).
type ConfigPersistenceError struct {
	Err error
}

func (e *ConfigPersistenceError) Error() string {
	if e == nil || e.Err == nil {
		return "persist configuration"
	}
	return "persist configuration: " + e.Err.Error()
}

func (e *ConfigPersistenceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func NewConfigStore(dataDir string) (*ConfigStore, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure data dir: %w", err)
	}
	cs := &ConfigStore{path: filepath.Join(dataDir, "pool_config.json")}

	data, err := readPrivateRegularFile(cs.path, maxPoolConfigBytes)
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
	if err := validatePersistedPoolConfig(&cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	cs.cfg = cfg
	migrated := migrateSourceAutoRefresh(&cs.cfg)
	if migrateProxyIPSourceMetadata(&cs.cfg) {
		migrated = true
	}
	if cs.cfg.CheckURL == legacyDefaultCheckURL {
		cs.cfg.CheckURL = defaultCheckURL
		migrated = true
	}
	if migrated {
		if err := cs.writeLocked(); err != nil {
			return nil, fmt.Errorf("persist source metadata migration: %w", err)
		}
	}
	return cs, nil
}

func migrateSourceAutoRefresh(cfg *PoolConfig) bool {
	changed := false
	for i := range cfg.Sources {
		if cfg.Sources[i].autoRefreshMissing {
			cfg.Sources[i].AutoRefreshEnabled = true
			cfg.Sources[i].autoRefreshMissing = false
			changed = true
		}
	}
	return changed
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
	cfg := PoolConfig{
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
	for i := range cfg.Sources {
		cfg.Sources[i].AutoRefreshEnabled = true
	}
	return cfg
}

func (cs *ConfigStore) writeLocked() error {
	return cs.writeConfigLocked(cs.cfg)
}

func (cs *ConfigStore) writeConfigLocked(cfg PoolConfig) error {
	if err := validatePersistedPoolConfig(&cfg); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxPoolConfigBytes {
		return fmt.Errorf("pool config exceeds %d bytes", maxPoolConfigBytes)
	}
	if err := writePrivateFileAtomic(cs.path, data); err != nil {
		return &ConfigPersistenceError{Err: err}
	}
	return nil
}

// mutate runs fn with exclusive access to the config, then persists it.
func (cs *ConfigStore) mutate(fn func(*PoolConfig) error) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	next := clonePoolConfig(cs.cfg)
	if err := fn(&next); err != nil {
		return err
	}
	// Validation also applies harmless canonicalization (trimmed URLs/names and
	// lower-case formats/protocols). Do it on the copy that will become live so
	// memory and the bytes written by writeConfigLocked cannot diverge.
	if err := validatePersistedPoolConfig(&next); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	if err := cs.writeConfigLocked(next); err != nil {
		return err
	}
	cs.cfg = next
	return nil
}

func (cs *ConfigStore) Snapshot() PoolConfig {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return clonePoolConfig(cs.cfg)
}

func (cs *ConfigStore) Sources() []Source {
	return cs.Snapshot().Sources
}

func (cs *ConfigStore) SourceByID(id string) (Source, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	for _, source := range cs.cfg.Sources {
		if source.ID == id {
			return source, true
		}
	}
	return Source{}, false
}

func (cs *ConfigStore) SetSourceAutoRefresh(id string, enabled bool, intervalSeconds int) error {
	if intervalSeconds != 0 && (intervalSeconds < minSourceRefreshIntervalSeconds || intervalSeconds > maxSourceRefreshIntervalSeconds) {
		return fmt.Errorf("refresh interval must be 0 or between %d and %d seconds", minSourceRefreshIntervalSeconds, maxSourceRefreshIntervalSeconds)
	}
	return cs.mutate(func(c *PoolConfig) error {
		for i := range c.Sources {
			if c.Sources[i].ID == id {
				c.Sources[i].AutoRefreshEnabled = enabled
				c.Sources[i].RefreshIntervalSeconds = intervalSeconds
				return nil
			}
		}
		return fmt.Errorf("source not found: %s", id)
	})
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
	validated, err := validateSourceDefinition(s)
	if err != nil {
		return Source{}, err
	}
	s = validated
	s.ID = generateID("src")
	s.Builtin = false
	s.Enabled = true // a source you just added is one you want to use now
	s.AutoRefreshEnabled = true
	err = cs.mutate(func(c *PoolConfig) error {
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
	if err := validateCheckURL(raw); err != nil {
		return err
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

func clonePoolConfig(cfg PoolConfig) PoolConfig {
	out := PoolConfig{
		Sources:   append([]Source(nil), cfg.Sources...),
		Rules:     append([]Rule(nil), cfg.Rules...),
		Groups:    make([]Group, len(cfg.Groups)),
		Listeners: append([]ListenerBinding(nil), cfg.Listeners...),
		CheckURL:  cfg.CheckURL,
	}
	for i, group := range cfg.Groups {
		out.Groups[i] = cloneGroup(group)
	}
	return out
}

func cloneGroup(group Group) Group {
	group.Countries = append([]string(nil), group.Countries...)
	group.Protocols = append([]string(nil), group.Protocols...)
	group.Sources = append([]string(nil), group.Sources...)
	group.Nodes = append([]string(nil), group.Nodes...)
	return group
}

func validatePersistedPoolConfig(cfg *PoolConfig) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if len(cfg.Sources) > maxConfiguredSources {
		return fmt.Errorf("source count %d exceeds limit %d", len(cfg.Sources), maxConfiguredSources)
	}
	seenIDs := make(map[string]struct{}, len(cfg.Sources))
	for i, source := range cfg.Sources {
		if strings.TrimSpace(source.ID) == "" || source.ID != strings.TrimSpace(source.ID) || len(source.ID) > maxConfigValueBytes || hasLogControlCharacters(source.ID) {
			return fmt.Errorf("source %d has an invalid id", i)
		}
		if _, exists := seenIDs[source.ID]; exists {
			return fmt.Errorf("source %d has duplicate id %q", i, source.ID)
		}
		seenIDs[source.ID] = struct{}{}
		normalized, err := validateSourceDefinition(source)
		if err != nil {
			return fmt.Errorf("source %d: %w", i, err)
		}
		cfg.Sources[i] = normalized
		if len(source.Note) > maxConfigValueBytes || hasLogControlCharacters(source.Note) {
			return fmt.Errorf("source %d note exceeds limits or contains control characters", i)
		}
	}
	if len(cfg.Rules) > maxConfigRules {
		return fmt.Errorf("rule count %d exceeds limit %d", len(cfg.Rules), maxConfigRules)
	}
	for i, rule := range cfg.Rules {
		for _, value := range []string{rule.ID, rule.Type, rule.Value, rule.Group} {
			if len(value) > maxConfigValueBytes || hasLogControlCharacters(value) {
				return fmt.Errorf("rule %d contains an oversized or control-character value", i)
			}
		}
	}
	if len(cfg.Groups) > maxConfigGroups {
		return fmt.Errorf("group count %d exceeds limit %d", len(cfg.Groups), maxConfigGroups)
	}
	for i, group := range cfg.Groups {
		for _, value := range []string{group.ID, group.Name, group.Strategy} {
			if len(value) > maxConfigValueBytes || hasLogControlCharacters(value) {
				return fmt.Errorf("group %d contains an oversized or control-character value", i)
			}
		}
		for _, values := range [][]string{group.Countries, group.Protocols, group.Sources, group.Nodes} {
			if len(values) > maxConfigListValues {
				return fmt.Errorf("group %d list exceeds %d entries", i, maxConfigListValues)
			}
			for _, value := range values {
				if len(value) > maxConfigValueBytes || hasLogControlCharacters(value) {
					return fmt.Errorf("group %d list contains an oversized or control-character value", i)
				}
			}
		}
	}
	if len(cfg.Listeners) > maxConfigListeners {
		return fmt.Errorf("listener count %d exceeds limit %d", len(cfg.Listeners), maxConfigListeners)
	}
	seenListenerIDs := make(map[string]struct{}, len(cfg.Listeners))
	seenListenerPorts := make(map[int]struct{}, len(cfg.Listeners))
	for i, listener := range cfg.Listeners {
		normalized, err := validateListenerBinding(cfg, listener, false)
		if err != nil {
			return fmt.Errorf("listener %d: %w", i, err)
		}
		if _, exists := seenListenerIDs[normalized.ID]; exists {
			return fmt.Errorf("listener %d has duplicate id %q", i, normalized.ID)
		}
		if _, exists := seenListenerPorts[normalized.Port]; exists {
			return fmt.Errorf("listener %d has duplicate port %d", i, normalized.Port)
		}
		seenListenerIDs[normalized.ID] = struct{}{}
		seenListenerPorts[normalized.Port] = struct{}{}
		cfg.Listeners[i] = normalized
	}
	if cfg.CheckURL != "" {
		cfg.CheckURL = strings.TrimSpace(cfg.CheckURL)
		if err := validateCheckURL(cfg.CheckURL); err != nil {
			return fmt.Errorf("check url: %w", err)
		}
	}
	return nil
}

// validateListenerBinding normalizes and validates a persisted auxiliary
// listener. Fixed node keys are intentionally not checked against the live
// pool: configuration may be restored before that node is refreshed.
func validateListenerBinding(cfg *PoolConfig, listener ListenerBinding, allowEmptyID bool) (ListenerBinding, error) {
	listener.ID = strings.TrimSpace(listener.ID)
	listener.Name = strings.TrimSpace(listener.Name)
	listener.Mode = strings.ToLower(strings.TrimSpace(listener.Mode))
	listener.Group = canonicalReservedGroup(listener.Group)
	listener.NodeKey = strings.TrimSpace(listener.NodeKey)
	for _, value := range []string{listener.ID, listener.Name, listener.Mode, listener.Group, listener.NodeKey} {
		if len(value) > maxConfigValueBytes || hasLogControlCharacters(value) {
			return ListenerBinding{}, fmt.Errorf("contains an oversized or control-character value")
		}
	}
	if (!allowEmptyID && listener.ID == "") || listener.ID != strings.TrimSpace(listener.ID) {
		return ListenerBinding{}, fmt.Errorf("id is required")
	}
	if listener.Name == "" {
		return ListenerBinding{}, fmt.Errorf("name is required")
	}
	if listener.Port < 1 || listener.Port > 65535 {
		return ListenerBinding{}, fmt.Errorf("port must be between 1 and 65535")
	}
	if !validListenerMode(listener.Mode) {
		return ListenerBinding{}, fmt.Errorf("unknown listener mode: %q", listener.Mode)
	}
	switch listener.Mode {
	case ListenerModeGroup:
		if listener.Group == "" {
			return ListenerBinding{}, fmt.Errorf("group is required for group mode")
		}
		if code, ok := parseCountryGroup(listener.Group); ok {
			if !validCountryGroupCode(code) {
				return ListenerBinding{}, fmt.Errorf("country group must use a two-letter ASCII country code")
			}
			listener.Group = countryGroupPrefix + strings.ToUpper(code)
		}
		if !routingTargetExists(cfg, listener.Group) {
			return ListenerBinding{}, fmt.Errorf("listener group does not exist: %s", listener.Group)
		}
		if listener.NodeKey != "" {
			return ListenerBinding{}, fmt.Errorf("node_key is only allowed for fixed mode")
		}
	case ListenerModeFixed:
		if listener.NodeKey == "" {
			return ListenerBinding{}, fmt.Errorf("node_key is required for fixed mode")
		}
		if err := validateListenerNodeKey(listener.NodeKey); err != nil {
			return ListenerBinding{}, err
		}
		if listener.Group != "" {
			return ListenerBinding{}, fmt.Errorf("group is only allowed for group mode")
		}
	case ListenerModeRules:
		if listener.Group != "" || listener.NodeKey != "" {
			return ListenerBinding{}, fmt.Errorf("group and node_key must be empty for rules mode")
		}
	}
	return listener, nil
}

func validateListenerNodeKey(key string) error {
	u, err := url.Parse(key)
	if err != nil || !isForwardingProtocol(strings.ToLower(u.Scheme)) || u.User != nil || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("node_key must be a protocol-aware proxy key")
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("node_key must include a port between 1 and 65535")
	}
	return nil
}

func validateSourceDefinition(source Source) (Source, error) {
	source.Name = strings.TrimSpace(source.Name)
	source.URL = strings.TrimSpace(source.URL)
	source.Format = strings.ToLower(strings.TrimSpace(source.Format))
	source.Protocol = strings.ToLower(strings.TrimSpace(source.Protocol))
	if source.RefreshIntervalSeconds != 0 && (source.RefreshIntervalSeconds < minSourceRefreshIntervalSeconds || source.RefreshIntervalSeconds > maxSourceRefreshIntervalSeconds) {
		return Source{}, fmt.Errorf("refresh interval must be 0 or between %d and %d seconds", minSourceRefreshIntervalSeconds, maxSourceRefreshIntervalSeconds)
	}
	if source.Name == "" || source.URL == "" {
		return Source{}, fmt.Errorf("name and url are required")
	}
	if len(source.Name) > maxSourceNameBytes || hasLogControlCharacters(source.Name) {
		return Source{}, fmt.Errorf("source name must be at most %d bytes and contain no control characters", maxSourceNameBytes)
	}
	if len(source.URL) > maxSourceURLBytes {
		return Source{}, fmt.Errorf("source url exceeds %d bytes", maxSourceURLBytes)
	}
	if _, err := validateSourceURL(source.URL, source.AllowPrivate); err != nil {
		return Source{}, err
	}
	switch source.Format {
	case FormatTextRegex, FormatEDTJSON, FormatProxyIPJSON:
	case FormatPlainList, FormatJSONArray:
		if !isForwardingProtocol(source.Protocol) {
			return Source{}, fmt.Errorf("format %q requires protocol socks5, http, or https", source.Format)
		}
	default:
		return Source{}, fmt.Errorf("unknown format: %q", source.Format)
	}
	return source, nil
}

func validateCheckURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("url is required")
	}
	if len(raw) > maxSourceURLBytes {
		return fmt.Errorf("url exceeds %d bytes", maxSourceURLBytes)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("invalid url: must be a full http:// or https:// address")
	}
	if u.User != nil {
		return fmt.Errorf("invalid url: embedded username/password is not allowed")
	}
	if u.Fragment != "" {
		return fmt.Errorf("invalid url: fragments are not allowed")
	}
	if port := u.Port(); port != "" {
		value, portErr := strconv.ParseUint(port, 10, 16)
		if portErr != nil || value == 0 {
			return fmt.Errorf("invalid url: port must be between 1 and 65535")
		}
	}
	return nil
}

// Listeners returns a defensive copy of all persisted auxiliary listener
// bindings.
func (cs *ConfigStore) Listeners() []ListenerBinding {
	return cloneListeners(cs.Snapshot().Listeners)
}

// AddListener persists a new auxiliary listener binding. The ID is generated
// server-side; callers receive the canonicalized binding back.
func (cs *ConfigStore) AddListener(binding ListenerBinding) (ListenerBinding, error) {
	binding.ID = generateID("listener")
	normalized, err := validateListenerBindingForAdd(binding)
	if err != nil {
		return ListenerBinding{}, err
	}
	normalized.ID = binding.ID
	err = cs.mutate(func(c *PoolConfig) error {
		if len(c.Listeners) >= maxConfigListeners {
			return fmt.Errorf("listener limit reached: at most %d listeners are allowed", maxConfigListeners)
		}
		if listenerPortTakenLocked(c, normalized.Port) {
			return fmt.Errorf("port %d is already in use by another listener", normalized.Port)
		}
		finalized, err := validateListenerBinding(c, normalized, false)
		if err != nil {
			return err
		}
		c.Listeners = append(c.Listeners, finalized)
		return nil
	})
	if err != nil {
		return ListenerBinding{}, err
	}
	return cs.lookupListener(normalized.ID, normalized.Port)
}

// validateListenerBindingForAdd normalizes an incoming binding before the
// store mutation runs, so structural failures return before any write.
func validateListenerBindingForAdd(binding ListenerBinding) (ListenerBinding, error) {
	binding.Name = strings.TrimSpace(binding.Name)
	binding.Mode = strings.ToLower(strings.TrimSpace(binding.Mode))
	binding.Group = canonicalReservedGroup(binding.Group)
	binding.NodeKey = strings.TrimSpace(binding.NodeKey)
	if binding.Name == "" {
		return ListenerBinding{}, fmt.Errorf("name is required")
	}
	if binding.Port < 1 || binding.Port > 65535 {
		return ListenerBinding{}, fmt.Errorf("port must be between 1 and 65535")
	}
	if !validListenerMode(binding.Mode) {
		return ListenerBinding{}, fmt.Errorf("unknown listener mode: %q", binding.Mode)
	}
	for _, value := range []string{binding.Name, binding.Group, binding.NodeKey} {
		if len(value) > maxConfigValueBytes || hasLogControlCharacters(value) {
			return ListenerBinding{}, fmt.Errorf("contains an oversized or control-character value")
		}
	}
	return binding, nil
}

// UpdateListener replaces an existing binding by ID. Port collisions with
// other listeners are rejected. group-mode references are re-validated against
// the current group set so an update can never strand a listener on a group
// that no longer exists.
func (cs *ConfigStore) UpdateListener(binding ListenerBinding) (ListenerBinding, error) {
	binding.ID = strings.TrimSpace(binding.ID)
	if binding.ID == "" {
		return ListenerBinding{}, fmt.Errorf("id is required")
	}
	normalized, err := validateListenerBindingForAdd(binding)
	if err != nil {
		return ListenerBinding{}, err
	}
	normalized.ID = binding.ID
	err = cs.mutate(func(c *PoolConfig) error {
		idx := -1
		for i, existing := range c.Listeners {
			if existing.ID == normalized.ID {
				idx = i
				break
			}
		}
		if idx == -1 {
			return fmt.Errorf("listener not found: %s", normalized.ID)
		}
		for i, existing := range c.Listeners {
			if i == idx {
				continue
			}
			if existing.Port == normalized.Port {
				return fmt.Errorf("port %d is already in use by another listener", normalized.Port)
			}
		}
		finalized, err := validateListenerBinding(c, normalized, false)
		if err != nil {
			return err
		}
		c.Listeners[idx] = finalized
		return nil
	})
	if err != nil {
		return ListenerBinding{}, err
	}
	return cs.lookupListener(normalized.ID, normalized.Port)
}

// DeleteListener removes the binding with the given ID. Missing IDs are an
// error so callers can distinguish a no-op from a successful delete.
func (cs *ConfigStore) DeleteListener(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("id is required")
	}
	return cs.mutate(func(c *PoolConfig) error {
		for i, existing := range c.Listeners {
			if existing.ID == id {
				c.Listeners = append(c.Listeners[:i], c.Listeners[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("listener not found: %s", id)
	})
}

// ReplaceListeners atomically swaps the entire listener set. It is the
// rollback path for the listener manager: a failed hot-reload restores the
// previous bindings in one write. group references must still resolve against
// the current group set.
func (cs *ConfigStore) ReplaceListeners(bindings []ListenerBinding) error {
	if len(bindings) > maxConfigListeners {
		return fmt.Errorf("listener limit reached: at most %d listeners are allowed", maxConfigListeners)
	}
	seenIDs := make(map[string]struct{}, len(bindings))
	seenPorts := make(map[int]struct{}, len(bindings))
	normalized := make([]ListenerBinding, 0, len(bindings))
	for _, binding := range bindings {
		binding.ID = strings.TrimSpace(binding.ID)
		// Allow empty IDs here only if every binding carries one; the
		// manager always supplies stable IDs, so require them.
		binding.Name = strings.TrimSpace(binding.Name)
		binding.Mode = strings.ToLower(strings.TrimSpace(binding.Mode))
		binding.Group = canonicalReservedGroup(binding.Group)
		binding.NodeKey = strings.TrimSpace(binding.NodeKey)
		if binding.ID == "" {
			return fmt.Errorf("listener id is required")
		}
		if _, exists := seenIDs[binding.ID]; exists {
			return fmt.Errorf("duplicate listener id %q", binding.ID)
		}
		if _, exists := seenPorts[binding.Port]; exists {
			return fmt.Errorf("duplicate listener port %d", binding.Port)
		}
		seenIDs[binding.ID] = struct{}{}
		seenPorts[binding.Port] = struct{}{}
		normalized = append(normalized, binding)
	}
	return cs.mutate(func(c *PoolConfig) error {
		for i, binding := range normalized {
			finalized, err := validateListenerBinding(c, binding, false)
			if err != nil {
				return fmt.Errorf("listener %d: %w", i, err)
			}
			normalized[i] = finalized
		}
		c.Listeners = normalized
		return nil
	})
}

func (cs *ConfigStore) lookupListener(id string, port int) (ListenerBinding, error) {
	for _, existing := range cs.Listeners() {
		if existing.ID == id {
			return existing, nil
		}
	}
	return ListenerBinding{}, fmt.Errorf("listener not found after write: %s (port %d)", id, port)
}

func listenerPortTakenLocked(c *PoolConfig, port int) bool {
	for _, existing := range c.Listeners {
		if existing.Port == port {
			return true
		}
	}
	return false
}

func cloneListeners(bindings []ListenerBinding) []ListenerBinding {
	return append([]ListenerBinding(nil), bindings...)
}

// readPrivateRegularFile treats persisted state as untrusted input. It rejects
// symlinks and special files, re-checks the opened descriptor to close the
// Lstat/Open race, applies a hard byte cap, and repairs legacy permissive modes.
func readPrivateRegularFile(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if info.Size() < 0 || info.Size() > maxBytes {
		return nil, fmt.Errorf("%s size %d exceeds limit %d", filepath.Base(path), info.Size(), maxBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened %s: %w", filepath.Base(path), err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("%s changed while opening or is not a regular file", filepath.Base(path))
	}
	if openedInfo.Size() < 0 || openedInfo.Size() > maxBytes {
		return nil, fmt.Errorf("opened %s size %d exceeds limit %d", filepath.Base(path), openedInfo.Size(), maxBytes)
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("secure %s permissions: %w", filepath.Base(path), err)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", filepath.Base(path), maxBytes)
	}
	return data, nil
}

func writePrivateFileAtomic(path string, data []byte) (returnErr error) {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("persisted state path must not be empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		if returnErr != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := io.Copy(temp, bytes.NewReader(data)); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func generateID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), rand.Intn(1_000_000))
}

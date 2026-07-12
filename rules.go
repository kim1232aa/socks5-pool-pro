package main

import (
	"fmt"
	"net"
	"strings"
)

// Routing rule types, modeled after Clash-style rule matching.
const (
	RuleDomain        = "DOMAIN"
	RuleDomainSuffix  = "DOMAIN-SUFFIX"
	RuleDomainKeyword = "DOMAIN-KEYWORD"
	RuleIPCIDR        = "IP-CIDR"
	RuleGeosite       = "GEOSITE" // value is a bundled category: "cn" or "gfw"
	RuleMatch         = "MATCH"
)

// Rule is one ordered entry in the routing table: the first rule whose
// pattern matches a connection's target host decides which Group (or
// DIRECT) handles it. A trailing MATCH rule is the catch-all fallback.
type Rule struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Value string `json:"value,omitempty"`
	Group string `json:"group"`
}

func validRuleType(t string) bool {
	switch t {
	case RuleDomain, RuleDomainSuffix, RuleDomainKeyword, RuleIPCIDR, RuleGeosite, RuleMatch:
		return true
	}
	return false
}

// MatchGroup walks rules in order and returns the Group name of the first
// match. If nothing matches (e.g. the persisted MATCH rule was somehow
// removed), it falls back to GroupAny so traffic is never silently
// dropped.
func MatchGroup(rules []Rule, host string) string {
	// DNS names with and without the absolute-name trailing dot identify the
	// same host. Canonicalize once before every rule type so an input such as
	// "example.com." cannot bypass DOMAIN/DOMAIN-SUFFIX while GEOSITE happens
	// to match it differently.
	lowerHost := canonicalRoutingHost(host)
	ip := net.ParseIP(lowerHost)

	for _, r := range rules {
		switch r.Type {
		case RuleDomain:
			if lowerHost == canonicalRoutingHost(r.Value) {
				return r.Group
			}
		case RuleDomainSuffix:
			v := strings.TrimPrefix(canonicalRoutingHost(r.Value), ".")
			if lowerHost == v || strings.HasSuffix(lowerHost, "."+v) {
				return r.Group
			}
		case RuleDomainKeyword:
			if r.Value != "" && strings.Contains(lowerHost, strings.ToLower(r.Value)) {
				return r.Group
			}
		case RuleIPCIDR:
			if ip == nil {
				continue
			}
			if _, cidr, err := net.ParseCIDR(r.Value); err == nil && cidr.Contains(ip) {
				return r.Group
			}
		case RuleGeosite:
			if ip == nil && geositeMatch(r.Value, lowerHost) {
				return r.Group
			}
		case RuleMatch:
			return r.Group
		}
	}
	return GroupAny
}

func canonicalRoutingHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func (cs *ConfigStore) Rules() []Rule {
	return cs.Snapshot().Rules
}

// AddRule inserts a new rule immediately before the trailing MATCH rule
// (rules are evaluated top to bottom, so MATCH must stay last).
func (cs *ConfigStore) AddRule(r Rule) (Rule, error) {
	if !validRuleType(r.Type) {
		return Rule{}, fmt.Errorf("unknown rule type: %q", r.Type)
	}
	if r.Type == RuleMatch {
		return Rule{}, fmt.Errorf("use the default-group action to edit the MATCH rule")
	}
	if r.Value == "" {
		return Rule{}, fmt.Errorf("value is required for rule type %q", r.Type)
	}
	if r.Type == RuleIPCIDR {
		if _, _, err := net.ParseCIDR(r.Value); err != nil {
			return Rule{}, fmt.Errorf("invalid CIDR: %w", err)
		}
	}
	if r.Type == RuleGeosite && !validGeositeCategory(r.Value) {
		return Rule{}, fmt.Errorf("GEOSITE value must be %q or %q", GeositeCN, GeositeGFW)
	}
	if r.Group == "" {
		return Rule{}, fmt.Errorf("group is required")
	}
	r.Group = canonicalReservedGroup(r.Group)
	// Normalize a dynamic country target ("country:jp" -> "COUNTRY:JP") so it
	// matches the uppercase ISO codes stored on nodes. Any other group name
	// is passed through and resolved (or gracefully fallen back) at dial time.
	if cc, ok := parseCountryGroup(r.Group); ok {
		if !validCountryGroupCode(cc) {
			return Rule{}, fmt.Errorf("country group must use a two-letter ASCII country code")
		}
		r.Group = countryGroupPrefix + strings.ToUpper(cc)
	}
	r.ID = generateID("rule")

	err := cs.mutate(func(c *PoolConfig) error {
		if !routingTargetExists(c, r.Group) {
			return fmt.Errorf("routing target does not exist: %s", r.Group)
		}
		insertAt := len(c.Rules)
		for i, existing := range c.Rules {
			if existing.Type == RuleMatch {
				insertAt = i
				break
			}
		}
		head := append([]Rule{}, c.Rules[:insertAt]...)
		tail := append([]Rule{}, c.Rules[insertAt:]...)
		c.Rules = append(append(head, r), tail...)
		return nil
	})
	return r, err
}

func (cs *ConfigStore) DeleteRule(id string) error {
	return cs.mutate(func(c *PoolConfig) error {
		for i, r := range c.Rules {
			if r.ID == id {
				if r.Type == RuleMatch {
					return fmt.Errorf("cannot delete the trailing MATCH rule; edit its target group instead")
				}
				c.Rules = append(c.Rules[:i], c.Rules[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("rule not found: %s", id)
	})
}

// MoveRule shifts the rule at id by delta positions (-1 = up, +1 = down).
// No-ops at the boundary; refuses to disturb the trailing MATCH rule.
func (cs *ConfigStore) MoveRule(id string, delta int) error {
	return cs.mutate(func(c *PoolConfig) error {
		idx := -1
		for i, r := range c.Rules {
			if r.ID == id {
				idx = i
				break
			}
		}
		if idx == -1 {
			return fmt.Errorf("rule not found: %s", id)
		}
		newIdx := idx + delta
		if newIdx < 0 || newIdx >= len(c.Rules) {
			return nil
		}
		if c.Rules[idx].Type == RuleMatch || c.Rules[newIdx].Type == RuleMatch {
			return fmt.Errorf("cannot reorder the trailing MATCH rule")
		}
		c.Rules[idx], c.Rules[newIdx] = c.Rules[newIdx], c.Rules[idx]
		return nil
	})
}

// SetDefaultGroup updates (or creates, if somehow missing) the trailing
// MATCH rule's target group - i.e. the fallback for any traffic that
// doesn't hit a more specific rule.
func (cs *ConfigStore) SetDefaultGroup(group string) error {
	if group == "" {
		return fmt.Errorf("group is required")
	}
	group = canonicalReservedGroup(group)
	// Same normalization AddRule applies - without it, a non-canonically-
	// cased "country:jp" passed directly via the API wouldn't match
	// parseCountryGroup's case-sensitive "COUNTRY:" prefix check, and the
	// default route would silently fall back to the entire pool instead of
	// the intended country.
	if cc, ok := parseCountryGroup(group); ok {
		if !validCountryGroupCode(cc) {
			return fmt.Errorf("country group must use a two-letter ASCII country code")
		}
		group = countryGroupPrefix + strings.ToUpper(cc)
	}
	return cs.mutate(func(c *PoolConfig) error {
		if !routingTargetExists(c, group) {
			return fmt.Errorf("routing target does not exist: %s", group)
		}
		for i, r := range c.Rules {
			if r.Type == RuleMatch {
				c.Rules[i].Group = group
				return nil
			}
		}
		c.Rules = append(c.Rules, Rule{ID: generateID("rule"), Type: RuleMatch, Group: group})
		return nil
	})
}

// InstallGFWPreset replaces the routing table with a GFW-style ruleset:
// LAN + mainland-China domains go DIRECT (bypass the proxy), and everything
// else is proxied via ANY. This is the common "domestic direct, foreign
// proxied" setup. Existing custom rules are replaced.
func (cs *ConfigStore) InstallGFWPreset() error {
	return cs.mutate(func(c *PoolConfig) error {
		c.Rules = []Rule{
			{ID: generateID("rule"), Type: RuleIPCIDR, Value: "127.0.0.0/8", Group: GroupDirect},
			{ID: generateID("rule"), Type: RuleIPCIDR, Value: "10.0.0.0/8", Group: GroupDirect},
			{ID: generateID("rule"), Type: RuleIPCIDR, Value: "172.16.0.0/12", Group: GroupDirect},
			{ID: generateID("rule"), Type: RuleIPCIDR, Value: "192.168.0.0/16", Group: GroupDirect},
			{ID: generateID("rule"), Type: RuleGeosite, Value: GeositeCN, Group: GroupDirect},
			{ID: generateID("rule"), Type: RuleGeosite, Value: GeositeGFW, Group: GroupAny},
			{ID: generateID("rule"), Type: RuleMatch, Group: GroupAny},
		}
		return nil
	})
}

func (cs *ConfigStore) Groups() []Group {
	return cs.Snapshot().Groups
}

// AddGroup creates a new named, filtered subset of the pool with its own
// load-balancing strategy.
func (cs *ConfigStore) AddGroup(g Group) (Group, error) {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" {
		return Group{}, fmt.Errorf("name is required")
	}
	if strings.EqualFold(g.Name, GroupAny) || strings.EqualFold(g.Name, GroupDirect) {
		return Group{}, fmt.Errorf("%q is a reserved group name", g.Name)
	}
	if len(g.Name) >= len(countryGroupPrefix) && strings.EqualFold(g.Name[:len(countryGroupPrefix)], countryGroupPrefix) {
		return Group{}, fmt.Errorf("%q uses the reserved dynamic country-group namespace", g.Name)
	}
	switch g.Strategy {
	case StrategySticky, StrategyRoundRobin, StrategyRandom, StrategyLatency, StrategySpeed, StrategyScore:
	case "":
		g.Strategy = StrategySticky
	default:
		return Group{}, fmt.Errorf("unknown strategy: %q", g.Strategy)
	}
	g.ID = generateID("grp")
	g = cloneGroup(g)

	err := cs.mutate(func(c *PoolConfig) error {
		for _, existing := range c.Groups {
			if strings.EqualFold(existing.Name, g.Name) {
				return fmt.Errorf("group already exists: %s", g.Name)
			}
		}
		c.Groups = append(c.Groups, cloneGroup(g))
		return nil
	})
	return cloneGroup(g), err
}

// SetGroupStrategy changes just the load-balancing strategy of an
// existing group. Filters (countries/protocols/sources) are immutable
// after creation, same as the name - to change them, delete and recreate
// the group.
func (cs *ConfigStore) SetGroupStrategy(id, strategy string) error {
	switch strategy {
	case StrategySticky, StrategyRoundRobin, StrategyRandom, StrategyLatency, StrategySpeed, StrategyScore:
	default:
		return fmt.Errorf("unknown strategy: %q", strategy)
	}
	return cs.mutate(func(c *PoolConfig) error {
		for i, g := range c.Groups {
			if g.ID == id {
				c.Groups[i].Strategy = strategy
				return nil
			}
		}
		return fmt.Errorf("group not found: %s", id)
	})
}

func (cs *ConfigStore) DeleteGroup(id string) error {
	return cs.mutate(func(c *PoolConfig) error {
		for i, g := range c.Groups {
			if g.ID == id {
				for _, rule := range c.Rules {
					if strings.EqualFold(rule.Group, g.Name) {
						return fmt.Errorf("group %q is still referenced by routing rule %q", g.Name, rule.ID)
					}
				}
				c.Groups = append(c.Groups[:i], c.Groups[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("group not found: %s", id)
	})
}

func validCountryGroupCode(code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != 2 {
		return false
	}
	for i := 0; i < len(code); i++ {
		b := code[i]
		if b >= 'a' && b <= 'z' {
			b -= 'a' - 'A'
		}
		if b < 'A' || b > 'Z' {
			return false
		}
	}
	return true
}

func routingTargetExists(config *PoolConfig, target string) bool {
	if target == GroupAny || target == GroupDirect {
		return true
	}
	if code, ok := parseCountryGroup(target); ok {
		return validCountryGroupCode(code)
	}
	for _, group := range config.Groups {
		if strings.EqualFold(group.Name, target) {
			return true
		}
	}
	return false
}

func canonicalReservedGroup(group string) string {
	group = strings.TrimSpace(group)
	if strings.EqualFold(group, GroupAny) {
		return GroupAny
	}
	if strings.EqualFold(group, GroupDirect) {
		return GroupDirect
	}
	return group
}

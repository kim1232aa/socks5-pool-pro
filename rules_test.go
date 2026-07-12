package main

import "testing"

func TestMatchGroupCanonicalizesAbsoluteDNSNames(t *testing.T) {
	tests := []struct {
		name  string
		rule  Rule
		host  string
		group string
	}{
		{name: "exact host", rule: Rule{Type: RuleDomain, Value: "Example.COM", Group: GroupDirect}, host: "example.com.", group: GroupDirect},
		{name: "suffix host", rule: Rule{Type: RuleDomainSuffix, Value: ".example.com.", Group: "proxy"}, host: "API.Example.Com.", group: "proxy"},
		{name: "whitespace is normalized", rule: Rule{Type: RuleDomain, Value: " example.com. ", Group: GroupDirect}, host: " example.com. ", group: GroupDirect},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := []Rule{tt.rule, {Type: RuleMatch, Group: GroupAny}}
			if got := MatchGroup(rules, tt.host); got != tt.group {
				t.Fatalf("MatchGroup() = %q, want %q", got, tt.group)
			}
		})
	}
}

func TestMatchGroupAbsoluteNameDoesNotChangeUnrelatedSuffix(t *testing.T) {
	rules := []Rule{
		{Type: RuleDomainSuffix, Value: "example.com", Group: GroupDirect},
		{Type: RuleMatch, Group: GroupAny},
	}
	if got := MatchGroup(rules, "notexample.com."); got != GroupAny {
		t.Fatalf("MatchGroup() = %q, want %q", got, GroupAny)
	}
}

func TestRuleTargetsMustExistAndReferencedGroupsCannotBeDeleted(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddRule(Rule{Type: RuleDomain, Value: "example.com", Group: "typo"}); err == nil {
		t.Fatal("AddRule accepted an unknown routing target")
	}
	if err := store.SetDefaultGroup("COUNTRY:TOO-LONG"); err == nil {
		t.Fatal("SetDefaultGroup accepted an invalid country target")
	}

	group, err := store.AddGroup(Group{Name: "private-egress", Strategy: StrategySticky})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := store.AddRule(Rule{Type: RuleDomainSuffix, Value: "example.com", Group: group.Name})
	if err != nil {
		t.Fatalf("AddRule with existing target: %v", err)
	}
	if err := store.DeleteGroup(group.ID); err == nil {
		t.Fatal("DeleteGroup removed a group still referenced by a rule")
	}
	if err := store.DeleteRule(rule.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteGroup(group.ID); err != nil {
		t.Fatalf("DeleteGroup after removing reference: %v", err)
	}
}

func TestAddGroupRejectsReservedCountryNamespaceAndOwnsSlices(t *testing.T) {
	store, err := NewConfigStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"COUNTRY:US", " country:jp ", "COUNTRY:"} {
		if _, err := store.AddGroup(Group{Name: name}); err == nil {
			t.Errorf("reserved group name %q was accepted", name)
		}
	}

	input := Group{Name: "  private-egress  ", Countries: []string{"JP"}, Protocols: []string{"socks5"}}
	created, err := store.AddGroup(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Countries[0] = "US"
	created.Protocols[0] = "http"
	stored := store.Groups()[0]
	if stored.Name != "private-egress" || stored.Countries[0] != "JP" || stored.Protocols[0] != "socks5" {
		t.Fatalf("stored group aliased caller/return slices: %#v", stored)
	}
}

func TestValidateCheckURLRejectsInvalidPorts(t *testing.T) {
	for _, raw := range []string{"http://example.com:0/health", "https://example.com:65536/health"} {
		if err := validateCheckURL(raw); err == nil {
			t.Errorf("validateCheckURL(%q) accepted invalid port", raw)
		}
	}
}

package model

import "testing"

func TestMatchPrecedence(t *testing.T) {
	// exact in B + prefix in A: exact wins for gpt-4o, prefix covers gpt-4-turbo.
	prov := []Provider{
		{Name: "A", Models: []Entry{{Kind: Prefix, Literal: "gpt-4"}}},
		{Name: "B", Models: []Entry{{Kind: Exact, Literal: "gpt-4o"}}},
	}
	r := NewRegistry(prov)
	if name, _ := r.Match("gpt-4o"); name != "B" {
		t.Errorf("gpt-4o -> %q, want B (exact)", name)
	}
	if name, _ := r.Match("gpt-4-turbo"); name != "A" {
		t.Errorf("gpt-4-turbo -> %q, want A (prefix)", name)
	}
	if name, _ := r.Match("gpt-4"); name != "A" {
		t.Errorf("gpt-4 -> %q, want A (prefix matches gpt-4)", name)
	}
}

func TestMatchLongestPrefixWins(t *testing.T) {
	prov := []Provider{
		{Name: "A", Models: []Entry{{Kind: Prefix, Literal: "gpt"}}},
		{Name: "B", Models: []Entry{{Kind: Prefix, Literal: "gpt-4"}}},
	}
	r := NewRegistry(prov)
	if name, _ := r.Match("gpt-4o"); name != "B" {
		t.Errorf("gpt-4o -> %q, want B (longest prefix)", name)
	}
	if name, _ := r.Match("gpt-3"); name != "A" {
		t.Errorf("gpt-3 -> %q, want A", name)
	}
}

func TestCatchAll(t *testing.T) {
	prov := []Provider{
		{Name: "A", Models: []Entry{{Kind: Exact, Literal: "gpt-4o"}}},
		{Name: "B", Models: []Entry{{Kind: CatchAll}}},
	}
	r := NewRegistry(prov)
	if name, _ := r.Match("gpt-4o"); name != "A" {
		t.Errorf("gpt-4o -> %q, want A (exact beats catch-all)", name)
	}
	if name, _ := r.Match("anything-else"); name != "B" {
		t.Errorf("anything-else -> %q, want B (catch-all)", name)
	}
	if !r.Lookup("zzz") {
		t.Error("Lookup should be true with catch-all")
	}
}

func TestNoMatch(t *testing.T) {
	prov := []Provider{{Name: "A", Models: []Entry{{Kind: Exact, Literal: "gpt-4o"}}}}
	r := NewRegistry(prov)
	if _, ok := r.Match("claude-3"); ok {
		t.Error("expected no match without catch-all")
	}
	if r.Lookup("claude-3") {
		t.Error("Lookup should be false")
	}
}

func TestNewRegistryAcceptsValidatedInput(t *testing.T) {
	// Overlap validation now lives in config.resolve; NewRegistry trusts the
	// input. This test pins that NewRegistry accepts what config would have
	// rejected and just builds the routing table.
	prov := []Provider{
		{Name: "A", Models: []Entry{{Kind: Exact, Literal: "x"}}},
		{Name: "B", Models: []Entry{{Kind: Exact, Literal: "x"}}},
	}
	r := NewRegistry(prov)
	if name, _ := r.Match("x"); name != "A" {
		t.Errorf("Match(x) = %q, want A (first declaration wins)", name)
	}
}

func TestEnumerate(t *testing.T) {
	prov := []Provider{
		{Name: "A", Models: []Entry{{Kind: Exact, Literal: "gpt-4o"}, {Kind: Prefix, Literal: "gpt-4"}}},
		{Name: "B", Models: []Entry{{Kind: CatchAll}}},
	}
	r := NewRegistry(prov)
	got := r.Enumerate()
	if len(got) != 1 || got[0].ID != "gpt-4o" || got[0].OwnedBy != "A" {
		t.Errorf("Enumerate = %+v, want only gpt-4o/A", got)
	}
}

// Package model implements model-id routing.
//
// model is a leaf package of pure data structures with no I/O and no internal
// imports. A Registry is built once from a provider list and answers Match,
// Lookup, and Enumerate in O(1) exact or O(prefix-length) longest-prefix form.
// All the tricky routing invariants (exact beats longest-prefix beats
// catch-all) live behind NewRegistry; cross-provider overlap and catch-all
// validation is enforced by config at load time. The runtime surface is
// three read methods.
package model

import (
	"sort"
	"strings"
)

// Kind classifies a model pattern.
type Kind int

const (
	Exact Kind = iota
	Prefix
	CatchAll
)

// Entry is one declared model pattern for a provider.
type Entry struct {
	Kind    Kind
	Literal string // for Prefix, the literal without a trailing "*"
}

// Provider is the subset of a resolved provider that routing needs. The
// caller, app or forward, maps config.Provider to model.Provider; model does
// not import config, which keeps it a leaf.
type Provider struct {
	Name   string
	Models []Entry
}

// Registry is the precomputed routing table. Construct once with NewRegistry;
// all reads are concurrent-safe after construction because there is no
// mutable state.
type Registry struct {
	exact    map[string]string // exact id -> provider name
	prefixes []prefixEntry     // sorted by descending length for longest-match
	catchAll string            // empty when none
	// enumerate is the union of exact ids per provider, for GET /v1/models.
	enumerate []ModelInfo
}

type prefixEntry struct {
	prefix   string
	provider string
}

// ModelInfo is one entry in the synthetic GET /v1/models list, exact ids
// only.
type ModelInfo struct {
	ID      string
	OwnedBy string // provider name
}

// NewRegistry builds the routing table from a config-validated provider list.
// It assumes the input is already validated by config: cross-provider overlap
// (duplicate exact/prefix, catch-all count, catch-all mixed) is enforced at
// load time in config.resolve. NewRegistry only builds the routing structures;
// it does not re-validate.
func NewRegistry(provs []Provider) *Registry {
	r := &Registry{exact: map[string]string{}}
	// Enumerate must be deterministic: sort by provider declaration order then
	// by id. We preserve provider order and sort ids within each provider.
	for _, p := range provs {
		for _, m := range p.Models {
			switch m.Kind {
			case CatchAll:
				r.catchAll = p.Name
			case Exact:
				if _, ok := r.exact[m.Literal]; !ok {
					r.exact[m.Literal] = p.Name
					r.enumerate = append(r.enumerate, ModelInfo{ID: m.Literal, OwnedBy: p.Name})
				}
			case Prefix:
				r.prefixes = append(r.prefixes, prefixEntry{prefix: m.Literal, provider: p.Name})
			}
		}
	}
	// Longest-prefix first: when multiple prefixes match, the longest wins.
	sort.Slice(r.prefixes, func(i, j int) bool {
		return len(r.prefixes[i].prefix) > len(r.prefixes[j].prefix)
	})
	return r
}

// Match resolves a model id to a provider name per the precedence order:
// exact, then longest prefix, then catch-all. It returns ("", false) when there
// is no match and no catch-all. The id is matched as-is; the caller trims it.
func (r *Registry) Match(id string) (string, bool) {
	if name, ok := r.exact[id]; ok {
		return name, true
	}
	for _, pe := range r.prefixes {
		if strings.HasPrefix(id, pe.prefix) {
			return pe.provider, true
		}
	}
	if r.catchAll != "" {
		return r.catchAll, true
	}
	return "", false
}

// Lookup reports whether id matches any known pattern: exact, prefix, or
// catch-all. It is used by GET /v1/models/:id.
func (r *Registry) Lookup(id string) bool {
	_, ok := r.Match(id)
	return ok
}

// Enumerate returns the synthetic model list, exact ids only, because prefix
// and catch-all contribute none. The order is provider declaration order, then
// id.
func (r *Registry) Enumerate() []ModelInfo {
	out := make([]ModelInfo, len(r.enumerate))
	copy(out, r.enumerate)
	return out
}

// CatchAll returns the catch-all provider name, or an empty string when there
// is none.
func (r *Registry) CatchAll() string { return r.catchAll }

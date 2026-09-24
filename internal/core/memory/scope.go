// Scope enforcement for memory stores.
//
// Private (non-shared) bindings are scoped at the key level: the store layer
// prefixes every key with the owning scope, so two users of one agent never
// read or write each other's rows even though they share a physical table.
// The wrapper composes around any BackendHandle — drivers keep their contract
// and no schema changes. Scoping is a StoreBinding property ("user" default,
// "agent" opt-out); shared bindings are not wrapped at all.
//
// Scope model:
//
//	user:<uuid>   rows written by a channel-originated turn of that user
//	service       rows written by non-user contexts (agent hops, and any
//	              engine context without a user stamp)
//	agent         the "scope: agent" opt-out: one pool for the whole agent
//	              (pre-isolation behavior, deliberately preserved)
//	(legacy)      rows written before isolation shipped: unprefixed, readable
//	              by every session of the agent — pre-upgrade history is
//	              definitionally shared, and is never written again
//
// Modes (derived from provenance + the user stamp, see ModeOf):
//
//	interactive   channel turn: reads own scope | service | legacy, writes own
//	service       agent hops / no user: reads service | legacy, writes service
//	maintenance   engine-fired (system/scheduler provenance — the nightly
//	              distiller): reads ALL scopes with raw scope-visible keys,
//	              writes are scope-explicit (the caller names the scope in the
//	              key), and Scopes() enumerates the binding's user scopes
package memory

import (
	"fmt"
	"sort"
	"strings"
)

// Scope separator and well-known scope names. A key is "scoped" iff its first
// segment (up to the first separator) is one of these forms; anything else —
// including legacy keys that merely start with "user:" but contain no
// separator — is the LEGACY stratum.
const (
	scopeSep     = "|"
	scopeService = "service"
	scopeAgent   = "agent"
	scopeUserPfx = "user:"
	oversampleK  = 4 // vector/text recall: fetch K*oversample, filter, trim
)

// ScopeMode selects the read strata and write scope of a scoped handle.
type ScopeMode int

const (
	// ModeService: no user in context (agent hops, unprovenanced sessions).
	// Reads service | legacy; writes service.
	ModeService ScopeMode = iota
	// ModeInteractive: a channel-originated turn. Reads own scope | service |
	// legacy; writes own scope. This is the isolation boundary: one user's
	// session can never read another user's rows.
	ModeInteractive
	// ModeMaintenance: engine-fired context (system/scheduler provenance) —
	// the agent's own maintenance loops (distiller). Reads every scope with
	// scope-visible keys; writes are scope-explicit; Scopes() enumerates.
	ModeMaintenance
)

// ModeOf derives the scope mode from the provenance kind (core-assigned,
// unforgeable) and the tenant user stamp. system/scheduler provenance is the
// maintenance class; a user stamp makes the session interactive; anything
// else is service.
func ModeOf(provenanceKind, userUUID string) ScopeMode {
	if provenanceKind == "system" || provenanceKind == "scheduler" {
		return ModeMaintenance
	}
	if userUUID != "" {
		return ModeInteractive
	}
	return ModeService
}

func scopedKey(scope, key string) string { return scope + scopeSep + key }

// splitScopedKey splits a stored key into (scope, key, true) when it carries
// a recognized scope prefix, or ("", key, false) for legacy rows.
func splitScopedKey(k string) (scope, key string, scoped bool) {
	i := strings.Index(k, scopeSep)
	if i <= 0 {
		return "", k, false
	}
	head, rest := k[:i], k[i+1:]
	switch {
	case head == scopeService || head == scopeAgent:
		return head, rest, true
	case strings.HasPrefix(head, scopeUserPfx) && len(head) > len(scopeUserPfx):
		return head, rest, true
	}
	return "", k, false
}

// WrapScoped wraps h with scope enforcement for a non-shared binding.
// scoping is the binding's configured scoping ("user" | "agent"); "" (shared
// bindings) returns h untouched. userUUID is the ctx tenant ("" for service
// contexts); it is only used in interactive mode.
func WrapScoped(h BackendHandle, scoping string, mode ScopeMode, userUUID string) BackendHandle {
	if scoping == "" {
		return h
	}
	primary := scopeUserPfx + userUUID
	if scoping == scopeAgent {
		primary = scopeAgent
	}
	return &scopedHandle{inner: h, scoping: scoping, mode: mode, primary: primary}
}

// ScopeEnumerator is implemented by scoped handles (maintenance mode) so an
// agent's own maintenance loops can enumerate the user scopes present in a
// binding — the distiller's per-user watermark iteration.
type ScopeEnumerator interface {
	Scopes(table string) ([]string, error)
}

type scopedHandle struct {
	inner   BackendHandle
	scoping string // "user" | "agent"
	mode    ScopeMode
	primary string // user:<uuid> or agent — the interactive/agent stratum
}

// strata returns the read strata in priority order. Maintenance reads
// everything (nil = no filter). Legacy is always an implicit extra stratum
// for filtered reads.
func (s *scopedHandle) strata() []string {
	switch s.mode {
	case ModeMaintenance:
		return nil
	case ModeInteractive:
		return []string{s.primary, scopeService}
	default: // ModeService
		if s.scoping == scopeAgent {
			return []string{scopeAgent, scopeService}
		}
		return []string{scopeService}
	}
}

// writeScope is the scope keys are prefixed with on Put. Maintenance writes
// are scope-explicit: "" means the caller's key is stored raw.
func (s *scopedHandle) writeScope() string {
	if s.mode == ModeMaintenance {
		return ""
	}
	if s.mode == ModeService {
		return scopeService
	}
	return s.primary
}

func (s *scopedHandle) Put(table, key string, value any, opts PutOpts) error {
	if ws := s.writeScope(); ws != "" {
		key = scopedKey(ws, key)
	}
	return s.inner.Put(table, key, value, opts)
}

func (s *scopedHandle) Get(table, key string) (any, bool, error) {
	if s.mode == ModeMaintenance {
		return s.inner.Get(table, key)
	}
	// Priority order: each scoped stratum, then the legacy (unprefixed) key.
	for _, stratum := range s.strata() {
		if v, ok, err := s.inner.Get(table, scopedKey(stratum, key)); err != nil || ok {
			return v, ok, err
		}
	}
	return s.inner.Get(table, key)
}

func (s *scopedHandle) Delete(table, key string) error {
	if s.mode == ModeMaintenance {
		return s.inner.Delete(table, key)
	}
	// Forget works regardless of which stratum holds the key.
	for _, stratum := range s.strata() {
		if err := s.inner.Delete(table, scopedKey(stratum, key)); err != nil {
			return err
		}
	}
	return s.inner.Delete(table, key)
}

func (s *scopedHandle) Query(table string, q Query) (Iterator, error) {
	if s.mode == ModeMaintenance {
		return s.inner.Query(table, q)
	}
	switch q.Kind {
	case "prefix":
		return s.queryPrefix(table, q)
	case "text", "vector":
		// Single query over the whole table (matches may live in any
		// stratum), then filter. Vector results are ranked by the ANN over
		// the unfiltered set, so oversample before trimming back to K.
		want := q.K
		if q.Kind == "vector" && want > 0 {
			q.K = want * oversampleK
		}
		it, err := s.inner.Query(table, q)
		if err != nil {
			return nil, err
		}
		return &filterIterator{inner: it, allow: s.allowedStrata(), strip: true, limit: want}, nil
	default: // time_range, all
		it, err := s.inner.Query(table, q)
		if err != nil {
			return nil, err
		}
		return &filterIterator{inner: it, allow: s.allowedStrata(), strip: true}, nil
	}
}

// queryPrefix runs one range query per stratum (scoped prefixes plus the raw
// legacy prefix) and merges them in stratum priority order.
func (s *scopedHandle) queryPrefix(table string, q Query) (Iterator, error) {
	type result struct {
		it  Iterator
		err error
	}
	prefixes := make([]string, 0, len(s.strata())+1)
	for _, stratum := range s.strata() {
		prefixes = append(prefixes, scopedKey(stratum, q.Prefix))
	}
	prefixes = append(prefixes, q.Prefix) // legacy stratum
	its := make([]Iterator, 0, len(prefixes))
	for _, p := range prefixes {
		qq := q
		qq.Prefix = p
		it, err := s.inner.Query(table, qq)
		if err != nil {
			return nil, err
		}
		// Each per-stratum query returns only that stratum's rows, so filtering
		// looks redundant here — but the list above also carries the RAW prefix
		// as the legacy stratum, and a loop chooses that prefix. Passing
		// "user:<someone-else>|" makes the legacy scan match another user's
		// rows, so every result is re-checked against the allowed strata exactly
		// as the text/vector/time_range paths do. Relying on the backend's
		// prefix semantics to be the isolation boundary is what made this the
		// one path that could return a row the caller may not read.
		its = append(its, &filterIterator{inner: it, allow: s.allowedStrata(), strip: true})
	}
	return &mergeIterator{its: its}, nil
}

// allowedStrata is the stratum set a filtered read may return, legacy
// included.
func (s *scopedHandle) allowedStrata() map[string]bool {
	out := map[string]bool{"": true} // legacy
	for _, stratum := range s.strata() {
		out[stratum] = true
	}
	return out
}

// GC and Close pass through: retention is per-table and applies to every
// scope equally.
func (s *scopedHandle) GC(table string, window int) error { return s.inner.GC(table, window) }
func (s *scopedHandle) Close() error                      { return s.inner.Close() }

// Scopes enumerates the distinct user scopes present in a table. Maintenance
// mode only — every other mode is denied, because scope enumeration is the
// agent's own maintenance loop's per-user iteration and must never be
// reachable from a channel turn. The default implementation scans keys once;
// it is correct on every driver and conversation-scale cheap. Distinct scope
// names are returned sorted; the service/agent/legacy strata are not user
// scopes and are omitted.
func (s *scopedHandle) Scopes(table string) ([]string, error) {
	if s.mode != ModeMaintenance {
		return nil, fmt.Errorf("memory: scope enumeration requires maintenance mode")
	}
	it, err := s.inner.Query(table, Query{Kind: "all"})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for it.Next() {
		scope, _, scoped := splitScopedKey(it.Record().Key)
		if scoped && strings.HasPrefix(scope, scopeUserPfx) {
			seen[scope] = true
		}
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for sc := range seen {
		out = append(out, sc)
	}
	sort.Strings(out)
	return out, nil
}

// --- iterators ---------------------------------------------------------------

// filterIterator drops records whose scope is not allowed, strips the scope
// prefix from allowed records, and (optionally) trims to a limit.
type filterIterator struct {
	inner Iterator
	allow map[string]bool
	strip bool
	limit int // 0 = no limit
	used  int
	next  *Record
	done  bool
}

func (f *filterIterator) Next() bool {
	if f.done {
		return false
	}
	for f.inner.Next() {
		if f.limit > 0 && f.used >= f.limit {
			f.done = true
			return false
		}
		rec := f.inner.Record()
		scope, key, scoped := splitScopedKey(rec.Key)
		if !f.allow[scope] {
			continue // another user's row: never surfaced
		}
		if scoped && f.strip {
			rec.Key = key
		}
		f.used++
		f.next = &rec
		return true
	}
	f.done = true
	return false
}

func (f *filterIterator) Record() Record {
	if f.next == nil {
		return Record{}
	}
	return *f.next
}
func (f *filterIterator) Err() error { return f.inner.Err() }

// mergeIterator concatenates per-stratum iterators in priority order.
type mergeIterator struct {
	its []Iterator
	idx int
	rec Record
	ok  bool
}

func (m *mergeIterator) Next() bool {
	for m.idx < len(m.its) {
		if m.its[m.idx].Next() {
			m.rec = m.its[m.idx].Record()
			m.ok = true
			return true
		}
		m.idx++
	}
	m.ok = false
	return false
}

func (m *mergeIterator) Record() Record {
	if !m.ok {
		return Record{}
	}
	return m.rec
}
func (m *mergeIterator) Err() error {
	for _, it := range m.its {
		if err := it.Err(); err != nil {
			return err
		}
	}
	return nil
}

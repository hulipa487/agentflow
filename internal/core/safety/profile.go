package safety

// This file is the named-profile surface: a deployment selects a chain by name
// in profiles.safety, and an agent selects one of those by name in its safety:
// field. Without it the profile map was parsed by YAML and read by nothing —
// and an unknown name resolved to safety.None, so a typo silently turned the
// chain off rather than failing.

// FilterNames lists the baseline filter names, in chain order. It is what a
// profiles.safety entry selects from, and what config.validate checks a name
// against.
func FilterNames() []string {
	base := DefaultProfile()
	out := make([]string, 0, len(base.Filters))
	for _, f := range base.Filters {
		out = append(out, f.Name())
	}
	return out
}

// ProfileOf builds a profile from baseline filter names, keeping each selected
// filter as the baseline configures it (its phrases and patterns, not a bare
// zero value).
//
// An empty list is an explicit empty chain: the profile declares no filters, so
// the dispatcher runs and decides nothing. That is deliberately different from
// safety:none, which bypasses the chain entirely — the distinction matters to a
// deployment that wants the core-owned hooks in place while contributing no
// filters of its own.
//
// An unknown name is skipped here; config.validate rejects it at load, so that
// is unreachable from a config. Skipping rather than panicking keeps a
// hand-built Profile from turning a lookup miss into a crash.
func ProfileOf(name string, names []string) *Profile {
	byName := map[string]Filter{}
	for _, f := range DefaultProfile().Filters {
		byName[f.Name()] = f
	}
	p := &Profile{Name: name}
	for _, n := range names {
		if f, ok := byName[n]; ok {
			p.Filters = append(p.Filters, f)
		}
	}
	return p
}

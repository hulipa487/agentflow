package workspace

import (
	"path"
	"strings"
)

// Ignore is a set of patterns for paths a job may create that are neither
// written back to the files store nor cleaned up: dependency directories,
// build output, caches. They stay on the instance that made them, as a
// best-effort cache, and are simply absent on another.
//
// The syntax is the small, unsurprising part of .gitignore:
//
//	node_modules/   a directory with this name, anywhere
//	*.pyc           a name matching this glob, anywhere
//	build/out       this path, from the workspace root
//
// A trailing slash restricts a pattern to directories. There is no negation
// and no "**": a list that needs them belongs in the build, not here.
type Ignore struct {
	rules []rule
}

type rule struct {
	pattern  string
	anchored bool // contains a slash: matched from the root
	dirOnly  bool
}

// NewIgnore compiles patterns. Empty and malformed patterns are dropped: an
// ignore list is advice about what not to store, and a typo must not stop a
// job.
func NewIgnore(patterns []string) Ignore {
	var ig Ignore
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		dirOnly := strings.HasSuffix(p, "/")
		p = strings.Trim(p, "/")
		if p == "" {
			continue
		}
		if _, err := path.Match(p, ""); err != nil {
			continue
		}
		ig.rules = append(ig.rules, rule{pattern: p, anchored: strings.Contains(p, "/"), dirOnly: dirOnly})
	}
	return ig
}

// Match reports whether a workspace-relative slash path is ignored, either
// itself or because a directory above it is.
func (ig Ignore) Match(rel string, isDir bool) bool {
	if len(ig.rules) == 0 || rel == "" {
		return false
	}
	parts := strings.Split(rel, "/")
	for _, r := range ig.rules {
		for i := range parts {
			// Everything but the last component is certainly a directory.
			if r.dirOnly && i == len(parts)-1 && !isDir {
				continue
			}
			candidate := parts[i]
			if r.anchored {
				candidate = strings.Join(parts[:i+1], "/")
			}
			if ok, _ := path.Match(r.pattern, candidate); ok {
				return true
			}
		}
	}
	return false
}

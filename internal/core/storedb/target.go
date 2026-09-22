package storedb

import (
	"fmt"
	"strings"
)

// BackendFile is the append-log backend of the log plane: a directory of
// day files rather than a database. It lives here because it is spelled the
// same way every other target is — a scheme and an address — and because the
// alternative is a second grammar for "where does this setting point".
const BackendFile = "file"

// Target is a parsed store target: which backend a setting names, and the
// address that backend takes.
type Target struct {
	// Backend is one of BackendSQLite, BackendPostgres or BackendFile.
	Backend string
	// Address is what the backend's opener takes. For the path-based backends
	// (SQLite, the log plane) the scheme is a marker rather than part of the
	// path, so it is stripped: "sqlite://./data/x.db" opens "./data/x.db". For
	// PostgreSQL the driver parses the DSN itself, scheme and all, so the
	// address is the target as written — stripping it would hand pgx a string
	// it cannot read.
	Address string
	// Raw is the target as written, for error messages.
	Raw string
}

// ParseTarget reads a store target: which backend, and where.
//
// A target names its backend with a scheme — postgres://, postgresql://,
// sqlite://, file:// — or is a bare path, which means the first of the allowed
// backends: a store's local form is a SQLite file, the log plane's is a
// directory. The callers differ in what they allow, so they say so: a store
// allows SQLite and PostgreSQL, the log plane allows file.
//
// A scheme this build has no backend for is an error, and that is the point of
// parsing in one place. Before this, any unrecognised scheme was classified as
// a local path, so `mongodb://host/db` as a runtime store became a directory
// named "mongodb:" on one operating system and an invalid-path failure on
// another. A target the engine cannot open fails at boot, naming the schemes it
// does have.
func ParseTarget(raw string, allowed ...string) (Target, error) {
	if len(allowed) == 0 {
		return Target{}, fmt.Errorf("storedb: ParseTarget needs at least one allowed backend")
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Target{}, fmt.Errorf("storedb: empty target")
	}

	// A scheme is "word://", which is what distinguishes a backend from a path
	// that happens to contain a colon (a Windows drive letter, for instance).
	if i := strings.Index(trimmed, "://"); i > 0 {
		scheme := strings.ToLower(trimmed[:i])
		address := trimmed[i+len("://"):]
		backend, ok := schemeBackend(scheme)
		if !ok {
			return Target{}, fmt.Errorf("storedb: target %q names a backend this engine does not have (%s); it has %s",
				trimmed, scheme, strings.Join(allowed, ", "))
		}
		if !contains(allowed, backend) {
			return Target{}, fmt.Errorf("storedb: target %q is a %s target, and this setting takes %s",
				trimmed, backend, strings.Join(allowed, ", "))
		}
		if strings.TrimSpace(address) == "" {
			return Target{}, fmt.Errorf("storedb: target %q has no address", trimmed)
		}
		opened := address
		if backend == BackendPostgres {
			opened = trimmed // the driver parses the DSN, scheme included
		}
		return Target{Backend: backend, Address: opened, Raw: trimmed}, nil
	}

	// No scheme: the caller's local form.
	return Target{Backend: allowed[0], Address: trimmed, Raw: trimmed}, nil
}

// schemeBackend maps a scheme to the backend it names.
func schemeBackend(scheme string) (string, bool) {
	switch scheme {
	case "postgres", "postgresql":
		return BackendPostgres, true
	case "sqlite":
		return BackendSQLite, true
	case "file":
		return BackendFile, true
	default:
		return "", false
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

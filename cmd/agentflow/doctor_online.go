// doctor -online: the live half of `agentflow doctor`.
//
// The offline checks answer "is this configuration coherent?". These answer
// "can this process actually reach what the configuration names?" — the
// questions a fleet deployment gets wrong silently: a DSN that only one
// instance can reach, a pgvector database without the extension, an S3 bucket
// the credentials cannot write, a Redis or Qdrant that is not there.
//
// Every check is one line with a pass/fail and the fix, because the audience is
// an operator reading a deploy log.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/media"
	"agentflow/internal/core/storedb"
	"agentflow/internal/drivers/pgvector"
	"agentflow/internal/drivers/postgres"
	"agentflow/internal/drivers/qdrant"
	"agentflow/internal/drivers/redis"
	"agentflow/internal/drivers/redisvector"
	"agentflow/internal/drivers/s3media"
)

// runDoctorOnline runs the live checks and returns true when one failed.
func runDoctorOnline(cfg *config.Config, add func(status, name, detail string)) bool {
	failed := false
	fail := func(name string, err error, fix string) {
		failed = true
		add("FAIL", name, err.Error()+" — fix: "+fix)
	}

	// Runtime store: the one store every tier has, and in a regional cell the
	// one every instance must reach.
	checkRuntimeStore(cfg, add, fail)

	// Memory backends: open each one through its own provider so the check is
	// the same code path boot uses. A backend that cannot open here will not
	// open at boot either.
	seen := map[string]bool{}
	for _, name := range sortedKeys(cfg.Memory.Backends) {
		b := cfg.Memory.Backends[name]
		switch b.Provider {
		case "sqlite", "volatile":
			// Local by definition; nothing to reach. The file is created at boot.
			add("SKIP", "memory:"+name, b.Provider+" is instance-local")
			continue
		case "pgvector":
			// Checked together with the extension below, so it is not opened
			// twice for the same DSN.
			if err := checkVectorExtension(b.Config); err != nil {
				fail("memory:"+name, err, "run CREATE EXTENSION vector in that database, or use qdrant")
			} else {
				add("PASS", "memory:"+name, "pgvector connection and vector extension")
			}
			continue
		case "postgres":
			// A postgres backend may also serve vectors when the extension is
			// present (phase 2); report it, but absence is not a failure.
			if err := checkVectorExtension(b.Config); err != nil {
				add("WARN", "memory:"+name, "postgres vector queries unavailable: "+err.Error())
			} else {
				add("PASS", "memory:"+name, "postgres connection (vector extension available)")
			}
			continue
		}
		key := b.Provider + "|" + fmt.Sprint(b.Config["url"])
		if seen[key] {
			continue
		}
		seen[key] = true
		handle, err := openMemoryBackend(b)
		if err != nil {
			fix := "check the backend url and that the server is reachable"
			switch b.Provider {
			case "qdrant":
				fix = "check the url (REST port 6333 is translated to gRPC 6334) and that Qdrant is reachable"
			case "redis", "redisvector":
				fix = "check the url and that Redis is reachable"
			}
			fail("memory:"+name, err, fix)
			continue
		}
		_ = handle.Close()
		add("PASS", "memory:"+name, b.Provider+" reachable")
	}

	// Blob stores: put/get/delete a small probe in each configured bucket. The
	// probe is content-addressed, so it is the same object every run and is
	// removed again.
	for _, b := range []struct {
		name    string
		backend string
		cfg     config.MediaS3
	}{
		{"media", cfg.Media.Backend, cfg.Media.S3},
		{"files", cfg.Files.Backend, cfg.Files.S3},
	} {
		if b.backend != "s3" {
			continue
		}
		st, err := s3media.New(b.cfg)
		if err != nil {
			fail("blobs:"+b.name, err, "set "+b.name+".s3.bucket/region/access_key/secret_key")
			continue
		}
		if err := probeS3(st); err != nil {
			fail("blobs:"+b.name, err, "check the bucket, region, endpoint and credentials (the probe must be writable and readable)")
			continue
		}
		add("PASS", "blobs:"+b.name, "S3 bucket "+b.cfg.Bucket+" accepted a put/get/delete probe")
	}
	return failed
}

// checkRuntimeStore opens the runtime store and verifies it answers. SQLite is
// opened only when the file already exists: doctor must not create a database
// as a side effect of asking whether one is reachable.
func checkRuntimeStore(cfg *config.Config, add func(status, name, detail string), fail func(name string, err error, fix string)) bool {
	target := cfg.PersistencePath()
	if storedb.BackendFor(target) == storedb.BackendSQLite {
		if _, err := os.Stat(target); err != nil {
			add("WARN", "runtime store", "SQLite file "+target+" does not exist yet; it will be created at boot")
			return false
		}
	}
	st, err := storedb.Open(target)
	if err != nil {
		fail("runtime store", err, "check runtime.persistence and that the server is reachable from this host")
		return true
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := st.Exec(ctx, "SELECT 1"); err != nil {
		fail("runtime store", err, "the target answered the connection but not a query; check permissions")
		return true
	}
	add("PASS", "runtime store", st.Backend()+" at "+st.Target())
	return false
}

// checkVectorExtension verifies that the vector extension exists on the
// PostgreSQL server a backend names. The caller decides whether its absence is
// a failure (pgvector) or a warning (postgres, where vectors are optional).
func checkVectorExtension(backendConfig map[string]any) error {
	raw, _ := backendConfig["url"].(string)
	if raw == "" {
		return fmt.Errorf("backend has no url")
	}
	st, err := storedb.Open(raw)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	if err := st.QueryRow(ctx, `SELECT COUNT(*) FROM pg_extension WHERE extname = 'vector'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("the vector extension is not installed")
	}
	return nil
}

// openMemoryBackend opens one configured backend with its own provider. The
// providers ping during Open, which is the check.
func openMemoryBackend(b config.Backend) (interface{ Close() error }, error) {
	switch b.Provider {
	case "redis":
		return redis.Provider{}.Open(b.Config)
	case "redisvector":
		return redisvector.Provider{}.Open(b.Config)
	case "qdrant":
		return qdrant.Provider{}.Open(b.Config)
	case "postgres":
		return postgres.Provider{}.Open(b.Config)
	case "pgvector":
		return pgvector.Provider{}.Open(b.Config)
	default:
		return nil, fmt.Errorf("cannot open provider %q", b.Provider)
	}
}

// probeS3 exercises the bucket end to end: write the probe, read it back, and
// delete it. A bucket that accepts a PUT but not a GET is still broken, and
// that is exactly the failure a HEAD-only check misses.
func probeS3(st *s3media.Store) error {
	probe := []byte("agentflow-doctor-probe")
	ref, err := st.Put(bytes.NewReader(probe), "text/plain", media.Policy{MaxBytes: 1024})
	if err != nil {
		return fmt.Errorf("put: %w", err)
	}
	defer func() { _ = st.Delete(ref.Handle) }()
	got, err := st.ReadAll(ref.Handle, 1024)
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	if !bytes.Equal(got, probe) {
		return fmt.Errorf("get returned different bytes")
	}
	if err := st.Delete(ref.Handle); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

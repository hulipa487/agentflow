// `agentflow migrate`: move a deployment's state to another store.
//
// The two commands exist for the tier change: a single-instance deployment
// moving its SQLite file into a shared PostgreSQL, and its local blobs into
// S3. Both are copies, not transforms — the engine writes one SQL dialect to
// both backends and names blobs by content hash — so a failure leaves the
// source untouched and a rerun is safe.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/credentials"
	"agentflow/internal/core/identity"
	"agentflow/internal/core/media"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/storedb"
	"agentflow/internal/drivers/s3media"
)

// storeTables are the tables `migrate store` knows how to copy: the runtime
// store's planes, the identity registry and the credential store. Tenancy,
// route state, session state, shell handles and file metadata live in
// files_meta as key/value rows, so they move with that table.
var storeTables = []string{
	"message_journal",
	"files_meta",
	"usage_daily",
	"usage_events",
	"profiles",
	"identities",
	"user_tokens",
	"link_challenges",
	"invites",
	"credentials",
}

func cmdMigrate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: agentflow migrate <store|blobs> [flags]")
		return 2
	}
	switch args[0] {
	case "store":
		return cmdMigrateStore(args[1:], stdout, stderr)
	case "blobs":
		return cmdMigrateBlobs(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "migrate: unknown target", args[0], "(want store or blobs)")
		return 2
	}
}

func cmdMigrateStore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate store", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "source store target (sqlite path or postgres DSN)")
	to := fs.String("to", "", "destination store target (sqlite path or postgres DSN)")
	tablesFlag := fs.String("tables", "", "comma-separated table subset (default: every known table)")
	appendRows := fs.Bool("append", false, "copy into a non-empty destination")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "migrate store: unexpected argument:", fs.Arg(0))
		return 2
	}
	if *from == "" || *to == "" {
		fmt.Fprintln(stderr, "migrate store: -from and -to are required")
		return 2
	}
	if config.SameStoreTarget(*from, *to) {
		fmt.Fprintln(stderr, "migrate store: source and destination are the same store")
		return 2
	}

	tables := storeTables
	if strings.TrimSpace(*tablesFlag) != "" {
		tables = nil
		known := map[string]bool{}
		for _, t := range storeTables {
			known[t] = true
		}
		for _, t := range strings.Split(*tablesFlag, ",") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if !known[t] {
				fmt.Fprintf(stderr, "migrate store: unknown table %q (known: %s)\n", t, strings.Join(storeTables, ", "))
				return 2
			}
			tables = append(tables, t)
		}
	}

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	src, err := storedb.Open(*from)
	if err != nil {
		fmt.Fprintln(stderr, "migrate store: source:", err)
		return 1
	}
	defer src.Close()

	// Create the destination schema by opening each subsystem that owns a
	// table. This is the same migration boot runs, so the copy lands in a
	// database a boot would accept.
	if dstRuntime, err := runtime.OpenStore(*to, log); err != nil {
		fmt.Fprintln(stderr, "migrate store: destination runtime store:", err)
		return 1
	} else {
		_ = dstRuntime.Close()
	}
	ident, err := identity.Open(*to, log)
	if err != nil {
		fmt.Fprintln(stderr, "migrate store: destination identity store:", err)
		return 1
	}
	_ = ident.Close()
	if err := credentials.EnsureSchema(*to, log); err != nil {
		fmt.Fprintln(stderr, "migrate store: destination credential store:", err)
		return 1
	}
	dst, err := storedb.Open(*to)
	if err != nil {
		fmt.Fprintln(stderr, "migrate store: destination:", err)
		return 1
	}
	defer dst.Close()

	var copied int64
	for _, table := range tables {
		count, err := tableCount(ctx, src, table)
		if err != nil {
			// A table the source never created is nothing to copy (identity
			// was disabled, credentials never used). Say so rather than fail.
			fmt.Fprintf(stdout, "skip  %-16s (not in source)\n", table)
			continue
		}
		if count == 0 {
			fmt.Fprintf(stdout, "skip  %-16s (empty)\n", table)
			continue
		}
		if !*appendRows {
			existing, err := tableCount(ctx, dst, table)
			if err != nil {
				fmt.Fprintln(stderr, "migrate store: destination:", err)
				return 1
			}
			if existing > 0 {
				fmt.Fprintf(stderr, "migrate store: destination table %q already has %d rows; pass -append to copy into it\n", table, existing)
				return 1
			}
		}
		n, err := copyTable(ctx, src, dst, table)
		if err != nil {
			fmt.Fprintf(stderr, "migrate store: %s: %v\n", table, err)
			return 1
		}
		copied += n
		fmt.Fprintf(stdout, "copy  %-16s %d rows\n", table, n)
	}
	fmt.Fprintf(stdout, "ok: %d rows copied from %s to %s\n", copied, storedb.Display(*from), storedb.Display(*to))
	fmt.Fprintln(stdout, "note: credentials are ciphertext; the destination must use the same master key")
	return 0
}

func tableCount(ctx context.Context, db *storedb.DB, table string) (int64, error) {
	var n int64
	// The table name comes from storeTables or is validated against it; it is
	// never request input.
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// copyTable copies one table in batches, so a large journal does not hold one
// transaction open for the whole migration.
func copyTable(ctx context.Context, src, dst *storedb.DB, table string) (int64, error) {
	rows, err := src.Query(ctx, "SELECT * FROM "+table)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	if len(cols) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
	insert := "INSERT INTO " + table + " (" + strings.Join(cols, ",") + ") VALUES (" + placeholders + ")"

	const batch = 1000
	var copied int64
	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	pending := 0
	rollback := func() { _ = tx.Rollback() }
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rollback()
			return copied, err
		}
		for i, v := range vals {
			// A BLOB column scanned from SQLite arrives as []byte; the same
			// value is TEXT on the other backend. The schemas store text as
			// text, so this conversion is the identity.
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		if _, err := tx.Exec(ctx, insert, vals...); err != nil {
			rollback()
			return copied, err
		}
		copied++
		pending++
		if pending >= batch {
			if err := tx.Commit(); err != nil {
				return copied, err
			}
			tx, err = dst.BeginTx(ctx, nil)
			if err != nil {
				return copied, err
			}
			pending = 0
		}
	}
	if err := rows.Err(); err != nil {
		rollback()
		return copied, err
	}
	if err := tx.Commit(); err != nil {
		return copied, err
	}
	return copied, nil
}

func cmdMigrateBlobs(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath, configDir := cliConfigFlags("migrate blobs", stderr)
	from := fs.String("from", "", "source directory (fs:<dir> or a bare path)")
	kind := fs.String("kind", "media", "which S3 settings to use: media or files")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "migrate blobs: unexpected argument:", fs.Arg(0))
		return 2
	}
	if *from == "" {
		fmt.Fprintln(stderr, "migrate blobs: -from is required")
		return 2
	}
	dir := strings.TrimSpace(strings.TrimPrefix(*from, "fs:"))
	if dir == "" {
		fmt.Fprintln(stderr, "migrate blobs: -from names no directory")
		return 2
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		fmt.Fprintf(stderr, "migrate blobs: %s is not a directory\n", dir)
		return 1
	}
	cfg, _, err := loadConfigForCLI(*cfgPath, *configDir, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "config:", err)
		return 1
	}
	var s3cfg config.MediaS3
	switch *kind {
	case "media":
		s3cfg = cfg.Media.S3
	case "files":
		s3cfg = cfg.Files.S3
	default:
		fmt.Fprintln(stderr, "migrate blobs: -kind must be media or files")
		return 2
	}
	st, err := s3media.New(s3cfg)
	if err != nil {
		fmt.Fprintln(stderr, "migrate blobs: S3 store:", err)
		return 1
	}

	var files, copied int64
	var bytes int64
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		files++
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return err
		}
		// The store is content-addressed: Put hashes the bytes, HEADs the
		// object, and skips the upload when it is already there. Rerunning the
		// migration therefore costs one HEAD per blob.
		_, putErr := st.Put(f, "application/octet-stream", media.Policy{MaxBytes: info.Size() + 1})
		_ = f.Close()
		if putErr != nil {
			return fmt.Errorf("%s: %w", path, putErr)
		}
		copied++
		bytes += info.Size()
		return nil
	})
	if err != nil {
		fmt.Fprintln(stderr, "migrate blobs:", err)
		return 1
	}
	fmt.Fprintf(stdout, "ok: %d of %d blobs copied to bucket %s (%d bytes)\n", copied, files, s3cfg.Bucket, bytes)
	return 0
}

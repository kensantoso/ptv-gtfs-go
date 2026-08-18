// Package store manages the local GTFS database: where it lives, when it was
// built, and when it has gone stale.
//
// The database is built on the user's machine rather than downloaded prebuilt.
// That avoids distributing a multi-gigabyte asset, avoids anyone having to
// trust a build, and keeps the data as fresh as the publisher's feed. The cost
// is a few minutes on first run.
//
// Deliberately not named index: [gtfs.Index] is the query handle, the file
// holds real SQL indexes, and one word for three things helps nobody.
//
// Unlike the root package, which takes a *sql.DB and never opens one, this
// package opens the file itself under the driver name "sqlite". A caller must
// therefore register one, which in practice means:
//
//	import _ "modernc.org/sqlite"
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	gtfs "github.com/kensantoso/ptv-gtfs-go"
)

// MaxAge is how old the database may be before a rebuild is suggested. The feed is
// republished roughly weekly, and a schedule a few days stale is still correct
// for almost every question.
const MaxAge = 7 * 24 * time.Hour

// Manager owns the database file and its lifecycle.
type Manager struct {
	Dir     string
	FeedURL string
	Modes   []gtfs.Mode
}

// DefaultDir is where the database lives when unset: the user's cache directory,
// because this is derived data that can always be rebuilt.
func DefaultDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "ptv-mcp")
}

// DBPath is the database file.
func (m *Manager) DBPath() string { return filepath.Join(m.Dir, "ptv.db") }

// Status describes the database on disk.
type Status struct {
	Exists  bool
	BuiltAt time.Time
	Stale   bool
	Bytes   int64
}

// Status reports on the database without building it.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	fi, err := os.Stat(m.DBPath())
	if os.IsNotExist(err) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	s := Status{Exists: true, Bytes: fi.Size()}

	db, err := sql.Open("sqlite", m.DBPath())
	if err != nil {
		return s, nil // present but unreadable; caller will rebuild
	}
	defer db.Close()

	var built string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='built_at'`).Scan(&built); err != nil {
		// No meta row means an interrupted build. Treat as stale so it is
		// replaced rather than queried and quietly returning nothing.
		s.Stale = true
		return s, nil
	}
	if t, err := time.Parse(time.RFC3339, built); err == nil {
		s.BuiltAt = t
		s.Stale = time.Since(t) > MaxAge
	}
	return s, nil
}

// Progress reports build progress to the caller.
type Progress struct {
	Stage      string // "download", "extract", "index"
	Bytes      int64
	Rows       int
	Mode       string
	File       string
	Elapsed    time.Duration
	Downloaded int64
}

// Build downloads the feed and loads it, replacing any existing database.
//
// The new database is built under a temporary name and moved into place only
// on success, so an interrupted build never leaves a partial one that queries
// would silently return nothing from.
func (m *Manager) Build(ctx context.Context, progress func(Progress)) error {
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return fmt.Errorf("store: create %s: %w", m.Dir, err)
	}
	start := time.Now()
	report := func(p Progress) {
		if progress != nil {
			p.Elapsed = time.Since(start)
			progress(p)
		}
	}

	zipPath := filepath.Join(m.Dir, "gtfs.zip")
	report(Progress{Stage: "download"})
	if err := gtfs.Download(ctx, m.FeedURL, zipPath, func(n int64) {
		report(Progress{Stage: "download", Downloaded: n})
	}); err != nil {
		return err
	}

	report(Progress{Stage: "extract"})
	modes := m.Modes
	if len(modes) == 0 {
		modes = gtfs.AllModes
	}
	mf, err := gtfs.OpenBundle(zipPath, modes)
	if err != nil {
		return err
	}

	tmp := m.DBPath() + ".building"
	os.Remove(tmp)
	db, err := sql.Open("sqlite", tmp)
	if err != nil {
		return fmt.Errorf("store: open %s: %w", tmp, err)
	}
	if err := gtfs.Build(ctx, db, mf, func(p gtfs.BuildProgress) {
		report(Progress{Stage: "index", Mode: p.Mode.String(), File: p.File, Rows: p.Rows})
	}); err != nil {
		db.Close()
		os.Remove(tmp)
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.DBPath()); err != nil {
		return fmt.Errorf("store: install: %w", err)
	}

	// The bundle is only needed during the build and is a third of a gigabyte.
	os.Remove(zipPath)
	return nil
}

// EnsureBuilt builds the database when absent, and returns an open handle.
//
// A stale database is used rather than rebuilt: blocking a user's first question
// for several minutes to refresh a schedule that is days old, not wrong, is a
// bad trade. Callers can rebuild explicitly.
func (m *Manager) EnsureBuilt(ctx context.Context, progress func(Progress)) (*sql.DB, error) {
	st, err := m.Status(ctx)
	if err != nil {
		return nil, err
	}
	if !st.Exists || (st.Stale && st.BuiltAt.IsZero()) {
		if err := m.Build(ctx, progress); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", m.DBPath())
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// Reads can run concurrently against an immutable file, but the pool has to
	// be allowed to open more than one connection or every goroutine queues
	// behind the same handle and parallel planning runs serially.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)

	// Read-only workload from here: these make queries substantially faster.
	for _, p := range []string{"PRAGMA query_only=ON", "PRAGMA cache_size=-64000"} {
		if _, err := db.ExecContext(ctx, p); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

// KeyFile is where a realtime key is stored when no environment variable is
// set. It lives beside the database because both are per-installation data.
func KeyFile(dir string) string { return filepath.Join(dir, "realtime.key") }

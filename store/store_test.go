package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestStatusOnAMissingDatabase(t *testing.T) {
	m := &Manager{Dir: t.TempDir()}
	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status on an empty directory: %v", err)
	}
	if st.Exists || st.Stale || st.Bytes != 0 {
		t.Errorf("got %+v, want the zero Status: absent is not stale, it is absent", st)
	}
}

// A file with no meta row is an interrupted build. Treating it as usable means
// querying a partial database that quietly returns nothing.
func TestAPartialBuildReportsStale(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{Dir: dir}
	if err := os.WriteFile(m.DBPath(), []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Exists {
		t.Error("Exists = false for a file that is present")
	}
	if !st.Stale {
		t.Error("Stale = false for a database with no meta row; it would be queried as if whole")
	}
}

func TestPathsAreUnderTheManagersDirectory(t *testing.T) {
	m := &Manager{Dir: "/tmp/x"}
	if got, want := m.DBPath(), filepath.Join("/tmp/x", "ptv.db"); got != want {
		t.Errorf("DBPath = %q, want %q", got, want)
	}
	if got, want := KeyFile("/tmp/x"), filepath.Join("/tmp/x", "realtime.key"); got != want {
		t.Errorf("KeyFile = %q, want %q", got, want)
	}
	if d := DefaultDir(); d == "" || !strings.HasSuffix(d, "ptv-mcp") {
		t.Errorf("DefaultDir = %q, want a path ending in ptv-mcp", d)
	}
}

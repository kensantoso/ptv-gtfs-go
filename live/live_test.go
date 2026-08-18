package live

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gtfs "github.com/kensantoso/ptv-gtfs-go"
	"github.com/kensantoso/ptv-gtfs-go/realtime"
)

// A nil *Cache is how the server runs with no realtime key: every call must
// stay safe and yield nothing, rather than the caller guarding each use.
func TestNilCacheIsUsable(t *testing.T) {
	var c *Cache
	if c.Enabled() {
		t.Error("Enabled = true on a nil cache")
	}
	snap, err := c.Get(context.Background())
	if err != nil {
		t.Errorf("Get on a nil cache: %v", err)
	}
	if snap != nil {
		t.Error("Get returned a snapshot from a nil cache")
	}
}

// A total outage must not replace a good snapshot, and must not report itself
// as fresh.
//
// FetchLive returns a non-nil Live even when every feed failed, so a nil test
// never fires: the empty snapshot gets stored, stamped as fetched now, and
// [gtfs.Live.Age] returns zero for its zero At — so the outage is invisible by
// the very mechanism meant to expose it, for a full TTL after recovery.
func TestTotalFailureKeepsTheLastGoodSnapshot(t *testing.T) {
	var fail bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.Write(nil) // an empty but valid body decodes to a snapshot with no updates
	}))
	defer srv.Close()

	rt, err := realtime.NewClient("k", realtime.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	c := New(rt, gtfs.ModeMetroTrain)

	// Prime it with a snapshot that carries a timestamp, as a real fetch would.
	c.snap = gtfs.NewLive(&realtime.Data{At: time.Now().Add(-30 * time.Second)})
	c.at = time.Now().Add(-2 * TTL) // expired, so Get refetches
	primed := c.snap

	fail = true
	got, err := c.Get(context.Background())
	if err == nil && got != primed {
		t.Error("a total failure replaced the good snapshot and reported no error")
	}
	if c.snap != primed {
		t.Error("the stored snapshot was overwritten by an empty one")
	}
	if got != nil && got.At.IsZero() {
		t.Error("served a contentless snapshot, which Age would report as perfectly fresh")
	}
}

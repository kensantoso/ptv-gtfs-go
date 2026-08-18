// Package live keeps a recent realtime snapshot without hammering the gateway.
//
// The gateway caches each feed for 30 seconds and allows 24 requests per minute
// per mode, so fetching per tool call would spend the budget on identical
// bytes. One shared snapshot, refreshed no faster than the gateway changes it,
// answers every tool from the same view of the network.
package live

import (
	"context"
	"sync"
	"time"

	gtfs "github.com/kensantoso/ptv-gtfs-go"
	"github.com/kensantoso/ptv-gtfs-go/realtime"
)

// TTL is how long a snapshot is reused. It matches the gateway's own cache:
// refreshing faster returns the same bytes and burns quota.
const TTL = 30 * time.Second

// Cache holds the most recent snapshot.
//
// The zero value is not usable; build one with New. A nil *Cache is valid and
// always yields a nil snapshot, which is how the server runs with no realtime
// key configured.
type Cache struct {
	client *realtime.Client
	modes  []gtfs.Mode

	mu   sync.Mutex
	snap *gtfs.Live
	at   time.Time
	err  error
}

// New returns a cache over the given client. A nil client yields a nil cache,
// so callers never have to branch on whether realtime is configured.
func New(c *realtime.Client, modes ...gtfs.Mode) *Cache {
	if c == nil {
		return nil
	}
	return &Cache{client: c, modes: modes}
}

// Enabled reports whether realtime is configured at all.
func (c *Cache) Enabled() bool { return c != nil }

// Get returns a snapshot no older than [TTL].
//
// A fetch failure returns the previous snapshot when there is one, alongside
// the error. Stale live data beats none: a passenger is better served by a
// delay from ninety seconds ago than by silence, provided the staleness is
// visible, which [gtfs.Live.Age] makes it.
func (c *Cache) Get(ctx context.Context) (*gtfs.Live, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.snap != nil && time.Since(c.at) < TTL {
		return c.snap, nil
	}

	snap, err := gtfs.FetchLive(ctx, c.client, c.modes...)
	// A snapshot that learned nothing is not a refresh. FetchLive returns one
	// even when every feed failed, so testing for nil never fires — and storing
	// it replaces good data with an empty snapshot that then reports itself as
	// freshly fetched, because [gtfs.Live.Age] is zero for a zero At. The outage
	// becomes invisible by the very mechanism meant to expose it.
	if err != nil && (snap == nil || snap.At.IsZero()) {
		if c.snap != nil {
			return c.snap, nil // stale beats nothing, and it keeps its real age
		}
		c.err = err
		return nil, err
	}
	// A partial failure still carries data, so a feed being down for one mode
	// refreshes the others.
	c.snap, c.at, c.err = snap, time.Now(), err
	return snap, nil
}

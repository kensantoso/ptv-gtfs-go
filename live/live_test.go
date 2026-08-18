package live

import (
	"context"
	"testing"
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

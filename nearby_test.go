package gtfs

import (
	"context"
	"testing"
	"time"
)

func TestModeBucketCollapsesEightModesToThree(t *testing.T) {
	for _, c := range []struct {
		in   Mode
		want string
	}{
		{ModeMetroTrain, "train"}, {ModeRegionalTrain, "train"},
		{ModeMetroTram, "tram"},
		{ModeMetroBus, "bus"}, {ModeRegionalCoach, "bus"},
		{ModeRegionalBus, "bus"}, {ModeTeleBus, "bus"}, {ModeNightBus, "bus"},
	} {
		if got := ModeBucket(c.in); got != c.want {
			t.Errorf("ModeBucket(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The reason grouping exists: bus stops outnumber stations, so a plain
// nearest-N returns nothing else. Here nine bus stops sit closer than the
// station, and the station must still be offered.
func TestStopsNearByModeKeepsAModeThatIsCrowdedOut(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,lat,lon,mode) VALUES
		 ('b1','Bus 1',-37.8000,144.9000,4),('b2','Bus 2',-37.8001,144.9000,4),
		 ('b3','Bus 3',-37.8002,144.9000,4),('b4','Bus 4',-37.8003,144.9000,4),
		 ('b5','Bus 5',-37.8004,144.9000,4),('b6','Bus 6',-37.8005,144.9000,4),
		 ('b7','Bus 7',-37.8006,144.9000,4),('b8','Bus 8',-37.8007,144.9000,4),
		 ('b9','Bus 9',-37.8008,144.9000,4),
		 ('t1','Tram 1',-37.8009,144.9000,3),
		 ('s1','Station',-37.8010,144.9000,2);
	`)
	ix := Open(db, time.UTC)
	groups, err := ix.StopsNearByMode(context.Background(), NearbyRequest{
		Lat: -37.8000, Lon: 144.9000, RadiusMetres: 2000, PerMode: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, g := range groups {
		if len(g.Stops) != 1 {
			t.Errorf("%s: got %d stops, want PerMode=1", g.Mode, len(g.Stops))
		}
		got[g.Mode] = g.Stops[0].Name
	}
	for _, m := range []string{"train", "tram", "bus"} {
		if got[m] == "" {
			t.Errorf("mode %q missing; a plain nearest-N would have returned buses only", m)
		}
	}
	// Nearest mode first: the buses are closest, so they lead.
	if groups[0].Mode != "bus" {
		t.Errorf("first group = %q, want bus (its nearest stop is closest)", groups[0].Mode)
	}
}

func TestNearbyRequestDefaults(t *testing.T) {
	r := NearbyRequest{}.withDefaults()
	if r.RadiusMetres != DefaultNearbyRadius || r.PerMode != 1 {
		t.Errorf("got radius=%v perMode=%d, want %v and 1", r.RadiusMetres, r.PerMode, DefaultNearbyRadius)
	}
	r2 := NearbyRequest{RadiusMetres: 400, PerMode: 3}.withDefaults()
	if r2.RadiusMetres != 400 || r2.PerMode != 3 {
		t.Error("withDefaults overwrote values the caller set")
	}
}

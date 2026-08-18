package gtfs

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// A station id must expand to every platform, whether the caller passes the
// station or one of its platforms.
//
// This regressed once in a way that produced no error and no results: Victoria
// names the station row "X Railway Station" and its platforms "X Station", so a
// name-based fallback matched nothing and every departure from that station
// silently disappeared.
func TestStopIDsForStationExpandsPlatforms(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,mode,parent) VALUES
			('vic:rail:FSS','Flinders Street Railway Station',2,NULL),
			('19842','Flinders Street Station',2,'vic:rail:FSS'),
			('19843','Flinders Street Station',2,'vic:rail:FSS'),
			('bus-1','Smith St/Main Rd',4,NULL),
			('bus-2','Smith St/Main Rd',4,NULL);
	`)
	ix := Open(db, nil)
	ctx := context.Background()

	// Passing the station itself.
	got, err := ix.StopIDsForStation(ctx, "vic:rail:FSS")
	if err != nil {
		t.Fatalf("StopIDsForStation(station): %v", err)
	}
	if len(got) != 3 {
		t.Errorf("station expanded to %d stops, want 3 (itself plus two platforms): %v", len(got), got)
	}

	// Passing a platform must give the same set.
	viaPlatform, err := ix.StopIDsForStation(ctx, "19842")
	if err != nil {
		t.Fatalf("StopIDsForStation(platform): %v", err)
	}
	if len(viaPlatform) != len(got) {
		t.Errorf("platform expanded to %d stops, station to %d; they must agree", len(viaPlatform), len(got))
	}

	// A stop with no parent groups by name, so both sides of a road are covered.
	bus, err := ix.StopIDsForStation(ctx, "bus-1")
	if err != nil {
		t.Fatalf("StopIDsForStation(bus): %v", err)
	}
	if len(bus) != 2 {
		t.Errorf("roadside stop expanded to %d, want 2 (both directions): %v", len(bus), bus)
	}
}

// A search must return a station once, not once per platform.
func TestFindStopsCollapsesPlatforms(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,mode,parent) VALUES
			('vic:rail:FSS','Flinders Street Railway Station',2,NULL),
			('19842','Flinders Street Station',2,'vic:rail:FSS'),
			('19843','Flinders Street Station',2,'vic:rail:FSS');
	`)
	ix := Open(db, nil)
	got, err := ix.FindStops(context.Background(), "Flinders Street")
	if err != nil {
		t.Fatalf("FindStops: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1 station: %+v", len(got), got)
	}
	// The station record should win over a platform, since that is what a
	// caller means by "Flinders Street".
	if got[0].ID != "vic:rail:FSS" {
		t.Errorf("got %q, want the station record vic:rail:FSS", got[0].ID)
	}
}

func testDB(t *testing.T, seed string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Skipf("no sqlite driver registered: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(Schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := db.Exec(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

func TestStopLooksUpOneStopByID(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,lat,lon,parent,mode,platform)
		VALUES ('P1','Richmond Station',-37.82,145.00,'STA',2,'1');
	`)
	ix := Open(db, time.UTC)
	got, err := ix.Stop(context.Background(), "P1")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got.ID != "P1" || got.Name != "Richmond Station" || got.Parent != "STA" ||
		got.Platform != "1" || got.Mode != ModeMetroTrain || got.Lat == 0 {
		t.Errorf("got %+v, want the row as inserted", got)
	}
	if _, err := ix.Stop(context.Background(), "nope"); err == nil {
		t.Error("Stop on a missing id returned no error")
	}
}

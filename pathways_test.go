package gtfs

import (
	"context"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Transfer time must come from the pathway graph, not from transfers.txt, whose
// rows in this feed are all in-seat continuations carrying a minimum time of
// zero.
func TestWalkTimeCrossesTheStationGraph(t *testing.T) {
	db := testDB(t, `
		INSERT INTO pathway(from_stop,to_stop,bidir,seconds) VALUES
			('P1','CONCOURSE',1,60),
			('CONCOURSE','P2',1,90),
			('P1','P3',0,30);
	`)
	ix := Open(db, time.UTC)
	ctx := context.Background()

	// Two hops through the concourse.
	got, ok, err := ix.WalkTime(ctx, "P1", "P2")
	if err != nil || !ok {
		t.Fatalf("WalkTime(P1,P2) ok=%v err=%v, want a path", ok, err)
	}
	if got != 150*time.Second {
		t.Errorf("got %v, want 150s (60 + 90)", got)
	}

	// Bidirectional edges must work in reverse.
	if got, ok, _ := ix.WalkTime(ctx, "P2", "P1"); !ok || got != 150*time.Second {
		t.Errorf("reverse walk = %v ok=%v, want 150s", got, ok)
	}

	// A one-way edge must not be traversed backwards.
	if _, ok, _ := ix.WalkTime(ctx, "P3", "P1"); ok {
		t.Error("traversed a one-way pathway backwards")
	}
}

// An unknown station must report not-known so the caller applies its own
// default, rather than reporting that no time is needed.
func TestWalkTimeUnknownIsNotZero(t *testing.T) {
	db := testDB(t, `INSERT INTO pathway(from_stop,to_stop,bidir,seconds) VALUES ('A','B',1,60);`)
	ix := Open(db, time.UTC)

	if _, ok, _ := ix.WalkTime(context.Background(), "X", "Y"); ok {
		t.Fatal("reported a walk time for stops absent from the graph")
	}
	got, err := ix.TransferTime(context.Background(), "X", "Y")
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultPolicy().DefaultTransferTime {
		t.Errorf("TransferTime = %v, want the %v default", got, DefaultPolicy().DefaultTransferTime)
	}
}

// A published path of a few seconds describes two points on one concourse, not
// a change a passenger makes. The default is a floor.
func TestTransferTimeIsFloored(t *testing.T) {
	db := testDB(t, `INSERT INTO pathway(from_stop,to_stop,bidir,seconds) VALUES ('A','B',1,6);`)
	ix := Open(db, time.UTC)
	got, err := ix.TransferTime(context.Background(), "A", "B")
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultPolicy().DefaultTransferTime {
		t.Errorf("TransferTime = %v, want it floored to %v", got, DefaultPolicy().DefaultTransferTime)
	}
}

// The planner must reject a connection the walk does not allow, and take the
// next one that works rather than dropping the journey.
func TestPlanRejectsUnwalkableConnection(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,mode,parent) VALUES
			('SUB','Suburb Station',2,NULL),
			('X','Interchange Railway Station',2,NULL),
			('XA','Interchange Station',2,'X'),
			('XB','Interchange Station',2,'X'),
			('DEST','Destination Station',2,NULL);

		INSERT INTO route(route_id,short_name,long_name,mode) VALUES ('R','L','Line',2);
		INSERT INTO calendar(service_id,dow,start_date,end_date) VALUES ('SAT',6,'20260101','20261231');

		INSERT INTO trip(trip_id,route_id,service_id,headsign,direction,mode) VALUES
			('IN','R','SAT','Interchange',0,2),
			('TIGHT','R','SAT','Destination',0,2),
			('OK','R','SAT','Destination',0,2);

		-- arrive XA at 10:10
		INSERT INTO stop_time(trip_id,stop_id,seq,arr,dep) VALUES
			('IN','SUB',1,36000,36000),
			('IN','XA',2,36600,36600),
		-- leaves XB at 10:13, only three minutes later
			('TIGHT','XB',1,36780,36780),
			('TIGHT','DEST',2,37200,37200),
		-- leaves XB at 10:20, comfortably after
			('OK','XB',1,37200,37200),
			('OK','DEST',2,37800,37800);

		-- the platforms are eight minutes apart on foot
		INSERT INTO pathway(from_stop,to_stop,bidir,seconds) VALUES ('XA','XB',1,480);
	`)
	ix := Open(db, time.UTC)

	js, err := ix.Plan(context.Background(), PlanRequest{
		FromStopID: "SUB", ToStopID: "DEST", After: saturday("09:00"),
		MaxTransfers: 1, Rank: RankFastest, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(js) == 0 {
		t.Fatal("no journey; the later connection is walkable and should have been used")
	}
	if got := js[0].Legs[1].TripID; got != "OK" {
		t.Errorf("connected to %s, want OK: the three-minute change needs an eight-minute walk", got)
	}
}

// Distance must be measured from the station, and platforms must not each
// appear as a separate nearby result.
func TestStopsNearCollapsesAndSorts(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,lat,lon,mode,parent) VALUES
			('vic:rail:A','A Railway Station',-37.8180,144.9670,2,NULL),
			('a1','A Station',-37.8181,144.9671,2,'vic:rail:A'),
			('a2','A Station',-37.8182,144.9672,2,'vic:rail:A'),
			('far','Far Station',-37.9000,145.1000,2,NULL),
			('tramstop','Swanston St #1',-37.8185,144.9668,3,NULL);
	`)
	ix := Open(db, time.UTC)
	ctx := context.Background()

	got, err := ix.StopsNear(ctx, -37.8180, 144.9670, 2000, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got {
		if s.ID == "a1" || s.ID == "a2" {
			t.Errorf("platform %s returned separately; it should collapse into its station", s.ID)
		}
	}
	if len(got) < 2 {
		t.Fatalf("got %d stops, want the station and the tram stop", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Metres < got[i-1].Metres {
			t.Errorf("results not sorted by distance: %v", got)
		}
	}
	// The far station is well outside the radius.
	for _, s := range got {
		if s.ID == "far" {
			t.Error("returned a stop beyond the radius")
		}
	}

	// A mode filter must apply.
	trams, err := ix.StopsNear(ctx, -37.8180, 144.9670, 2000, 10, ModeMetroTram)
	if err != nil {
		t.Fatal(err)
	}
	if len(trams) != 1 || trams[0].ID != "tramstop" {
		t.Errorf("mode filter returned %v, want just the tram stop", trams)
	}
}

// The whole point of asking transfers.txt first is that the answer improves on
// its own if the operator ever populates it. Today Victoria publishes only
// in-seat continuations with no times, so nothing comes from there; a feed that
// stated real minimums must be picked up without a code change.
func TestPublishedTransferTimeBeatsTheWalk(t *testing.T) {
	db := testDB(t, `
		INSERT INTO pathway(from_stop,to_stop,bidir,seconds) VALUES ('A','B',1,300);
	`)
	ix := Open(db, time.UTC)
	ctx := context.Background()

	// With nothing published, the walk stands: five minutes. Kept under
	// MaxTransferTime, or the graph search discards it before we get here.
	if got, _ := ix.TransferTime(ctx, "A", "B"); got != 300*time.Second {
		t.Fatalf("walk-derived = %v, want 300s", got)
	}

	// The operator says three minutes. Their station, their figure.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO transfer(from_stop,to_stop,type,min_time) VALUES('A','B',2,180)`); err != nil {
		t.Fatal(err)
	}
	if got, _ := ix.TransferTime(ctx, "A", "B"); got != 180*time.Second {
		t.Errorf("published = %v, want 180s to win over the derived 300s", got)
	}
}

// A published time is taken as given rather than floored: the operator saying
// forty seconds is not improved by our opinion of it.
func TestPublishedTransferTimeIsNotFloored(t *testing.T) {
	db := testDB(t, `
		INSERT INTO transfer(from_stop,to_stop,type,min_time) VALUES('A','B',2,40);
	`)
	ix := Open(db, time.UTC)
	got, err := ix.TransferTime(context.Background(), "A", "B")
	if err != nil {
		t.Fatal(err)
	}
	if got != 40*time.Second {
		t.Errorf("got %v, want the published 40s rather than the %v default",
			got, DefaultPolicy().DefaultTransferTime)
	}
}

// Type 3 means the transfer cannot be made. It must not read as a fast one.
func TestTransferNotPossibleBlocksTheConnection(t *testing.T) {
	db := testDB(t, `
		INSERT INTO pathway(from_stop,to_stop,bidir,seconds) VALUES ('A','B',1,60);
		INSERT INTO transfer(from_stop,to_stop,type,min_time) VALUES('A','B',3,0);
	`)
	ix := Open(db, time.UTC)
	got, err := ix.TransferTime(context.Background(), "A", "B")
	if err != nil {
		t.Fatal(err)
	}
	if got <= DefaultPolicy().MaxTransferTime {
		t.Errorf("got %v; a forbidden transfer must exceed any connection window", got)
	}
}

// Types 4 and 5 are in-seat continuations. Their blank time means the passenger
// does not move, not that the change is instant.
func TestInSeatTransferDoesNotSetATime(t *testing.T) {
	db := testDB(t, `
		INSERT INTO transfer(from_stop,to_stop,from_trip,to_trip,type,min_time)
		VALUES('11212','11212','tripA','tripB',4,0);
	`)
	ix := Open(db, time.UTC)
	ctx := context.Background()

	got, err := ix.TransferTime(ctx, "11212", "11212")
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultPolicy().DefaultTransferTime {
		t.Errorf("got %v, want the default: an in-seat row states no walking time", got)
	}

	// It does say the two trips are one vehicle.
	ok, err := ix.Continuation(ctx, "tripA", "tripB", "11212")
	if err != nil || !ok {
		t.Errorf("Continuation = %v (err %v), want true", ok, err)
	}
	if ok, _ := ix.Continuation(ctx, "tripA", "somethingElse", "11212"); ok {
		t.Error("reported a continuation between unrelated trips")
	}
}

// A caller who knows the station overrides everything derived from the feed.
// The pathway graph is cautious by design — it counts stairs at a pace chosen
// not to strand anyone — so a change a regular passenger makes in ninety seconds
// can be published as three minutes, and workable connections get discarded.
func TestStatedTransferTimeBeatsTheFeed(t *testing.T) {
	db := testDB(t, `
		INSERT INTO pathway(from_stop,to_stop,bidir,seconds) VALUES ('P8','P3',1,180);
		INSERT INTO transfer(from_stop,to_stop,type,min_time) VALUES('P8','P3',2,240);
	`)
	ctx := context.Background()

	// Untouched, the operator's four minutes wins over the graph's three.
	if got, _ := Open(db, time.UTC).TransferTime(ctx, "P8", "P3"); got != 240*time.Second {
		t.Fatalf("baseline = %v, want the published 240s", got)
	}

	// Stated: ninety seconds, and it beats both. Not floored by
	// DefaultTransferTime either — someone saying ninety seconds has measured it.
	ix := Open(db, time.UTC, WithPolicy(Policy{
		TransferTimes: map[StopPair]time.Duration{
			{From: "P8", To: "P3"}: 90 * time.Second,
		},
	}))
	if got, _ := ix.TransferTime(ctx, "P8", "P3"); got != 90*time.Second {
		t.Errorf("stated = %v, want 90s to win over both feed sources", got)
	}
	// A footbridge takes the same time in either direction.
	if got, _ := ix.TransferTime(ctx, "P3", "P8"); got != 90*time.Second {
		t.Errorf("reverse = %v, want 90s; the walk back is the same walk", got)
	}
	// An unrelated pair is untouched.
	if got, _ := ix.TransferTime(ctx, "P8", "P9"); got != DefaultPolicy().DefaultTransferTime {
		t.Errorf("unrelated pair = %v, want the default", got)
	}
}

// An explicit reverse entry wins over the one implied by symmetry: stairs down
// are not stairs up.
func TestStatedTransferDirectionsAreIndependent(t *testing.T) {
	ix := Open(testDB(t, ``), time.UTC, WithPolicy(Policy{
		TransferTimes: map[StopPair]time.Duration{
			{From: "UP", To: "DOWN"}: 60 * time.Second,
			{From: "DOWN", To: "UP"}: 150 * time.Second,
		},
	}))
	ctx := context.Background()
	if got, _ := ix.TransferTime(ctx, "UP", "DOWN"); got != 60*time.Second {
		t.Errorf("down = %v, want 60s", got)
	}
	if got, _ := ix.TransferTime(ctx, "DOWN", "UP"); got != 150*time.Second {
		t.Errorf("up = %v, want 150s, not the 60s implied by the other direction", got)
	}
}

package gtfs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kensantoso/ptv-gtfs-go/realtime"
	_ "modernc.org/sqlite"
)

// planFixture models the shape of Melbourne that broke the planner: a suburban
// station, a city terminus, and a loop station reachable only by changing.
//
//	SUB --(T1, direct)--> INT --> TERM        the "direct to the city" service
//	                      INT --(T2)--> LOOP  the connecting service
//
// T3 also runs SUB -> INT -> TERM, so direct journeys to TERM are plentiful.
// That plenty is what used to suppress the transfer search entirely.
func planFixture(t *testing.T) *Index {
	t.Helper()
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,mode,parent) VALUES
			('SUB','Suburb Station',2,NULL),
			('INT','Interchange Station',2,NULL),
			('TERM','Terminus Station',2,NULL),
			('LOOP','Loop Station',2,NULL);

		INSERT INTO route(route_id,short_name,long_name,mode) VALUES
			('R1','Suburb','Suburb - City',2),
			('R2','Loop','Loop Line',2);

		INSERT INTO calendar(service_id,dow,start_date,end_date) VALUES
			('SAT',6,'20260101','20261231');

		INSERT INTO trip(trip_id,route_id,service_id,headsign,direction,mode) VALUES
			('T1','R1','SAT','Terminus',0,2),
			('T3','R1','SAT','Terminus',0,2),
			('T2','R2','SAT','Loop',0,2),
			('T4','R2','SAT','Terminus express',0,2);

		-- T1: SUB 10:00 -> INT 10:20 -> TERM 10:30
		INSERT INTO stop_time(trip_id,stop_id,seq,arr,dep) VALUES
			('T1','SUB',1,36000,36000),
			('T1','INT',2,37200,37200),
			('T1','TERM',3,37800,37800),
		-- T3: SUB 10:10 -> INT 10:30 -> TERM 10:40
			('T3','SUB',1,36600,36600),
			('T3','INT',2,37800,37800),
			('T3','TERM',3,38400,38400),
		-- T2: INT 10:25 -> LOOP 10:35   (the only way to reach LOOP)
			('T2','INT',1,37500,37500),
			('T2','LOOP',2,38100,38100),
		-- T4: INT 10:22 -> TERM 10:26, an express finish that beats staying on T1
			('T4','INT',1,37320,37320),
			('T4','TERM',2,37560,37560);
	`)
	return Open(db, time.UTC)
}

func saturday(hhmm string) time.Time {
	t, _ := time.Parse("15:04", hhmm)
	return time.Date(2026, 8, 15, t.Hour(), t.Minute(), 0, 0, time.UTC)
}

// The bug: a destination reachable only by changing returned nothing, because
// the transfer search never ran.
func TestPlanFindsTransferOnlyDestination(t *testing.T) {
	ix := planFixture(t)
	js, err := ix.Plan(context.Background(), PlanRequest{
		FromStopID: "SUB", ToStopID: "LOOP", After: saturday("09:00"),
		MaxTransfers: 1, Rank: RankFastest, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(js) == 0 {
		t.Fatal("no journey found; LOOP is reachable by changing at INT")
	}
	j := js[0]
	if j.Transfers != 1 || len(j.Legs) != 2 {
		t.Fatalf("got %d transfers over %d legs, want one change", j.Transfers, len(j.Legs))
	}
	if j.Legs[0].TripID != "T1" || j.Legs[1].TripID != "T2" {
		t.Errorf("legs %s then %s, want T1 then T2", j.Legs[0].TripID, j.Legs[1].TripID)
	}
	if !j.Arrive.Equal(saturday("10:35")) {
		t.Errorf("arrives %v, want 10:35", j.Arrive.Format("15:04"))
	}
}

// Even where directs are plentiful, a change must still be offered so it can be
// compared. This is the case the old gate suppressed, and it only bites when the
// direct services alone fill the requested limit, which at a real station they
// almost always do.
func TestPlanOffersTransfersAlongsideDirects(t *testing.T) {
	ix := planFixture(t)
	js, err := ix.Plan(context.Background(), PlanRequest{
		FromStopID: "SUB", ToStopID: "TERM", After: saturday("09:00"),
		MaxTransfers: 1, Rank: RankFastest, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	var direct int
	for _, j := range js {
		if j.Transfers == 0 {
			direct++
		}
	}
	if direct == 0 {
		t.Fatal("no direct journeys returned")
	}
	if len(js) == direct {
		t.Error("only direct journeys returned; the transfer search did not run despite directs existing")
	}
}

// Via is a constraint: it returns journeys changing there, and not the non-stop
// services that would otherwise dominate the ranking.
func TestPlanViaExcludesDirects(t *testing.T) {
	ix := planFixture(t)
	js, err := ix.Plan(context.Background(), PlanRequest{
		FromStopID: "SUB", ToStopID: "TERM", After: saturday("09:00"),
		MaxTransfers: 1, Rank: RankFastest, Limit: 10, Via: "INT",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(js) == 0 {
		t.Fatal("no journey found changing at INT")
	}
	for _, j := range js {
		if j.Transfers == 0 {
			t.Errorf("a direct journey was returned for a via request: %s", j.Legs[0].TripID)
		}
		if j.Legs[0].ToStop != "INT" {
			t.Errorf("changed at %s, want INT", j.Legs[0].ToStop)
		}
	}
}

// An unreachable via must return nothing rather than quietly routing elsewhere.
func TestPlanViaUnreachableReturnsNothing(t *testing.T) {
	ix := planFixture(t)
	js, err := ix.Plan(context.Background(), PlanRequest{
		FromStopID: "SUB", ToStopID: "TERM", After: saturday("09:00"),
		MaxTransfers: 1, Rank: RankFastest, Limit: 10, Via: "LOOP",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(js) != 0 {
		t.Fatalf("got %d journeys via LOOP, want none: nothing runs onward from there", len(js))
	}
}

// You arrive at an interchange on one platform and leave from another. Matching
// candidates on stop id therefore finds nothing at exactly the stations where a
// change is most useful, and the search silently falls back to whichever
// terminus happens to reuse platform ids.
//
// This is not hypothetical: it hid Richmond completely on Melbourne's eastern
// lines, leaving Flinders Street as the only candidate and every answer five
// minutes slower than the real best.
func TestInterchangeMatchesAcrossPlatforms(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,mode,parent) VALUES
			('SUB','Suburb Station',2,NULL),
			('X','Interchange Railway Station',2,NULL),
			('X1','Interchange Station',2,'X'),
			('X2','Interchange Station',2,'X'),
			('DEST','Destination Station',2,NULL);

		INSERT INTO route(route_id,short_name,long_name,mode) VALUES ('R','L','Line',2);
		INSERT INTO calendar(service_id,dow,start_date,end_date) VALUES ('SAT',6,'20260101','20261231');

		INSERT INTO trip(trip_id,route_id,service_id,headsign,direction,mode) VALUES
			('IN','R','SAT','Interchange',0,2),
			('OUT','R','SAT','Destination',0,2);

		-- arrives on platform 1
		INSERT INTO stop_time(trip_id,stop_id,seq,arr,dep) VALUES
			('IN','SUB',1,36000,36000),
			('IN','X1',2,36600,36600),
		-- departs from platform 2
			('OUT','X2',1,36900,36900),
			('OUT','DEST',2,37500,37500);
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
		t.Fatal("no journey found; the change works, it just uses two platforms of one station")
	}
	if js[0].Legs[0].ToStop != "X1" || js[0].Legs[1].FromStop != "X2" {
		t.Errorf("changed %s -> %s, want X1 -> X2", js[0].Legs[0].ToStop, js[0].Legs[1].FromStop)
	}
}

// Passing through the loop is counted; starting or ending at one of its
// stations is not, because that is a destination rather than a detour.
func TestCityLoopStopsCountsPassingThroughOnly(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,mode,parent) VALUES
			('vic:rail:FSS','Flinders Street',2,NULL),
			('vic:rail:FGS','Flagstaff',2,NULL),
			('vic:rail:MCE','Melbourne Central',2,NULL),
			('vic:rail:PAR','Parliament',2,NULL),
			('OUT','Suburb',2,NULL);
		INSERT INTO stop_time(trip_id,stop_id,seq,arr,dep) VALUES
			('LOOP','vic:rail:FSS',1,0,0),
			('LOOP','vic:rail:FGS',2,60,60),
			('LOOP','vic:rail:MCE',3,120,120),
			('LOOP','vic:rail:PAR',4,180,180),
			('LOOP','OUT',5,600,600);
	`)
	ix := Open(db, time.UTC)
	ctx := context.Background()

	// Flinders Street to the suburb, round the loop: three stations passed.
	through := Journey{Legs: []Leg{{TripID: "LOOP", FromSeq: 1, ToSeq: 5}}}
	if n, err := ix.CityLoopStops(ctx, through); err != nil || n != 3 {
		t.Errorf("passing through the loop = %d (err %v), want 3", n, err)
	}

	// Travelling to Parliament is not a detour, it is the destination.
	toLoop := Journey{Legs: []Leg{{TripID: "LOOP", FromSeq: 1, ToSeq: 4}}}
	if n, err := ix.CityLoopStops(ctx, toLoop); err != nil || n != 2 {
		t.Errorf("to Parliament = %d (err %v), want 2: Flagstaff and Melbourne Central are passed, Parliament is the destination", n, err)
	}

	// Boarding at Parliament and heading out passes none of them.
	fromLoop := Journey{Legs: []Leg{{TripID: "LOOP", FromSeq: 4, ToSeq: 5}}}
	if n, err := ix.CityLoopStops(ctx, fromLoop); err != nil || n != 0 {
		t.Errorf("from Parliament outbound = %d (err %v), want 0", n, err)
	}
}

// A bus that sets down outside a station shares no edge with it in this feed:
// different mode, no shared parent, nothing in transfers.txt or pathways.txt.
// Without a walking edge the planner reports no journey for a connection every
// passenger makes without thinking about it.
func TestPlanWalksBetweenNearbyStops(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,lat,lon,mode,parent) VALUES
			('busA','Origin/Main Rd',-37.8000,145.0000,4,NULL),
			('busB','Station/Main Rd',-37.8200,145.0000,4,NULL),
			('vic:rail:X','X Railway Station',-37.8206,145.0000,2,NULL),
			('x1','X Station',-37.8206,145.0000,2,'vic:rail:X'),
			('vic:rail:Y','Y Railway Station',-37.8500,145.0000,2,NULL),
			('y1','Y Station',-37.8500,145.0000,2,'vic:rail:Y');

		INSERT INTO route(route_id,short_name,long_name,mode) VALUES
			('B','bus','Bus Route',4), ('T','train','Train Line',2);
		INSERT INTO calendar(service_id,dow,start_date,end_date) VALUES ('SAT',6,'20260101','20261231');
		INSERT INTO trip(trip_id,route_id,service_id,headsign,direction,mode) VALUES
			('bus1','B','SAT','Station',0,4),
			('trn1','T','SAT','Y',0,2);

		INSERT INTO stop_time(trip_id,stop_id,seq,arr,dep) VALUES
			('bus1','busA',1,36000,36000),
			('bus1','busB',2,36300,36300),
			('trn1','x1',1,36900,36900),
			('trn1','y1',2,37500,37500);
	`)
	ix := Open(db, time.UTC)

	js, err := ix.Plan(context.Background(), PlanRequest{
		FromStopID: "busA", ToStopID: "vic:rail:Y", After: saturday("09:00"),
		MaxTransfers: 1, Rank: RankFastest, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(js) == 0 {
		t.Fatal("no journey; the bus stops about 70m from the station and a walk connects them")
	}
	var walked bool
	for _, l := range js[0].Legs {
		if l.Walk {
			walked = true
			if l.WalkMetres <= 0 || l.WalkMetres > DefaultPolicy().WalkRadius {
				t.Errorf("walk of %.0fm is outside the allowed radius", l.WalkMetres)
			}
			if l.TripID != "" {
				t.Error("a walking leg must not carry a trip id")
			}
		}
	}
	if !walked {
		t.Error("journey found but with no walking leg")
	}
}

// Walking must be refusable: a caller who cannot walk should get the old
// behaviour rather than a plan that assumes they can.
func TestPlanWalkingCanBeDisabled(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,lat,lon,mode,parent) VALUES
			('busA','Origin/Main Rd',-37.8000,145.0000,4,NULL),
			('busB','Station/Main Rd',-37.8200,145.0000,4,NULL),
			('vic:rail:X','X Railway Station',-37.8206,145.0000,2,NULL),
			('x1','X Station',-37.8206,145.0000,2,'vic:rail:X'),
			('vic:rail:Y','Y Railway Station',-37.8500,145.0000,2,NULL),
			('y1','Y Station',-37.8500,145.0000,2,'vic:rail:Y');
		INSERT INTO route(route_id,short_name,long_name,mode) VALUES
			('B','bus','Bus Route',4), ('T','train','Train Line',2);
		INSERT INTO calendar(service_id,dow,start_date,end_date) VALUES ('SAT',6,'20260101','20261231');
		INSERT INTO trip(trip_id,route_id,service_id,headsign,direction,mode) VALUES
			('bus1','B','SAT','Station',0,4), ('trn1','T','SAT','Y',0,2);
		INSERT INTO stop_time(trip_id,stop_id,seq,arr,dep) VALUES
			('bus1','busA',1,36000,36000), ('bus1','busB',2,36300,36300),
			('trn1','x1',1,36900,36900), ('trn1','y1',2,37500,37500);
	`)
	ix := Open(db, time.UTC)
	js, err := ix.Plan(context.Background(), PlanRequest{
		FromStopID: "busA", ToStopID: "vic:rail:Y", After: saturday("09:00"),
		MaxTransfers: 1, Rank: RankFastest, Limit: 5, MaxWalk: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(js) != 0 {
		t.Fatalf("got %d journeys with walking disabled, want none", len(js))
	}
}

// The wait at a change is what remains after walking. Reporting the raw gap
// describes time the passenger does not have.
func TestWaitAtTransferExcludesWalking(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,lat,lon,mode,parent) VALUES
			('busA','Origin/Main Rd',-37.8000,145.0000,4,NULL),
			('busB','Station/Main Rd',-37.8200,145.0000,4,NULL),
			('vic:rail:X','X Railway Station',-37.8206,145.0000,2,NULL),
			('x1','X Station',-37.8206,145.0000,2,'vic:rail:X'),
			('vic:rail:Y','Y Railway Station',-37.8500,145.0000,2,NULL),
			('y1','Y Station',-37.8500,145.0000,2,'vic:rail:Y');
		INSERT INTO route(route_id,short_name,long_name,mode) VALUES
			('B','bus','Bus Route',4), ('T','train','Train Line',2);
		INSERT INTO calendar(service_id,dow,start_date,end_date) VALUES ('SAT',6,'20260101','20261231');
		INSERT INTO trip(trip_id,route_id,service_id,headsign,direction,mode) VALUES
			('bus1','B','SAT','Station',0,4), ('trn1','T','SAT','Y',0,2);
		INSERT INTO stop_time(trip_id,stop_id,seq,arr,dep) VALUES
			('bus1','busA',1,36000,36000),
			('bus1','busB',2,36300,36300),
			('trn1','x1',1,36900,36900),
			('trn1','y1',2,37500,37500);
	`)
	ix := Open(db, time.UTC)
	js, err := ix.Plan(context.Background(), PlanRequest{
		FromStopID: "busA", ToStopID: "vic:rail:Y", After: saturday("09:00"),
		MaxTransfers: 1, Rank: RankFastest, Limit: 3,
	})
	if err != nil || len(js) == 0 {
		t.Fatalf("no journey (err %v)", err)
	}
	j := js[0]
	var onFoot time.Duration
	for _, l := range j.Legs {
		if l.Walk {
			onFoot += l.Arrive.Sub(l.Depart)
		}
	}
	if onFoot == 0 {
		t.Fatal("expected a walking leg in this fixture")
	}
	raw := j.Legs[len(j.Legs)-1].Depart.Sub(j.Legs[0].Arrive)
	if j.WaitAtTransfer[0] != raw-onFoot {
		t.Errorf("wait = %v, want %v (a %v gap less a %v walk)",
			j.WaitAtTransfer[0], raw-onFoot, raw, onFoot)
	}
}

// The planner used to offer the same train several times over: board a
// different service, ride one stop into the City Loop, get off, and board the
// train you could have stayed on. Same arrival, an extra change, and sometimes
// an earlier departure to achieve nothing.
func TestFilterDominatedDropsStrictlyWorseJourneys(t *testing.T) {
	direct := Journey{Depart: at("20:20"), Arrive: at("20:40"), Transfers: 0}
	viaLoop := Journey{Depart: at("20:20"), Arrive: at("20:40"), Transfers: 1}
	earlier := Journey{Depart: at("20:18"), Arrive: at("20:40"), Transfers: 1}
	later := Journey{Depart: at("20:35"), Arrive: at("20:55"), Transfers: 0}

	got := filterDominated([]Journey{direct, viaLoop, earlier, later})
	if len(got) != 2 {
		t.Fatalf("got %d journeys, want 2: only the direct and the next service survive", len(got))
	}
	if got[0].Transfers != 0 || !got[0].Depart.Equal(at("20:20")) {
		t.Errorf("first survivor is %v with %d changes, want the 20:20 direct",
			got[0].Depart.Format("15:04"), got[0].Transfers)
	}
	if !got[1].Depart.Equal(at("20:35")) {
		t.Errorf("second survivor departs %v, want 20:35", got[1].Depart.Format("15:04"))
	}
}

// A genuine trade must survive: leaving later and arriving later is a choice,
// not a worse version of the same journey.
func TestFilterDominatedKeepsRealTrades(t *testing.T) {
	soon := Journey{Depart: at("20:20"), Arrive: at("20:50"), Transfers: 1}
	quicker := Journey{Depart: at("20:35"), Arrive: at("20:55"), Transfers: 0}
	if got := filterDominated([]Journey{soon, quicker}); len(got) != 2 {
		t.Fatalf("got %d, want both: one arrives sooner, the other needs no change", len(got))
	}

	// Identical on every axis: keep one, not both.
	a := Journey{Depart: at("20:20"), Arrive: at("20:40"), Transfers: 0}
	b := Journey{Depart: at("20:20"), Arrive: at("20:40"), Transfers: 0}
	if got := filterDominated([]Journey{a, b}); len(got) != 2 {
		t.Errorf("got %d for two equal journeys, want both kept; dedupe removes real duplicates", len(got))
	}
}

// Bounded patience is what makes "least time travelling" useful. Unbounded, the
// shortest journey in the window may leave in ninety minutes to save three.
func TestPlanMaxWaitBoundsTheSearch(t *testing.T) {
	ix := planFixture(t)
	base := PlanRequest{
		FromStopID: "SUB", ToStopID: "TERM", After: saturday("09:00"),
		MaxTransfers: 1, Rank: RankShortest, Limit: 10,
	}

	all, err := ix.Plan(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no journeys at all")
	}

	// The fixture's first service leaves at 10:00, an hour after the request.
	tight := base
	tight.MaxWait = 30 * time.Minute
	got, err := ix.Plan(context.Background(), tight)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d journeys within 30 minutes, want none: nothing leaves before 10:00", len(got))
	}

	generous := base
	generous.MaxWait = 90 * time.Minute
	got, err = ix.Plan(context.Background(), generous)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("got none within 90 minutes, want the 10:00 services")
	}
	for _, j := range got {
		if j.Depart.After(saturday("10:30")) {
			t.Errorf("journey departs %v, past the 90 minute bound", j.Depart.Format("15:04"))
		}
	}
}

// Where two services share several stops, changing at any of them gives an
// identical journey. Listing each as a separate option describes one journey
// several times.
func TestDedupeCollapsesEquivalentChangePoints(t *testing.T) {
	mk := func(trip1, mid, trip2 string) Journey {
		return Journey{
			Legs: []Leg{
				{TripID: trip1, FromStop: "A", ToStop: mid, Depart: at("16:36"), Arrive: at("16:40")},
				{TripID: trip2, FromStop: mid, ToStop: "B", Depart: at("16:42"), Arrive: at("16:51")},
			},
			Depart: at("16:36"), Arrive: at("16:51"), Transfers: 1,
		}
	}
	// The same two trains, stepped off at three stops they both call at.
	got := dedupeJourneys([]Journey{
		mk("t1", "Richmond", "t2"),
		mk("t1", "EastRichmond", "t2"),
		mk("t1", "Burnley", "t2"),
	})
	if len(got) != 1 {
		t.Fatalf("got %d journeys, want 1: same trains, same times, different place to step off", len(got))
	}
}

// A genuinely different journey must still survive, even at the same times.
func TestDedupeKeepsDifferentRideCounts(t *testing.T) {
	direct := Journey{
		Legs:   []Leg{{TripID: "d", FromStop: "A", ToStop: "B", Depart: at("16:36"), Arrive: at("16:51")}},
		Depart: at("16:36"), Arrive: at("16:51"),
	}
	changed := Journey{
		Legs: []Leg{
			{TripID: "x", FromStop: "A", ToStop: "M", Depart: at("16:36"), Arrive: at("16:40")},
			{TripID: "y", FromStop: "M", ToStop: "B", Depart: at("16:42"), Arrive: at("16:51")},
		},
		Depart: at("16:36"), Arrive: at("16:51"), Transfers: 1,
	}
	if got := dedupeJourneys([]Journey{direct, changed}); len(got) != 2 {
		t.Fatalf("got %d, want both: one needs a change and the other does not", len(got))
	}
}

// A feed covers a fixed timetable period. Past its end every query matches no
// services, and without this the caller cannot tell "nothing runs then" from
// "my data ran out in November".
func TestQueriesOutsideTheFeedPeriodAreRejected(t *testing.T) {
	ix := planFixture(t)
	ctx := context.Background()
	if _, err := ix.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key,value) VALUES('valid_from','20260101'),('valid_to','20261231')`); err != nil {
		t.Fatal(err)
	}

	inside := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	if err := ix.checkRange(ctx, inside); err != nil {
		t.Errorf("a date inside the period was rejected: %v", err)
	}

	after := time.Date(2027, 1, 5, 9, 0, 0, 0, time.UTC)
	err := ix.checkRange(ctx, after)
	var fre *FeedRangeError
	if !errors.As(err, &fre) {
		t.Fatalf("got %v, want a FeedRangeError for a date past the end", err)
	}
	if !strings.Contains(fre.Error(), "rebuild") {
		t.Errorf("the error should say what to do: %q", fre.Error())
	}

	// The planner must surface it rather than returning an empty list.
	if _, err := ix.Plan(ctx, PlanRequest{
		FromStopID: "SUB", ToStopID: "TERM", After: after, MaxTransfers: 1,
	}); !errors.As(err, &fre) {
		t.Errorf("Plan returned %v, want a FeedRangeError", err)
	}
	if _, err := ix.Departures(ctx, DeparturesRequest{StopID: "SUB", After: after}); !errors.As(err, &fre) {
		t.Errorf("Departures returned %v, want a FeedRangeError", err)
	}
}

// An index with no recorded period must keep working rather than refuse
// everything: it predates this being written down.
func TestNoRecordedPeriodIsNotAnError(t *testing.T) {
	ix := planFixture(t)
	ctx := context.Background()
	ix.db.ExecContext(ctx, `DELETE FROM meta`)
	ix.db.ExecContext(ctx, `DELETE FROM calendar`)
	if err := ix.checkRange(ctx, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Errorf("an index with no period should not reject queries: %v", err)
	}
}

// A caller must be able to disagree with the judgements this package makes.
func TestPolicyIsHonoured(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,lat,lon,mode,parent) VALUES
			('busA','Origin',-37.8000,145.0000,4,NULL),
			('busB','Near Station',-37.8200,145.0000,4,NULL),
			('vic:rail:X','X Railway Station',-37.8206,145.0000,2,NULL),
			('x1','X Station',-37.8206,145.0000,2,'vic:rail:X'),
			('vic:rail:Y','Y Railway Station',-37.8500,145.0000,2,NULL),
			('y1','Y Station',-37.8500,145.0000,2,'vic:rail:Y');
		INSERT INTO route(route_id,short_name,long_name,mode) VALUES ('B','b','Bus',4),('T','t','Train',2);
		INSERT INTO calendar(service_id,dow,start_date,end_date) VALUES ('SAT',6,'20260101','20261231');
		INSERT INTO trip(trip_id,route_id,service_id,headsign,direction,mode) VALUES
			('bus1','B','SAT','Station',0,4),('trn1','T','SAT','Y',0,2);
		INSERT INTO stop_time(trip_id,stop_id,seq,arr,dep) VALUES
			('bus1','busA',1,36000,36000),('bus1','busB',2,36300,36300),
			('trn1','x1',1,36900,36900),('trn1','y1',2,37500,37500);
	`)
	req := PlanRequest{FromStopID: "busA", ToStopID: "vic:rail:Y",
		After: saturday("09:00"), MaxTransfers: 1, Limit: 5}

	// The default radius reaches the station about 70m away.
	if js, err := Open(db, time.UTC).Plan(context.Background(), req); err != nil || len(js) == 0 {
		t.Fatalf("default policy found no journey (err %v)", err)
	}

	// A caller who will not walk gets no walking connections.
	noWalk := Open(db, time.UTC, WithPolicy(Policy{WalkRadius: -1}))
	if js, err := noWalk.Plan(context.Background(), req); err != nil || len(js) != 0 {
		t.Errorf("got %d journeys with walking refused, want none (err %v)", len(js), err)
	}

	// Unset fields keep their default rather than being taken as zero.
	partial := Open(db, time.UTC, WithPolicy(Policy{OnTimeThreshold: 5 * time.Minute}))
	if got := partial.Policy().WalkRadius; got != DefaultPolicy().WalkRadius {
		t.Errorf("WalkRadius = %v, want the default %v", got, DefaultPolicy().WalkRadius)
	}
	if got := partial.Policy().OnTimeThreshold; got != 5*time.Minute {
		t.Errorf("OnTimeThreshold = %v, want the override", got)
	}
}

// The on-time threshold is the caller's to set: a punctuality dashboard wants
// nothing rounded away.
func TestOnTimeThresholdIsConfigurable(t *testing.T) {
	data := &realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "T1", Stops: []realtime.StopUpdate{{Sequence: 5, Departure: secs(90)}}},
	}}
	d := dep("T1", 5, "S", "08:00")

	if got := NewLive(data).Departure(d); got.Status != StatusLate {
		t.Errorf("default: %v, want late at 90s", got.Status)
	}
	strict := NewLiveWithPolicy(Policy{OnTimeThreshold: time.Second}, data)
	if got := strict.Departure(d); got.Status != StatusLate {
		t.Errorf("strict: %v, want late", got.Status)
	}
	loose := NewLiveWithPolicy(Policy{OnTimeThreshold: 5 * time.Minute}, data)
	if got := loose.Departure(d); got.Status != StatusOnTime {
		t.Errorf("loose: %v, want on time under a five minute threshold", got.Status)
	}
}

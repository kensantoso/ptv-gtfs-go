package gtfs

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	// Test-only, so importers are not made to carry it: these tests load a
	// named zone, and a CI container may have no tzdata installed.
	_ "time/tzdata"
)

var record = flag.Bool("record", false, "re-record the schedule baseline")

// These are characterisation tests over the real feed.
//
// Every other test here is a synthetic fixture proving a mechanism: that the
// transfer search runs, that platforms are matched by station, that a walk is
// costed. None of them asserts an answer. The results that justify this
// package's existence are answers — specific journeys, on specific days, that
// the obvious route misses — and a refactor could leave every mechanism test
// green while quietly returning something worse.
//
// They are skipped when no index is present, so they never break a build that
// has no 1.5GB database. Run them by building one first.
//
// They will also expire. The feed they describe is valid to 15 November 2026,
// and after that the assertions are about a timetable that no longer runs; the
// range check makes that fail loudly rather than silently.
func indexOrSkip(t *testing.T) (*Index, time.Time) {
	t.Helper()
	// UserCacheDir, matching where the server writes it: ~/Library/Caches on
	// darwin, ~/.cache on Linux, %LocalAppData% on Windows. Hardcoding the
	// darwin path meant these could never run anywhere else.
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Skip("no cache directory")
	}
	path := filepath.Join(cache, "ptv-mcp", "ptv.db")
	if _, err := os.Stat(path); err != nil {
		t.Skip("no local index; build one to run the integration tests")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Skip("index unreadable")
	}
	t.Cleanup(func() { db.Close() })

	loc, err := time.LoadLocation("Australia/Melbourne")
	if err != nil {
		t.Skip("no Melbourne timezone")
	}
	ix := Open(db, loc)

	// Tuesday 18 August 2026, inside the feed's period.
	when := time.Date(2026, 8, 18, 0, 0, 0, 0, loc)
	if _, to, _ := ix.Validity(context.Background()); !to.IsZero() && when.After(to) {
		t.Skipf("the index expired on %s; rebuild it to run the integration tests", to.Format("2 Jan 2006"))
	}
	return ix, when
}

func at2(base time.Time, hhmm string) time.Time {
	t, _ := time.Parse("15:04", hhmm)
	return time.Date(base.Year(), base.Month(), base.Day(), t.Hour(), t.Minute(), 0, 0, base.Location())
}

// best returns the quickest journey and the id of where it changes.
//
// The id rather than the name, which keeps the baseline terse and stable
// across renamings. It is not concealment: these ids are public and decode in
// one lookup. What keeps the set impersonal is the choice of journeys, spread
// across corridors, with no station appearing as an endpoint twice.
func best(t *testing.T, ix *Index, from, to string, when time.Time) (Journey, string) {
	t.Helper()
	js, err := ix.Plan(context.Background(), PlanRequest{
		FromStopID: from, ToStopID: to, After: when,
		MaxTransfers: 1, Rank: RankFastest, Limit: 5,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(js) == 0 {
		t.Fatal("no journey found")
	}
	j := js[0]
	via := "direct"
	if j.Transfers > 0 {
		var station string
		if err := ix.db.QueryRowContext(context.Background(),
			`SELECT COALESCE(NULLIF(parent,''), stop_id) FROM stop WHERE stop_id=?`,
			j.Legs[0].ToStop).Scan(&station); err == nil {
			via = station
		} else {
			via = j.Legs[0].ToStop
		}
	}
	return j, via
}

// Canonical journeys whose answers are worth noticing a change in. Each is a
// case where the obvious route is not the best one, so a shift in any of them
// means either the timetable moved or the planner did.
var canonical = []struct {
	name, from, to, when string
}{
	// Spread deliberately across the network. A set drawn from one corridor
	// tests less, and says more about whoever wrote it than about the planner.
	// Interchanges may repeat, since those are the planner's findings; endpoints
	// do not.
	{"west: city to a station the expresses pass", "vic:rail:FSS", "vic:rail:LAV", "15:00"},
	{"west: city to the junction, evening", "vic:rail:FSS", "vic:rail:NPT", "17:30"},
	{"north: city outbound, morning", "vic:rail:FSS", "vic:rail:BMS", "08:00"},
	{"north: city outbound, evening", "vic:rail:SSS", "vic:rail:COB", "17:30"},
	{"east: city to a station the expresses skip, afternoon", "vic:rail:PAR", "vic:rail:ECM", "16:30"},
	{"bayside: suburb to a loop station, evening", "vic:rail:NBN", "vic:rail:PAR", "17:30"},
	{"across the city, midday", "vic:rail:SSS", "vic:rail:FSS", "12:30"},
	{"regional gateway, morning", "vic:rail:FSS", "vic:rail:SUN", "08:00"},
}

type answer struct {
	Depart, Arrive string
	Minutes        int
	Changes        int
	Via            string
	LoopStops      int
}

func compute(t *testing.T, ix *Index, day time.Time) map[string]answer {
	t.Helper()
	out := map[string]answer{}
	for _, c := range canonical {
		j, via := best(t, ix, c.from, c.to, at2(day, c.when))
		loop, err := ix.CityLoopStops(context.Background(), j)
		if err != nil {
			t.Fatal(err)
		}
		out[c.name] = answer{
			Depart: j.Depart.Format("15:04"), Arrive: j.Arrive.Format("15:04"),
			Minutes: int(j.Duration().Minutes()), Changes: j.Transfers,
			Via: via, LoopStops: loop,
		}
	}
	return out
}

// TestScheduleHasNotChanged compares the current answers against a recorded
// baseline.
//
// This is not really testing the code. It is a canary for the data: PTV
// republishes the feed weekly and reshapes it at timetable boundaries, and a
// change to any of these journeys is worth seeing rather than discovering
// months later. A failure here is information, not necessarily a defect — read
// the difference, decide whether it is the timetable or a regression, and
// re-record if it is the timetable.
//
//	go test -run TestScheduleHasNotChanged -record
func TestScheduleHasNotChanged(t *testing.T) {
	ix, day := indexOrSkip(t)
	got := compute(t, ix, day)

	const path = "testdata/schedule_baseline.json"
	if *record {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %d journeys to %s", len(got), path)
		return
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no baseline recorded; run with -record to create one")
	}
	var want map[string]answer
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}

	for _, c := range canonical {
		w, ok := want[c.name]
		if !ok {
			t.Errorf("%s: no baseline; re-record", c.name)
			continue
		}
		g := got[c.name]
		if g != w {
			t.Errorf("%s changed:\n   was  %s->%s  %2dmin  %d change  via %-16s loop %d"+
				"\n   now  %s->%s  %2dmin  %d change  via %-16s loop %d",
				c.name,
				w.Depart, w.Arrive, w.Minutes, w.Changes, w.Via, w.LoopStops,
				g.Depart, g.Arrive, g.Minutes, g.Changes, g.Via, g.LoopStops)
		}
	}
}

// The properties below should hold whatever the timetable does. They are the
// claims this planner makes, rather than the numbers it happens to produce.

// A bus stop outside a station shares no edge with it in this feed. Without a
// synthetic walking edge the planner reports no journey at all.
func TestBusReachesTheTrainNetwork(t *testing.T) {
	ix, day := indexOrSkip(t)
	ctx := context.Background()

	var busStop string
	err := ix.db.QueryRowContext(ctx, `
		SELECT s.stop_id FROM stop s
		WHERE s.mode = 4 AND s.lat <> 0
		  AND EXISTS (SELECT 1 FROM stop_time st WHERE st.stop_id = s.stop_id)
		  AND EXISTS (
		    SELECT 1 FROM stop r
		    WHERE r.mode = 2 AND r.parent = ''
		      AND ABS(r.lat - s.lat) < 0.001 AND ABS(r.lon - s.lon) < 0.001)
		LIMIT 1`).Scan(&busStop)
	if err != nil {
		t.Skip("no bus stop found beside a station in this index")
	}
	js, err := ix.Plan(ctx, PlanRequest{
		FromStopID: busStop, ToStopID: "vic:rail:FSS", After: at2(day, "08:00"),
		MaxTransfers: 1, Rank: RankFastest, Limit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(js) == 0 {
		t.Fatalf("no journey from bus stop %s to the city; walking connections have regressed", busStop)
	}
}

// Leaving the city across the loop, some options go round it and some do not.
// If every option reports the same count, the measure has broken.
func TestLoopStopsDistinguishOptions(t *testing.T) {
	ix, day := indexOrSkip(t)
	js, err := ix.Plan(context.Background(), PlanRequest{
		FromStopID: "vic:rail:SSS", ToStopID: "vic:rail:MRN", After: at2(day, "17:30"),
		MaxTransfers: 1, Rank: RankFastest, Limit: 6,
	})
	if err != nil || len(js) < 2 {
		t.Skipf("need several options to compare (got %d, err %v)", len(js), err)
	}
	seen := map[int]bool{}
	for _, j := range js {
		n, err := ix.CityLoopStops(context.Background(), j)
		if err != nil {
			t.Fatal(err)
		}
		seen[n] = true
	}
	if len(seen) < 2 {
		t.Errorf("every option passes the same number of loop stations (%v)", seen)
	}
}

// Waiting a little can buy a better journey, and the planner has to be able to
// say so. Leaving now means whatever is next, which may be a stopper needing a
// change; a few minutes' patience can put you on a direct service that is also
// quicker end to end.
//
// This is the pairing of RankShortest with MaxWait. Unbounded, "least time
// travelling" will happily send you off to wait ninety minutes to save three,
// which is why the bound is part of the question rather than an afterthought.
func TestWaitingBuysABetterJourney(t *testing.T) {
	ix, day := indexOrSkip(t)
	ctx := context.Background()
	when := at2(day, "15:00")

	plan := func(r Rank) Journey {
		js, err := ix.Plan(ctx, PlanRequest{
			FromStopID: "vic:rail:FSS", ToStopID: "vic:rail:LAV", After: when,
			MaxTransfers: 1, Rank: r, Limit: 1, MaxWait: 30 * time.Minute,
		})
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(js) == 0 {
			t.Skip("no journey in the window; the timetable has moved")
		}
		return js[0]
	}

	soonest, quickest := plan(RankFastest), plan(RankShortest)

	if soonest.Depart.Equal(quickest.Depart) && soonest.Arrive.Equal(quickest.Arrive) {
		t.Skip("the soonest service is also the quickest here; nothing to trade")
	}

	// The whole point: less time on board, at the price of leaving later.
	if quickest.Duration() > soonest.Duration() {
		t.Errorf("the shortest option travels for %v against the soonest option's %v; "+
			"ranking by duration has regressed",
			quickest.Duration().Round(time.Minute), soonest.Duration().Round(time.Minute))
	}
	if quickest.Depart.Before(soonest.Depart) {
		t.Errorf("the shortest option leaves at %s, before the soonest at %s; "+
			"then it would simply be the soonest",
			quickest.Depart.Format("15:04"), soonest.Depart.Format("15:04"))
	}

	// And the bound must actually bind.
	if wait := quickest.Depart.Sub(when); wait > 30*time.Minute {
		t.Errorf("the chosen journey leaves in %v, past the thirty minute bound", wait.Round(time.Minute))
	}

	// Fewer changes is the form this usually takes: the wait buys a direct run.
	if quickest.Transfers > soonest.Transfers {
		t.Errorf("waiting produced %d changes against %d; that is not a better journey",
			quickest.Transfers, soonest.Transfers)
	}

	t.Logf("leave now: %s->%s %v, %d change; wait: %s->%s %v, %d change",
		soonest.Depart.Format("15:04"), soonest.Arrive.Format("15:04"),
		soonest.Duration().Round(time.Minute), soonest.Transfers,
		quickest.Depart.Format("15:04"), quickest.Arrive.Format("15:04"),
		quickest.Duration().Round(time.Minute), quickest.Transfers)
}

// An unbounded search for the shortest journey is not useful: it will wait an
// hour to save a few minutes. The bound is what makes the question answerable.
func TestWaitBoundActuallyBounds(t *testing.T) {
	ix, day := indexOrSkip(t)
	ctx := context.Background()
	when := at2(day, "15:00")

	req := PlanRequest{
		FromStopID: "vic:rail:FSS", ToStopID: "vic:rail:LAV", After: when,
		MaxTransfers: 1, Rank: RankShortest, Limit: 1,
	}
	unbounded, err := ix.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.MaxWait = 15 * time.Minute
	bounded, err := ix.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(unbounded) == 0 || len(bounded) == 0 {
		t.Skip("no journeys to compare")
	}
	if w := bounded[0].Depart.Sub(when); w > 15*time.Minute {
		t.Errorf("bounded search returned a departure %v away, past the bound", w.Round(time.Minute))
	}
	// The unbounded answer may leave much later; that is the behaviour the bound exists to tame.
	if unbounded[0].Depart.Sub(when) < bounded[0].Depart.Sub(when) {
		t.Errorf("the unbounded search leaves earlier than the bounded one, which cannot be right")
	}
}

// A bus replacing a train is published on the train mode, so a journey can be
// labelled "metro train" and be a bus at the kerb. Sending someone to a
// platform when the service leaves from the forecourt strands them.
//
// Replacement services carry ordinary calendar rows with narrow date ranges,
// often a single day, rather than calendar_dates exceptions. Finding one means
// asking the calendar for a day it actually runs.
func TestReplacementBusesAreMarked(t *testing.T) {
	ix, _ := indexOrSkip(t)
	ctx := context.Background()

	var stop, from string
	var dow int
	err := ix.db.QueryRowContext(ctx, `
		SELECT st.stop_id, c.dow, c.start_date
		FROM stop_time st
		JOIN trip t USING(trip_id)
		JOIN route r ON r.route_id = t.route_id
		JOIN calendar c ON c.service_id = t.service_id
		WHERE r.route_id LIKE '%-R:' AND t.mode = 2
		LIMIT 1`).Scan(&stop, &dow, &from)
	if err != nil {
		t.Skip("no replacement services in this index")
	}
	start, err := time.ParseInLocation("20060102", from, ix.loc)
	if err != nil {
		t.Skip("unparseable calendar date")
	}
	// Walk forward to the first day matching the pattern's weekday.
	day := start
	for i := 0; int(day.Weekday()) != dow && i < 7; i++ {
		day = day.AddDate(0, 0, 1)
	}

	deps, err := ix.Departures(ctx, DeparturesRequest{
		StopID: stop, After: day.Add(4 * time.Hour), Within: 18 * time.Hour, Limit: 300,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) == 0 {
		t.Skipf("no departures at %s on %s", stop, day.Format("2006-01-02"))
	}

	var marked int
	for _, d := range deps {
		if d.Replacement {
			marked++
			if d.Mode != ModeMetroTrain {
				t.Errorf("replacement service on mode %v; these are published on the train mode", d.Mode)
			}
		}
	}
	if marked == 0 {
		t.Errorf("%d departures at a replacement bus stop on %s and none marked as a replacement; "+
			"a passenger would be told to stand on a platform", len(deps), day.Format("Mon 2 Jan"))
	}
	t.Logf("%d of %d departures at %s on %s marked as replacement buses",
		marked, len(deps), stop, day.Format("Mon 2 Jan"))
}

package gtfs

import (
	"testing"
	"time"

	"github.com/kensantoso/ptv-gtfs-go/realtime"
)

var testLoc = time.FixedZone("AEST", 10*3600)

func at(hhmm string) time.Time {
	t, err := time.ParseInLocation("15:04", hhmm, testLoc)
	if err != nil {
		panic(err)
	}
	return time.Date(2026, 8, 15, t.Hour(), t.Minute(), 0, 0, testLoc)
}

func dep(tripID string, seq int, stopID, hhmm string) Departure {
	return Departure{TripID: tripID, Seq: seq, StopID: stopID, Depart: at(hhmm)}
}

func secs(n int) realtime.StopEvent {
	return realtime.StopEvent{Set: true, Delay: time.Duration(n) * time.Second}
}

// The rule the whole package turns on: no data is not the same as on time.
func TestDepartureWithoutRealtimeIsUnknown(t *testing.T) {
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "other", Stops: []realtime.StopUpdate{{Sequence: 1, Departure: secs(0)}}},
	}})

	got := l.Departure(dep("T1", 5, "S", "08:00"))
	if got.Status != StatusUnknown {
		t.Fatalf("status = %v, want unknown", got.Status)
	}
	if !got.Estimated.Equal(at("08:00")) {
		t.Errorf("estimated = %v, want the scheduled time as fallback", got.Estimated)
	}
}

// A nil snapshot is what a caller has when no realtime key is configured. It
// must degrade to schedule-only rather than panic.
func TestNilLiveIsScheduleOnly(t *testing.T) {
	var l *Live
	got := l.Departure(dep("T1", 5, "S", "08:00"))
	if got.Status != StatusUnknown || !got.Estimated.Equal(at("08:00")) {
		t.Fatalf("got %v at %v, want unknown at scheduled time", got.Status, got.Estimated)
	}
	if a := l.Alerts(at("08:00")); a != nil {
		t.Errorf("alerts = %v, want none", a)
	}
}

// A delay of exactly zero is a real prediction, and must read as on time rather
// than falling through to unknown.
func TestZeroDelayIsOnTimeNotUnknown(t *testing.T) {
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "T1", Stops: []realtime.StopUpdate{{Sequence: 5, Departure: secs(0)}}},
	}})
	got := l.Departure(dep("T1", 5, "S", "08:00"))
	if got.Status != StatusOnTime {
		t.Fatalf("status = %v, want on time", got.Status)
	}
}

func TestDelayClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay int
		want  LiveStatus
	}{
		{"four minutes late", 240, StatusLate},
		{"a minute late is late", 60, StatusLate},
		{"under a minute is on time", 59, StatusOnTime},
		{"slightly early is on time", -30, StatusOnTime},
		{"two minutes early", -120, StatusEarly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
				{TripID: "T1", Stops: []realtime.StopUpdate{{Sequence: 5, Departure: secs(tc.delay)}}},
			}})
			got := l.Departure(dep("T1", 5, "S", "08:00"))
			if got.Status != tc.want {
				t.Errorf("status = %v, want %v", got.Status, tc.want)
			}
			want := at("08:00").Add(time.Duration(tc.delay) * time.Second)
			if !got.Estimated.Equal(want) {
				t.Errorf("estimated = %v, want %v", got.Estimated, want)
			}
		})
	}
}

// The metro feed regularly sets an arrival delay and leaves departure unset.
// Without the fallback those calls would report as unknown.
func TestFallsBackToArrivalWhenDepartureUnset(t *testing.T) {
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "T1", Stops: []realtime.StopUpdate{{Sequence: 5, Arrival: secs(180)}}},
	}})
	got := l.Departure(dep("T1", 5, "S", "08:00"))
	if got.Status != StatusLate || got.Delay != 3*time.Minute {
		t.Fatalf("got %v %v, want late by 3m", got.Status, got.Delay)
	}
}

// Most metro trips publish non-contiguous stop_sequence. The spec says a stop
// with no update of its own inherits the previous one's delay.
func TestDelayPropagatesForwardToUncoveredStop(t *testing.T) {
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{{
		TripID: "T1",
		Stops: []realtime.StopUpdate{
			{Sequence: 3, Departure: secs(60)},
			{Sequence: 7, Departure: secs(300)},
		},
	}}})

	// Sequence 9 is past every update, so it inherits sequence 7's five minutes.
	got := l.Departure(dep("T1", 9, "S", "08:00"))
	if got.Status != StatusLate || got.Delay != 5*time.Minute {
		t.Fatalf("got %v %v, want late by 5m inherited from seq 7", got.Status, got.Delay)
	}

	// Sequence 5 sits between the two and inherits the earlier one, not the later.
	got = l.Departure(dep("T1", 5, "S", "08:00"))
	if got.Delay != time.Minute {
		t.Errorf("delay = %v, want 1m inherited from seq 3", got.Delay)
	}
}

// A stop before every published update has nothing to inherit.
func TestNoPropagationBackwards(t *testing.T) {
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "T1", Stops: []realtime.StopUpdate{{Sequence: 20, Departure: secs(600)}}},
	}})
	got := l.Departure(dep("T1", 2, "S", "08:00"))
	if got.Status != StatusUnknown {
		t.Fatalf("status = %v, want unknown; a later stop's delay says nothing about an earlier one", got.Status)
	}
}

// An inherited prediction must not carry the earlier stop's skip flag: that
// stop being skipped says nothing about this one.
func TestSkipDoesNotPropagate(t *testing.T) {
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "T1", Stops: []realtime.StopUpdate{{Sequence: 3, Skipped: true, Departure: secs(120)}}},
	}})
	if got := l.Departure(dep("T1", 3, "S", "08:00")); got.Status != StatusSkipped {
		t.Fatalf("own stop: status = %v, want skipped", got.Status)
	}
	if got := l.Departure(dep("T1", 8, "S", "08:00")); got.Status == StatusSkipped {
		t.Fatal("later stop inherited the skip flag")
	}
}

func TestCanceledTripBeatsStopPredictions(t *testing.T) {
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "T1", Canceled: true, Stops: []realtime.StopUpdate{{Sequence: 5, Departure: secs(0)}}},
	}})
	if got := l.Departure(dep("T1", 5, "S", "08:00")); got.Status != StatusCanceled {
		t.Fatalf("status = %v, want cancelled", got.Status)
	}
}

// An absolute predicted time is authoritative where the publisher gives one.
func TestAbsoluteTimeWinsOverDelay(t *testing.T) {
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{{
		TripID: "T1",
		Stops: []realtime.StopUpdate{{
			Sequence:  5,
			Departure: realtime.StopEvent{Set: true, Delay: 30 * time.Second, Time: at("08:07")},
		}},
	}}})
	got := l.Departure(dep("T1", 5, "S", "08:00"))
	if !got.Estimated.Equal(at("08:07")) {
		t.Fatalf("estimated = %v, want 08:07", got.Estimated)
	}
	if got.Delay != 7*time.Minute || got.Status != StatusLate {
		t.Errorf("got %v %v, want late by 7m", got.Status, got.Delay)
	}
}

// Where the publisher omits stop_sequence entirely, stop id is the only key.
func TestMatchesByStopIDWhenSequenceOmitted(t *testing.T) {
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "T1", Stops: []realtime.StopUpdate{{StopID: "19842", Departure: secs(120)}}},
	}})
	got := l.Departure(dep("T1", 14, "19842", "08:00"))
	if got.Status != StatusLate || got.Delay != 2*time.Minute {
		t.Fatalf("got %v %v, want late by 2m matched on stop id", got.Status, got.Delay)
	}
}

func TestAlertActivePeriod(t *testing.T) {
	a := realtime.Alert{Start: at("09:00"), End: at("17:00")}
	for _, tc := range []struct {
		when string
		want bool
	}{{"08:00", false}, {"09:00", true}, {"12:00", true}, {"17:00", true}, {"18:00", false}} {
		if got := a.ActiveAt(at(tc.when)); got != tc.want {
			t.Errorf("ActiveAt(%s) = %v, want %v", tc.when, got, tc.want)
		}
	}
	// The tram feed publishes most notices with no period at all.
	if !(realtime.Alert{}).ActiveAt(at("03:00")) {
		t.Error("an alert with no stated period should always be active")
	}
}

func TestAlertsForMatchesRouteOrStop(t *testing.T) {
	l := NewLive(&realtime.Data{Alerts: []realtime.Alert{
		{Header: "belgrave", Routes: []string{"aus:vic:vic-02-BEG:"}},
		{Header: "mernda station", Stops: []string{"vic:rail:MDD"}},
		{Header: "unrelated", Routes: []string{"aus:vic:vic-02-SUY:"}},
		{Header: "expired", Routes: []string{"aus:vic:vic-02-BEG:"}, End: at("07:00")},
	}})
	got := l.AlertsFor(at("08:00"), []string{"aus:vic:vic-02-BEG:"}, []string{"vic:rail:MDD"})
	if len(got) != 2 {
		t.Fatalf("got %d alerts, want 2: %+v", len(got), got)
	}
	if got[0].Header != "belgrave" || got[1].Header != "mernda station" {
		t.Errorf("wrong alerts matched: %v, %v", got[0].Header, got[1].Header)
	}
}

func journey(legs ...Leg) Journey {
	j := Journey{Legs: legs, Depart: legs[0].Depart, Arrive: legs[len(legs)-1].Arrive}
	j.Transfers = len(legs) - 1
	return j
}

// The point of the journey join: a connection that realtime has eaten cannot be
// seen from either leg alone.
func TestBrokenTransferDetected(t *testing.T) {
	j := journey(
		Leg{TripID: "A", FromSeq: 1, ToSeq: 9, Depart: at("08:00"), Arrive: at("08:20")},
		Leg{TripID: "B", FromSeq: 4, ToSeq: 12, Depart: at("08:25"), Arrive: at("08:50")},
	)
	// The first train loses eight minutes; the connection was five.
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "A", Stops: []realtime.StopUpdate{{Sequence: 9, Departure: secs(480)}}},
		{TripID: "B", Stops: []realtime.StopUpdate{{Sequence: 4, Departure: secs(0)}}},
	}})

	got := l.Journey(j)
	if !got.BrokenTransfer {
		t.Fatal("BrokenTransfer = false, want true: arrival 08:28 against departure 08:25")
	}
	if got.Status != StatusLate {
		t.Errorf("status = %v, want late", got.Status)
	}
	if len(got.TightTransfers) != 1 || got.TightTransfers[0] != -3*time.Minute {
		t.Errorf("TightTransfers = %v, want [-3m]", got.TightTransfers)
	}
}

// Two unknowns leave the scheduled connection intact, which is workable by
// construction and must not be reported as broken.
func TestUnknownDoesNotBreakTransfer(t *testing.T) {
	j := journey(
		Leg{TripID: "A", FromSeq: 1, ToSeq: 9, Depart: at("08:00"), Arrive: at("08:20")},
		Leg{TripID: "B", FromSeq: 4, ToSeq: 12, Depart: at("08:25"), Arrive: at("08:50")},
	)
	got := NewLive().Journey(j)
	if got.BrokenTransfer {
		t.Fatal("BrokenTransfer = true with no realtime data at all")
	}
	if got.Status != StatusUnknown {
		t.Errorf("status = %v, want unknown", got.Status)
	}
	if !got.EstimatedArrive.Equal(at("08:50")) {
		t.Errorf("estimated arrival = %v, want the scheduled 08:50", got.EstimatedArrive)
	}
}

// A journey is only as good as its worst leg.
func TestJourneyTakesWorstLegStatus(t *testing.T) {
	j := journey(
		Leg{TripID: "A", FromSeq: 1, ToSeq: 9, Depart: at("08:00"), Arrive: at("08:20")},
		Leg{TripID: "B", FromSeq: 4, ToSeq: 12, Depart: at("08:30"), Arrive: at("08:50")},
	)
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "A", Stops: []realtime.StopUpdate{{Sequence: 1, Departure: secs(0)}}},
		{TripID: "B", Canceled: true},
	}})
	got := l.Journey(j)
	if got.Status != StatusCanceled {
		t.Fatalf("status = %v, want cancelled: one leg is cancelled", got.Status)
	}
}

// Delay is measured at the destination, not at the origin.
func TestJourneyDelayIsAtArrival(t *testing.T) {
	j := journey(Leg{TripID: "A", FromSeq: 1, ToSeq: 9, Depart: at("08:00"), Arrive: at("08:20")})
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{{
		TripID: "A",
		Stops: []realtime.StopUpdate{
			{Sequence: 1, Departure: secs(0)},   // left on time
			{Sequence: 9, Departure: secs(360)}, // lost six minutes on the way
		},
	}}})
	got := l.Journey(j)
	if got.Delay != 6*time.Minute {
		t.Fatalf("delay = %v, want 6m measured at arrival", got.Delay)
	}
	if !got.EstimatedDepart.Equal(at("08:00")) {
		t.Errorf("estimated departure = %v, want the on-time 08:00", got.EstimatedDepart)
	}
	if !got.EstimatedArrive.Equal(at("08:26")) {
		t.Errorf("estimated arrival = %v, want 08:26", got.EstimatedArrive)
	}
}

func TestRealtimeFeedsForMode(t *testing.T) {
	if got := FeedsFor(ModeRegionalTrain); got != nil {
		t.Errorf("regional train feeds = %v, want none: no realtime is published", got)
	}
	if got := FeedsFor(ModeMetroTrain); len(got) != 2 {
		t.Errorf("metro train feeds = %d, want 2", len(got))
	}
}

// Deduplication keeps one row per physical service, but realtime publishes
// under whichever variant it likes. Without trying the alternates, the join
// reports no live data for a service the feed is actively describing.
func TestMatchesRealtimePublishedUnderAnAlternateTripID(t *testing.T) {
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "02-BEG--62-T2-3026", Stops: []realtime.StopUpdate{{Sequence: 22, Departure: secs(240)}}},
	}})

	d := dep("02-BEG--1-T2-3026", 22, "12241", "12:35")
	d.Alts = []AltTrip{
		{TripID: "02-BEG--62-T2-3026", Seq: 22},
		{TripID: "02-BEG--63-T2-3026", Seq: 16},
	}

	got := l.Departure(d)
	if got.Status != StatusLate || got.Delay != 4*time.Minute {
		t.Fatalf("got %v %v, want late by 4m via the alternate id", got.Status, got.Delay)
	}
}

// The alternates number their stops differently, so the alternate's own
// sequence must be used rather than the survivor's.
func TestAlternateUsesItsOwnStopSequence(t *testing.T) {
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "variant", Stops: []realtime.StopUpdate{
			{Sequence: 16, Departure: secs(300)},
			{Sequence: 22, Departure: secs(0)},
		}},
	}})
	d := dep("primary", 22, "12241", "12:35")
	d.Alts = []AltTrip{{TripID: "variant", Seq: 16}}

	if got := l.Departure(d); got.Delay != 5*time.Minute {
		t.Fatalf("delay = %v, want 5m read at the alternate's sequence 16", got.Delay)
	}
}

// One run published as both a Bayswater and a Belgrave service: realtime covers
// the one that is really running.
func TestDisambiguatePrefersTheCoveredVariant(t *testing.T) {
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "belgrave", Stops: []realtime.StopUpdate{{Sequence: 9, Departure: secs(60)}}},
	}})
	in := l.Departures([]Departure{
		{TripID: "bayswater", Seq: 9, StopID: "12242", RouteID: "BEG",
			Depart: at("12:52"), Headsign: "Bayswater via City Loop"},
		{TripID: "belgrave", Seq: 9, StopID: "12242", RouteID: "BEG",
			Depart: at("12:52"), Headsign: "Belgrave via City Loop"},
	})

	got := l.Disambiguate(in)
	if len(got) != 1 {
		t.Fatalf("got %d departures, want 1", len(got))
	}
	if got[0].Headsign != "Belgrave via City Loop" {
		t.Errorf("kept %q, want the variant realtime is tracking", got[0].Headsign)
	}
}

// With nothing covered there is no evidence, so nothing is dropped: a guess
// here would just be a guess with fewer rows.
func TestDisambiguateKeepsBothWhenNeitherIsCovered(t *testing.T) {
	l := NewLive()
	in := l.Departures([]Departure{
		{TripID: "a", StopID: "12242", RouteID: "BEG", Depart: at("12:52"), Headsign: "Bayswater"},
		{TripID: "b", StopID: "12242", RouteID: "BEG", Depart: at("12:52"), Headsign: "Belgrave"},
	})
	if got := l.Disambiguate(in); len(got) != 2 {
		t.Fatalf("got %d, want both kept", len(got))
	}
}

// Two genuinely different services must never be collapsed by this.
func TestDisambiguateLeavesDistinctDeparturesAlone(t *testing.T) {
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "a", Stops: []realtime.StopUpdate{{Sequence: 1, Departure: secs(60)}}},
	}})
	in := l.Departures([]Departure{
		{TripID: "a", Seq: 1, StopID: "12242", RouteID: "BEG", Depart: at("12:52")},
		{TripID: "b", Seq: 1, StopID: "12242", RouteID: "BEG", Depart: at("13:12")},
	})
	if got := l.Disambiguate(in); len(got) != 2 {
		t.Fatalf("got %d, want 2: different times are different services", len(got))
	}
}

// One incident across three lines arrives as three identical alerts.
func TestAlertsDeduplicateAcrossAffectedRoutes(t *testing.T) {
	const desc = "Delays up to 15 minutes after an earlier police request in the Caulfield area."
	l := NewLive(&realtime.Data{Alerts: []realtime.Alert{
		{Header: "Minor Delay", Description: desc, Routes: []string{"CBE"}, Stops: []string{"vic:rail:CFD"}},
		{Header: "Minor Delay", Description: desc, Routes: []string{"FKN"}},
		{Header: "Minor Delay", Description: desc, Routes: []string{"PKM"}, Stops: []string{"vic:rail:CFD"}},
		{Header: "Minor Delay", Description: "A different incident entirely.", Routes: []string{"BEG"}},
	}})

	got := l.Alerts(at("14:40"))
	if len(got) != 2 {
		t.Fatalf("got %d alerts, want 2", len(got))
	}
	if len(got[0].Routes) != 3 {
		t.Errorf("routes = %v, want all three pooled onto one alert", got[0].Routes)
	}
	if len(got[0].Stops) != 1 {
		t.Errorf("stops = %v, want the duplicate stop merged once", got[0].Stops)
	}
}

// A deduplicated journey must still find realtime published under one of the
// variants it absorbed, at each end of the leg independently.
func TestJourneyMatchesRealtimeViaLegAlternates(t *testing.T) {
	j := journey(Leg{
		TripID: "primary", FromSeq: 1, ToSeq: 9,
		Depart: at("08:00"), Arrive: at("08:20"),
		Alts: []LegAlt{{TripID: "variant", FromSeq: 5, ToSeq: 13}},
	})
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{{
		TripID: "variant",
		Stops: []realtime.StopUpdate{
			{Sequence: 5, Departure: secs(0)},
			{Sequence: 13, Departure: secs(240)},
		},
	}}})

	got := l.Journey(j)
	if got.Status != StatusLate {
		t.Fatalf("status = %v, want late via the alternate", got.Status)
	}
	if got.Delay != 4*time.Minute {
		t.Errorf("delay = %v, want 4m read at the alternate's arrival sequence", got.Delay)
	}
}

// A walk sits inside a transfer rather than being one. Measuring the gap either
// side of it reports zero for a connection that is workable, and hides the walk
// from the time the passenger actually has.
func TestJourneyTransferSlackAccountsForWalking(t *testing.T) {
	j := Journey{
		Legs: []Leg{
			{TripID: "bus", FromSeq: 1, ToSeq: 9, Depart: at("19:59"), Arrive: at("20:19")},
			{Walk: true, WalkMetres: 89, Depart: at("20:19"), Arrive: at("20:21")},
			{TripID: "train", FromSeq: 3, ToSeq: 9, Depart: at("20:22"), Arrive: at("20:39")},
		},
		Depart: at("19:59"), Arrive: at("20:39"), Transfers: 1,
	}
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "bus", Stops: []realtime.StopUpdate{{Sequence: 9, Departure: secs(0)}}},
		{TripID: "train", Stops: []realtime.StopUpdate{{Sequence: 3, Departure: secs(0)}}},
	}})

	got := l.Journey(j)
	if n := len(got.TightTransfers); n != 1 {
		t.Fatalf("got %d transfers, want 1: the walk is part of the change, not a change of its own", n)
	}
	// Three minutes between services, two of them spent walking.
	if got.TightTransfers[0] != time.Minute {
		t.Errorf("slack = %v, want 1m (3m gap less a 2m walk)", got.TightTransfers[0])
	}
	if got.BrokenTransfer {
		t.Error("BrokenTransfer set for a connection with a minute to spare")
	}
}

// A walk long enough to eat the connection breaks it.
func TestJourneyWalkCanBreakTheConnection(t *testing.T) {
	j := Journey{
		Legs: []Leg{
			{TripID: "bus", FromSeq: 1, ToSeq: 9, Depart: at("19:59"), Arrive: at("20:19")},
			{Walk: true, WalkMetres: 400, Depart: at("20:19"), Arrive: at("20:27")},
			{TripID: "train", FromSeq: 3, ToSeq: 9, Depart: at("20:22"), Arrive: at("20:39")},
		},
		Depart: at("19:59"), Arrive: at("20:39"), Transfers: 1,
	}
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "bus", Stops: []realtime.StopUpdate{{Sequence: 9, Departure: secs(0)}}},
		{TripID: "train", Stops: []realtime.StopUpdate{{Sequence: 3, Departure: secs(0)}}},
	}})
	if got := l.Journey(j); !got.BrokenTransfer {
		t.Fatalf("BrokenTransfer = false; the walk takes 8 minutes and the train leaves in 3")
	}
}

// A late first leg pushes the walk later, and the slack comes out of the change.
func TestJourneyWalkShiftsWithDelay(t *testing.T) {
	j := Journey{
		Legs: []Leg{
			{TripID: "bus", FromSeq: 1, ToSeq: 9, Depart: at("19:59"), Arrive: at("20:19")},
			{Walk: true, WalkMetres: 89, Depart: at("20:19"), Arrive: at("20:21")},
			{TripID: "train", FromSeq: 3, ToSeq: 9, Depart: at("20:25"), Arrive: at("20:39")},
		},
		Depart: at("19:59"), Arrive: at("20:39"), Transfers: 1,
	}
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "bus", Stops: []realtime.StopUpdate{{Sequence: 9, Departure: secs(240)}}}, // four late
		{TripID: "train", Stops: []realtime.StopUpdate{{Sequence: 3, Departure: secs(0)}}},
	}})
	got := l.Journey(j)
	if len(got.LiveLegs) != 3 || !got.LiveLegs[1].Walk {
		t.Fatalf("expected the walk to survive as a leg: %+v", got.LiveLegs)
	}
	if !got.LiveLegs[1].EstimatedDepart.Equal(at("20:23")) {
		t.Errorf("walk starts at %v, want 20:23 after the bus loses four minutes",
			got.LiveLegs[1].EstimatedDepart.Format("15:04"))
	}
	if got.TightTransfers[0] != 0 {
		t.Errorf("slack = %v, want 0: six minutes of gap, four lost to delay and two to walking",
			got.TightTransfers[0])
	}
}

// Trip ids repeat daily, so a snapshot taken today matches tomorrow's timetable
// by id alone. Applying it produces predictions a day out, which is exactly the
// kind of confident nonsense the tri-state exists to prevent.
func TestDepartureIgnoresRealtimeFromAnotherServiceDay(t *testing.T) {
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "T1", StartDate: "20260815", Stops: []realtime.StopUpdate{{Sequence: 5, Departure: secs(120)}}},
	}})

	// The fixture's clock is 15 August, so this matches.
	today := dep("T1", 5, "S", "08:00")
	today.DepartSec = 8 * 3600
	if got := l.Departure(today); got.Status != StatusLate {
		t.Fatalf("same day: status = %v, want late", got.Status)
	}

	// The next day's timetable shares the trip id and must not be touched.
	tomorrow := dep("T1", 5, "S", "08:00")
	tomorrow.Depart = tomorrow.Depart.AddDate(0, 0, 1)
	tomorrow.DepartSec = 8 * 3600
	if got := l.Departure(tomorrow); got.Status != StatusUnknown {
		t.Fatalf("next day: status = %v, want unknown", got.Status)
	}
}

// An after-midnight service belongs to the previous day's timetable, and its
// realtime update is stamped with that day.
func TestServiceDayHandlesAfterMidnight(t *testing.T) {
	// 25:30 on the 15th is 01:30 on the 16th.
	d := Departure{TripID: "T1", Seq: 3, DepartSec: 25*3600 + 30*60,
		Depart: time.Date(2026, 8, 16, 1, 30, 0, 0, testLoc)}
	if !sameServiceDay("20260815", d) {
		t.Error("a 25:30 departure should belong to the 15th")
	}
	if sameServiceDay("20260816", d) {
		t.Error("it must not match the 16th, which is only the wall-clock date")
	}
}

// A platform change is a walk with no leg to represent it, so a delay can eat it
// while the arithmetic still reports slack. This is the Richmond case: arrive on
// platform 8 at 13:32, leave from platform 3 at 13:38, six minutes for a
// crossing the pathway graph puts at three. Five minutes late and there is one
// minute left for a three-minute walk — gone, though nothing has yet departed
// before the passenger lands.
func TestDelayEatsAPlatformChange(t *testing.T) {
	j := Journey{
		Legs: []Leg{
			{TripID: "lilydale", FromSeq: 17, ToSeq: 21, Depart: at("13:23"), Arrive: at("13:32")},
			{TripID: "frankston", FromSeq: 27, ToSeq: 28, Depart: at("13:38"), Arrive: at("13:41")},
		},
		Depart: at("13:23"), Arrive: at("13:41"), Transfers: 1,
		WaitAtTransfer: []time.Duration{6 * time.Minute},
		TransferNeeded: []time.Duration{3 * time.Minute},
	}
	late := func(mins int) *Live {
		return NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
			{TripID: "lilydale", Stops: []realtime.StopUpdate{{Sequence: 21, Departure: secs(mins * 60)}}},
			{TripID: "frankston", Stops: []realtime.StopUpdate{{Sequence: 27, Departure: secs(0)}}},
		}})
	}

	// On time: six minutes, three of them crossing. Three to spare.
	if got := late(0).Journey(j); got.TightTransfers[0] != 3*time.Minute || got.BrokenTransfer {
		t.Errorf("on time: slack = %v, broken = %v; want 3m and false",
			got.TightTransfers[0], got.BrokenTransfer)
	}
	// Three late: the crossing exactly consumes the gap. Still makeable.
	if got := late(3).Journey(j); got.TightTransfers[0] != 0 || got.BrokenTransfer {
		t.Errorf("3m late: slack = %v, broken = %v; want 0 and false",
			got.TightTransfers[0], got.BrokenTransfer)
	}
	// Five late: one minute of clock against a three-minute walk. Missed.
	got := late(5).Journey(j)
	if got.TightTransfers[0] != -2*time.Minute {
		t.Errorf("5m late: slack = %v, want -2m (1m gap less a 3m crossing)", got.TightTransfers[0])
	}
	if !got.BrokenTransfer {
		t.Fatal("5m late: BrokenTransfer = false, but one minute does not cover a three-minute crossing")
	}
}

// A journey planned before TransferNeeded existed, or one built by hand, still
// behaves as it did rather than treating the missing value as free.
func TestMissingTransferNeededIsNotCharged(t *testing.T) {
	j := Journey{
		Legs: []Leg{
			{TripID: "a", FromSeq: 1, ToSeq: 2, Depart: at("09:00"), Arrive: at("09:10")},
			{TripID: "b", FromSeq: 5, ToSeq: 6, Depart: at("09:14"), Arrive: at("09:30")},
		},
		Depart: at("09:00"), Arrive: at("09:30"), Transfers: 1,
	}
	l := NewLive(&realtime.Data{TripUpdates: []realtime.TripUpdate{
		{TripID: "a", Stops: []realtime.StopUpdate{{Sequence: 2, Departure: secs(0)}}},
		{TripID: "b", Stops: []realtime.StopUpdate{{Sequence: 5, Departure: secs(0)}}},
	}})
	if got := l.Journey(j); got.TightTransfers[0] != 4*time.Minute || got.BrokenTransfer {
		t.Errorf("slack = %v, broken = %v; want the raw 4m gap and false",
			got.TightTransfers[0], got.BrokenTransfer)
	}
}

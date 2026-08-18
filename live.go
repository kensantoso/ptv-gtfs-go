// Live data joins the realtime feeds onto the static schedule.
//
// The rule that governs this file: absence of realtime data is never reported
// as on time. The feeds only cover services that are running or about to,
// roughly an hour ahead, so most of the schedule has no live data at any
// moment. A tool that collapses "no prediction" into "on time" invents
// reassurance about a train nobody is watching yet.

package gtfs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/kensantoso/ptv-gtfs-go/realtime"
)

// LiveStatus is what realtime says about one service call.
type LiveStatus int

const (
	// StatusUnknown means no realtime data covers this service. It is the zero
	// value deliberately: anything that fails to make a determination reports
	// not-known rather than on-time.
	StatusUnknown LiveStatus = iota
	StatusOnTime
	StatusEarly
	StatusLate
	StatusCanceled
	// StatusSkipped means the service runs but will not call at this stop.
	StatusSkipped
)

// String returns a stable machine name, not a phrase for a person.
//
// Wording belongs to whatever is doing the talking: an assistant, a web page
// and a departure board all say this differently, and one of them is in another
// language. A library that hands out "not stopping" has made that decision for
// all of them.
func (s LiveStatus) String() string {
	switch s {
	case StatusOnTime:
		return "on_time"
	case StatusEarly:
		return "early"
	case StatusLate:
		return "late"
	case StatusCanceled:
		return "cancelled"
	case StatusSkipped:
		return "skipped"
	}
	return "unknown"
}

// Known reports whether realtime had anything to say.
func (s LiveStatus) Known() bool { return s != StatusUnknown }

// Live is a snapshot of the realtime feeds, joined against the schedule.
//
// It is immutable once built. The feeds are published as full datasets, so a
// refresh means building a new snapshot rather than merging into this one.
type Live struct {
	// At is the oldest publisher timestamp across the feeds in this snapshot,
	// so staleness is judged by the least fresh part of it.
	At      time.Time
	policy  Policy
	byTrip  map[string]*realtime.TripUpdate
	alerts  []realtime.Alert
	modes   map[Mode]bool
	vehicle map[string]realtime.VehiclePosition
}

// NewLive builds a snapshot from already-decoded feed data. Use it to join
// against data fetched elsewhere, or to test without a network.
func NewLive(data ...*realtime.Data) *Live {
	return NewLiveWithPolicy(DefaultPolicy(), data...)
}

// NewLiveWithPolicy builds a snapshot under a caller's own judgements, chiefly
// how far from schedule counts as on time.
func NewLiveWithPolicy(p Policy, data ...*realtime.Data) *Live {
	l := &Live{
		policy:  p.withDefaults(),
		byTrip:  make(map[string]*realtime.TripUpdate),
		modes:   make(map[Mode]bool),
		vehicle: make(map[string]realtime.VehiclePosition),
	}
	for _, d := range data {
		if d == nil {
			continue
		}
		if l.At.IsZero() || (!d.At.IsZero() && d.At.Before(l.At)) {
			l.At = d.At
		}
		for i := range d.TripUpdates {
			tu := &d.TripUpdates[i]
			l.byTrip[tu.TripID] = tu
		}
		l.alerts = append(l.alerts, d.Alerts...)
		for _, v := range d.Vehicles {
			l.vehicle[v.TripID] = v
		}
	}
	return l
}

// Covers reports whether the snapshot includes any feed for a mode. A mode with
// no feed cannot be reported on at all, which is different from a mode whose
// feed says nothing about a particular trip.
func (l *Live) Covers(m Mode) bool { return l != nil && l.modes[m] }

// Age is how stale the snapshot is relative to t.
func (l *Live) Age(t time.Time) time.Duration {
	if l == nil || l.At.IsZero() {
		return 0
	}
	return t.Sub(l.At)
}

// Alerts returns every alert in the snapshot that is active at t, deduplicated.
//
// The feeds publish one entity per affected route, so a single incident across
// three lines arrives as three identical alerts. Merging them on their text and
// pooling the routes they name turns that back into one disruption affecting
// three lines, which is what it is.
func (l *Live) Alerts(at time.Time) []realtime.Alert {
	if l == nil {
		return nil
	}
	var out []realtime.Alert
	index := make(map[string]int)
	for _, a := range l.alerts {
		if !a.ActiveAt(at) {
			continue
		}
		// Text is the identity. Two incidents with the same description on the
		// same day are the same incident.
		k := a.Header + "\x00" + a.Description
		if i, ok := index[k]; ok {
			out[i].Routes = mergeIDs(out[i].Routes, a.Routes)
			out[i].Stops = mergeIDs(out[i].Stops, a.Stops)
			out[i].Trips = mergeIDs(out[i].Trips, a.Trips)
			continue
		}
		index[k] = len(out)
		out = append(out, a)
	}
	return out
}

func mergeIDs(into, from []string) []string {
	seen := make(map[string]bool, len(into))
	for _, s := range into {
		seen[s] = true
	}
	for _, s := range from {
		if !seen[s] {
			seen[s] = true
			into = append(into, s)
		}
	}
	return into
}

// AlertsFor returns active alerts naming any of the given routes or stops.
//
// Stop matching should be given the station id as well as the platform id: the
// trip feeds key on platforms while alerts name stations, so passing only the
// platform silently matches nothing.
func (l *Live) AlertsFor(at time.Time, routeIDs, stopIDs []string) []realtime.Alert {
	if l == nil {
		return nil
	}
	want := make(map[string]bool, len(routeIDs)+len(stopIDs))
	for _, r := range routeIDs {
		want[r] = true
	}
	for _, s := range stopIDs {
		want[s] = true
	}
	var out []realtime.Alert
	for _, a := range l.alerts {
		if !a.ActiveAt(at) {
			continue
		}
		// A network-wide alert names no routes or stops at all; it applies to
		// everything and is dropped here only because a caller asking about one
		// line does not want the whole network's notices. Alerts() returns them.
		if match(a.Routes, want) || match(a.Stops, want) {
			out = append(out, a)
		}
	}
	return out
}

func match(have []string, want map[string]bool) bool {
	for _, h := range have {
		if want[h] {
			return true
		}
	}
	return false
}

// Vehicle returns the last reported position for a trip.
func (l *Live) Vehicle(tripID string) (realtime.VehiclePosition, bool) {
	if l == nil {
		return realtime.VehiclePosition{}, false
	}
	v, ok := l.vehicle[tripID]
	return v, ok
}

// sameServiceDay reports whether a realtime update describes the service day a
// scheduled departure belongs to.
//
// Trip ids repeat from one day to the next, so a snapshot taken today will match
// tomorrow's timetable by id alone and apply today's predictions to it. The
// result is not a small error: it is a day out. The feed states the service date
// on every update, so it can simply be checked.
func sameServiceDay(startDate string, d Departure) bool {
	if startDate == "" || d.Depart.IsZero() {
		return true // nothing to check against
	}
	// A trip beginning at 25:30 belongs to the previous calendar day, so the
	// service day is the departure less its seconds-after-midnight.
	day := d.Depart.Add(-time.Duration(d.DepartSec) * time.Second)
	return day.Format("20060102") == startDate
}

// LiveDeparture is a scheduled departure with realtime applied.
type LiveDeparture struct {
	Departure
	Status LiveStatus
	// Delay is signed; positive is late. Meaningless unless Status is known.
	Delay time.Duration
	// Estimated is when the service is now expected to depart. It falls back to
	// the scheduled time when nothing is known, so it is always usable for
	// ordering, but callers showing it to a person should check Status first.
	Estimated time.Time
}

// Departure applies realtime to one scheduled departure.
func (l *Live) Departure(d Departure) LiveDeparture {
	out := LiveDeparture{Departure: d, Estimated: d.Depart}
	if l == nil {
		return out
	}

	// The schedule publishes one physical service under several trip ids while
	// realtime describes only one of them, so a match must be attempted against
	// every variant. Checking only the survivor of deduplication would leave
	// most departures reporting no live data while the feed in fact covers them.
	tu, ok := l.byTrip[d.TripID]
	seq := d.Seq
	if !ok {
		for _, alt := range d.Alts {
			if tu, ok = l.byTrip[alt.TripID]; ok {
				seq = alt.Seq
				break
			}
		}
	}
	if !ok || !sameServiceDay(tu.StartDate, d) {
		return out
	}
	if tu.Canceled {
		out.Status = StatusCanceled
		return out
	}

	su, ok := stopUpdateFor(tu, seq, d.StopID)
	if !ok {
		return out
	}
	if su.Skipped {
		out.Status = StatusSkipped
		return out
	}

	// Prefer the departure prediction. Falling back to arrival matters: the
	// metro feed frequently sets an arrival delay and leaves departure unset.
	ev := su.Departure
	if !ev.Set {
		ev = su.Arrival
	}
	if !ev.Set {
		return out
	}

	switch {
	case !ev.Time.IsZero():
		out.Estimated = ev.Time.In(d.Depart.Location())
		out.Delay = out.Estimated.Sub(d.Depart)
	default:
		out.Delay = ev.Delay
		out.Estimated = d.Depart.Add(ev.Delay)
	}

	threshold := l.policy.OnTimeThreshold
	if threshold == 0 {
		threshold = DefaultPolicy().OnTimeThreshold
	}
	switch {
	case out.Delay >= threshold:
		out.Status = StatusLate
	case out.Delay <= -threshold:
		out.Status = StatusEarly
	default:
		out.Status = StatusOnTime
	}
	return out
}

// Departures applies realtime to a list, preserving order.
func (l *Live) Departures(ds []Departure) []LiveDeparture {
	out := make([]LiveDeparture, len(ds))
	for i, d := range ds {
		out[i] = l.Departure(d)
	}
	return out
}

// stopUpdateFor finds the prediction covering a call.
//
// An exact stop_sequence match is preferred. Where there is none, the GTFS
// Realtime specification says a stop inherits the delay of the last update
// before it, and gaps are common: on the metro feed most trips publish
// non-contiguous sequences. Without this fallback the majority of calls would
// report as unknown while the feed does in fact describe them.
func stopUpdateFor(tu *realtime.TripUpdate, seq int, stopID string) (realtime.StopUpdate, bool) {
	var best realtime.StopUpdate
	found := false
	for _, s := range tu.Stops {
		if s.Sequence == seq {
			return s, true
		}
		// Sequence 0 means the publisher omitted it; fall back to stop id.
		if s.Sequence == 0 && stopID != "" && s.StopID == stopID {
			return s, true
		}
		if s.Sequence < seq && (!found || s.Sequence > best.Sequence) {
			best, found = s, true
		}
	}
	if !found {
		return realtime.StopUpdate{}, false
	}
	// An inherited prediction carries the earlier stop's delay forward, but not
	// its absolute times or its skip flag, which belong to that stop alone.
	return realtime.StopUpdate{
		StopID:   stopID,
		Sequence: seq,
		Arrival:  realtime.StopEvent{Set: best.Arrival.Set, Delay: best.Arrival.Delay},
		Departure: realtime.StopEvent{
			Set:   best.Departure.Set,
			Delay: best.Departure.Delay,
		},
	}, true
}

// LiveLeg is a journey leg with realtime applied.
type LiveLeg struct {
	Leg
	Status          LiveStatus
	Delay           time.Duration
	EstimatedDepart time.Time
	EstimatedArrive time.Time
}

// LiveJourney is a planned journey with realtime applied.
type LiveJourney struct {
	Journey
	LiveLegs []LiveLeg
	// Status is the worst across the legs, so a journey with one cancelled leg
	// reports as cancelled rather than averaging out to fine.
	Status LiveStatus
	// Delay is at the destination, which is the number a passenger cares about.
	Delay time.Duration
	// EstimatedDepart and EstimatedArrive are the journey's real endpoints.
	EstimatedDepart time.Time
	EstimatedArrive time.Time
	// BrokenTransfer is set when realtime has eaten a connection: the incoming
	// service now arrives at or after the outgoing one leaves. This is the
	// signal that a planned journey needs replanning, and it cannot be seen by
	// looking at either leg alone.
	BrokenTransfer bool
	// TightTransfers holds the revised connection time at each change.
	TightTransfers []time.Duration
}

// Journey applies realtime to a planned journey.
func (l *Live) Journey(j Journey) LiveJourney {
	out := LiveJourney{
		Journey:         j,
		EstimatedDepart: j.Depart,
		EstimatedArrive: j.Arrive,
	}
	if len(j.Legs) == 0 {
		return out
	}
	for _, leg := range j.Legs {
		ll := LiveLeg{Leg: leg, EstimatedDepart: leg.Depart, EstimatedArrive: leg.Arrive}

		// A walk has no service behind it, so realtime says nothing about it.
		// It does move, though: if the leg before it runs late, the walk starts
		// late too and the time is taken out of the connection that follows.
		if leg.Walk {
			if n := len(out.LiveLegs); n > 0 {
				prev := out.LiveLegs[n-1]
				ll.EstimatedDepart = prev.EstimatedArrive
				ll.EstimatedArrive = prev.EstimatedArrive.Add(leg.Arrive.Sub(leg.Depart))
			}
			out.LiveLegs = append(out.LiveLegs, ll)
			continue
		}

		// A leg is a departure from its first stop and an arrival at its last.
		// Both ends get their own prediction: a train can leave on time and
		// lose five minutes before it reaches you.
		fromAlts := make([]AltTrip, 0, len(leg.Alts))
		toAlts := make([]AltTrip, 0, len(leg.Alts))
		for _, a := range leg.Alts {
			fromAlts = append(fromAlts, AltTrip{TripID: a.TripID, Seq: a.FromSeq})
			toAlts = append(toAlts, AltTrip{TripID: a.TripID, Seq: a.ToSeq})
		}
		dep := l.Departure(Departure{
			TripID: leg.TripID, StopID: leg.FromStop, Seq: leg.FromSeq,
			Depart: leg.Depart, Alts: fromAlts,
		})
		arr := l.Departure(Departure{
			TripID: leg.TripID, StopID: leg.ToStop, Seq: leg.ToSeq,
			Depart: leg.Arrive, Alts: toAlts,
		})
		ll.Status = dep.Status
		if worse(arr.Status, ll.Status) {
			ll.Status = arr.Status
		}
		ll.EstimatedDepart = dep.Estimated
		ll.EstimatedArrive = arr.Estimated
		ll.Delay = arr.Delay
		if !arr.Status.Known() {
			ll.Delay = dep.Delay
		}
		out.LiveLegs = append(out.LiveLegs, ll)

		if worse(ll.Status, out.Status) {
			out.Status = ll.Status
		}
	}

	out.EstimatedDepart = out.LiveLegs[0].EstimatedDepart
	last := out.LiveLegs[len(out.LiveLegs)-1]
	out.EstimatedArrive = last.EstimatedArrive
	out.Delay = out.EstimatedArrive.Sub(j.Arrive)

	// Transfers are between services, not between every pair of legs. A walk
	// sits inside a transfer rather than being one: measuring the gap either
	// side of it reports zero for a connection that is actually workable, and
	// hides the walk from the time the passenger has.
	rides := make([]int, 0, len(out.LiveLegs))
	for i, ll := range out.LiveLegs {
		if !ll.Walk {
			rides = append(rides, i)
		}
	}
	for k := 1; k < len(rides); k++ {
		prev, next := rides[k-1], rides[k]
		var onFoot time.Duration
		for i := prev + 1; i < next; i++ {
			onFoot += out.LiveLegs[i].EstimatedArrive.Sub(out.LiveLegs[i].EstimatedDepart)
		}
		// The change itself costs time even when it is not a leg: stepping off
		// at platform 8 and boarding at platform 3 is a walk with nothing in
		// Legs to represent it. Planning already priced it, and dropping it here
		// is what let a delay eat a connection while it still reported as
		// workable — the gap goes to a minute, the crossing takes three, and
		// slack stays positive right up until the train leaves before you land.
		var crossing time.Duration
		if k-1 < len(j.TransferNeeded) {
			crossing = j.TransferNeeded[k-1]
		}
		// What is left after walking and crossing is the slack the passenger
		// actually has.
		slack := out.LiveLegs[next].EstimatedDepart.Sub(out.LiveLegs[prev].EstimatedArrive) - onFoot - crossing
		out.TightTransfers = append(out.TightTransfers, slack)
		if slack < 0 && out.LiveLegs[prev].Status.Known() && out.LiveLegs[next].Status.Known() {
			out.BrokenTransfer = true
		}
	}
	return out
}

// worse reports whether a is a more serious status than b, for rolling several
// calls up into one verdict.
func worse(a, b LiveStatus) bool { return severity(a) > severity(b) }

func severity(s LiveStatus) int {
	switch s {
	case StatusCanceled:
		return 4
	case StatusSkipped:
		return 3
	case StatusLate:
		return 2
	case StatusEarly, StatusOnTime:
		return 1
	}
	return 0 // unknown ranks lowest: a known verdict always wins
}

// FeedsFor returns the published feeds for a mode, or nil where the
// mode has none. Regional trains, coaches and Skybus are scheduled in the
// static feed but have no realtime publication.
func FeedsFor(m Mode) []realtime.Feed {
	switch m {
	case ModeMetroTrain:
		return []realtime.Feed{realtime.MetroTrainTripUpdates, realtime.MetroTrainServiceAlerts}
	case ModeMetroTram:
		return []realtime.Feed{realtime.TramTripUpdates, realtime.TramServiceAlerts}
	case ModeMetroBus:
		return []realtime.Feed{realtime.BusTripUpdates}
	}
	return nil
}

// FetchLive builds a snapshot for the given modes, defaulting to every mode
// that publishes realtime.
//
// Feeds are fetched concurrently. A partial failure returns both a usable
// snapshot and an error describing what is missing: one mode's outage should
// not blind a caller to the others, and a caller that wants all-or-nothing can
// still check the error.
func FetchLive(ctx context.Context, c *realtime.Client, modes ...Mode) (*Live, error) {
	if c == nil {
		return nil, errors.New("gtfs: FetchLive: nil client")
	}
	if len(modes) == 0 {
		modes = []Mode{ModeMetroTrain, ModeMetroTram, ModeMetroBus}
	}

	type job struct {
		mode Mode
		feed realtime.Feed
	}
	var jobs []job
	seen := map[string]bool{}
	for _, m := range modes {
		for _, f := range FeedsFor(m) {
			if seen[f.Path] {
				continue
			}
			seen[f.Path] = true
			jobs = append(jobs, job{m, f})
		}
	}
	if len(jobs) == 0 {
		return NewLive(), nil
	}

	var (
		mu   sync.Mutex
		data []*realtime.Data
		errs []error
		got  = map[Mode]bool{}
		wg   sync.WaitGroup
	)
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			body, err := c.Fetch(ctx, j.feed)
			if err == nil {
				var d *realtime.Data
				d, err = realtime.Decode(body)
				if err == nil {
					mu.Lock()
					data = append(data, d)
					got[j.mode] = true
					mu.Unlock()
					return
				}
			}
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}(j)
	}
	wg.Wait()

	l := NewLive(data...)
	l.modes = got
	return l, errors.Join(errs...)
}

// Disambiguate drops departures the schedule could not decide about and
// realtime can.
//
// Victoria publishes some runs under two destinations at once: the same train,
// same platform, same second, described both as terminating short and as
// running through. Deduplication deliberately keeps both rather than guess at a
// destination. Where realtime covers one of them and not the others, though,
// the guess is no longer needed, and the covered one is the service that is
// actually running.
//
// Rows are left alone unless exactly one candidate in a group has live data.
// Two covered variants, or none, means realtime is not resolving anything and
// dropping either would be a guess wearing better clothes.
func (l *Live) Disambiguate(in []LiveDeparture) []LiveDeparture {
	if l == nil || len(in) < 2 {
		return in
	}
	type key struct {
		stop  string
		at    int64
		route string
	}
	groups := make(map[key][]int, len(in))
	for i, d := range in {
		k := key{d.StopID, d.Depart.Unix(), d.RouteID}
		groups[k] = append(groups[k], i)
	}

	drop := make(map[int]bool)
	for _, idx := range groups {
		if len(idx) < 2 {
			continue
		}
		known := -1
		for _, i := range idx {
			if !in[i].Status.Known() {
				continue
			}
			if known >= 0 {
				known = -1 // more than one is covered; realtime settles nothing
				break
			}
			known = i
		}
		if known < 0 {
			continue
		}
		for _, i := range idx {
			if i != known {
				drop[i] = true
			}
		}
	}
	if len(drop) == 0 {
		return in
	}
	out := make([]LiveDeparture, 0, len(in)-len(drop))
	for i, d := range in {
		if !drop[i] {
			out = append(out, d)
		}
	}
	return out
}

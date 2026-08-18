package gtfs

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Leg is one ride on one service.
type Leg struct {
	TripID     string
	RouteName  string
	Headsign   string
	Mode       Mode
	FromStop   string
	FromName   string
	FromSeq    int
	ToStop     string
	ToName     string
	ToSeq      int
	Depart     time.Time
	Arrive     time.Time
	StopsCount int // intermediate calls, so an express reads as fewer
	// RouteShort is the route's own label: a number for trams and buses, a line
	// name for trains. It is what is written on the front of the vehicle, which
	// is what a passenger is looking for.
	RouteShort string
	// Replacement marks a bus running in place of the train it replaces.
	//
	// These are published on the train mode, so a leg can say "metro train" and
	// be a bus at the kerb. Victoria carries 3,313 such trips, marked by the
	// line's route id carrying an -R suffix. Telling someone to go to a platform
	// when the service leaves from the forecourt is the kind of wrong answer
	// that strands people.
	Replacement bool
	// FromPlatform and ToPlatform are the platform numbers, where the feed gives
	// them. A change between two platforms of one station is a walk across a
	// concourse or a footbridge, and saying which is the difference between a
	// connection a passenger can plan and one they have to hunt for.
	FromPlatform string
	ToPlatform   string
	// Walk marks a leg made on foot between two stops the feed does not connect.
	// TripID, RouteName and Headsign are empty for these.
	Walk bool
	// WalkMetres is straight-line distance, so the real walk is longer. See
	// [WalkDuration] for how it becomes a duration.
	WalkMetres float64
	// Alts are other trip ids publishing this same physical leg. See
	// [dedupeJourneys]; realtime describes only one of them.
	Alts []LegAlt
}

// LegAlt is a duplicate publication of a leg. Both sequence numbers travel with
// it because the variants number their stops differently and a leg is looked up
// at each end.
type LegAlt struct {
	TripID  string
	FromSeq int
	ToSeq   int
}

// Journey is a complete trip, one or more legs.
type Journey struct {
	Legs      []Leg
	Depart    time.Time
	Arrive    time.Time
	Transfers int
	// WaitAtTransfer is the connection time at each change, in order.
	WaitAtTransfer []time.Duration
	// TransferNeeded is how long each change itself takes: crossing to the
	// other platform, or the walk between two stops the feed does not connect.
	// Parallel to WaitAtTransfer.
	//
	// Carried on the journey because realtime has to re-test the connection
	// after a delay, and the alternative is a database lookup on a path that
	// otherwise touches none. Without it a late service silently eats a
	// connection that still reports as workable.
	TransferNeeded []time.Duration
}

// Duration is door-to-door time for the journey as planned.
func (j Journey) Duration() time.Duration { return j.Arrive.Sub(j.Depart) }

// Rank orders journeys.
type Rank int

const (
	// RankFastest sorts by arrival time. The usual default.
	RankFastest Rank = iota
	// RankFewestTransfers prefers fewer changes, then arrival.
	RankFewestTransfers
	// RankLeaveLatest prefers the latest departure that still arrives soonest,
	// which is what "when should I leave" actually means.
	RankLeaveLatest
	// RankShortest prefers the least time spent travelling, whenever it leaves.
	//
	// This is a different question from RankFastest and often a different
	// answer. Arriving soonest means boarding whatever is next; travelling least
	// may mean waiting half an hour for an express that overtakes it. Someone
	// who can leave when they like, or who would rather wait at home than stand
	// on a train, is asking for this one.
	RankShortest
)

// PlanRequest asks for journeys between two stations.
type PlanRequest struct {
	FromStopID string    // any stop at the origin station
	ToStopID   string    // any stop at the destination station
	After      time.Time // defaults to now
	Within     time.Duration
	Modes      []Mode // empty means every mode
	// MaxTransfers is 0 for direct only, 1 for one change. Values above 1 are
	// accepted by the API and currently planned as 1; the search is structured
	// so raising it is a change of loop bound rather than a rewrite.
	MaxTransfers int
	Rank         Rank
	Limit        int
	// TransferBuffer is added to the published minimum transfer time. Zero
	// trusts the feed's own figure.
	TransferBuffer time.Duration
	// MaxWait bounds how long the traveller will wait for a departure. Zero
	// means no bound.
	//
	// This is what separates "least time travelling" from a useless answer.
	// Unbounded, the shortest journey in a three-hour window may leave in ninety
	// minutes to save three, which nobody wants. Bounded, it answers the real
	// question: I am free for the next while, so which is the quickest run I can
	// actually catch, even if I have to wait for it.
	MaxWait time.Duration
	// MaxWalk is how far, in metres, a connecting walk may be. Zero uses
	// the policy radius; negative disables walking connections entirely.
	MaxWalk float64
	// Via restricts results to journeys that change at this station. Empty means
	// any interchange the search finds. Setting it excludes direct services,
	// because a request to change somewhere is a request for a journey with a
	// change in it.
	Via string
}

// Plan finds journeys from one station to another.
//
// Direct legs are found by looking for trips that call at both stations with
// the origin's stop_sequence lower than the destination's. That ordering is the
// whole of direction: a trip calling at both says nothing about which way it
// travels, and ignoring sequence silently returns services heading the other
// way.
func (ix *Index) Plan(ctx context.Context, req PlanRequest) ([]Journey, error) {
	if req.FromStopID == "" || req.ToStopID == "" {
		return nil, fmt.Errorf("gtfs: Plan: FromStopID and ToStopID are required")
	}
	after := req.After
	if after.IsZero() {
		after = time.Now()
	}
	after = after.In(ix.loc)
	if err := ix.checkRange(ctx, after); err != nil {
		return nil, err
	}
	within := req.Within
	if within <= 0 {
		within = 3 * time.Hour
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 6
	}

	from, err := ix.StopIDsForStation(ctx, req.FromStopID)
	if err != nil {
		return nil, err
	}
	to, err := ix.StopIDsForStation(ctx, req.ToStopID)
	if err != nil {
		return nil, err
	}

	var journeys []Journey
	if req.Via == "" {
		journeys, err = ix.directJourneys(ctx, from, to, after, within, req.Modes)
		if err != nil {
			return nil, err
		}
	}

	// Always explore transfers when they are allowed. Gating this on there being
	// too few direct journeys hides the case that matters most: a change that
	// beats every direct service. Out of the eastern suburbs in the afternoon
	// peak the direct train runs to Flinders Street only, so reaching Parliament
	// or Melbourne Central needs a change even though directs to the city exist.
	if req.MaxTransfers >= 1 {
		via, err := ix.journeysWithOneChange(ctx, from, to, after, within, req)
		if err != nil {
			return nil, err
		}
		journeys = append(journeys, via...)
	}

	// Deduplicate before ranking and limiting. Victoria publishes one physical
	// service under several trip ids, so without this a request for five options
	// returns the same train five times.
	journeys = dedupeJourneys(journeys)
	if req.MaxWait > 0 {
		cutoff := after.Add(req.MaxWait)
		kept := journeys[:0]
		for _, j := range journeys {
			if !j.Depart.After(cutoff) {
				kept = append(kept, j)
			}
		}
		journeys = kept
	}
	// Drop options that are worse than another on every axis, before ranking
	// spends the limit on them.
	journeys = filterDominated(journeys)

	rankJourneys(journeys, req.Rank)
	if len(journeys) > limit {
		journeys = journeys[:limit]
	}
	return journeys, nil
}

// dedupeJourneys collapses journeys that are the same travel, differing only in
// which published trip id the schedule happens to name.
//
// Two journeys calling at the same stops at the same times are the same
// journey: a passenger cannot tell them apart and does not care which internal
// identifier the operator filed it under. The survivor keeps the others as
// [LegAlt]s so realtime, which publishes under only one variant, still matches.
func dedupeJourneys(in []Journey) []Journey {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]int, len(in))
	out := make([]Journey, 0, len(in))
	for _, j := range in {
		// Two journeys are the same when they start and end at the same place at
		// the same times with the same number of rides. Where the two services
		// share several stops, changing at any of them gives an identical result,
		// and listing "change at Richmond", "at East Richmond" and "at Burnley"
		// describes one journey three times.
		//
		// Keyed on stops and times rather than trip ids, because Victoria
		// publishes one train under several ids and keying on those collapses
		// nothing.
		var b strings.Builder
		rides := 0
		for _, l := range j.Legs {
			if !l.Walk {
				rides++
			}
		}
		first, last := j.Legs[0], j.Legs[len(j.Legs)-1]
		fmt.Fprintf(&b, "%s@%d>%s@%d|rides=%d",
			first.FromStop, j.Depart.Unix(), last.ToStop, j.Arrive.Unix(), rides)
		k := b.String()
		if i, ok := seen[k]; ok {
			// Prefer the shorter walk between otherwise identical journeys.
			if w1, w2 := totalWalk(j), totalWalk(out[i]); w1 < w2 {
				alts := out[i].Legs
				out[i] = j
				_ = alts
			}
			for li := range out[i].Legs {
				if li < len(j.Legs) {
					out[i].Legs[li].Alts = append(out[i].Legs[li].Alts, LegAlt{
						TripID:  j.Legs[li].TripID,
						FromSeq: j.Legs[li].FromSeq,
						ToSeq:   j.Legs[li].ToSeq,
					})
				}
			}
			continue
		}
		seen[k] = len(out)
		out = append(out, j)
	}
	return out
}

// directJourneys finds journeys made of a single service.
func (ix *Index) directJourneys(ctx context.Context, from, to []string, after time.Time,
	within time.Duration, modes []Mode) ([]Journey, error) {

	var out []Journey
	for _, dayOffset := range []int{0, -1} {
		day := midnight(after, ix.loc).AddDate(0, 0, dayOffset)
		services, err := ix.ServiceIDsOn(ctx, day)
		if err != nil {
			return nil, err
		}
		if len(services) == 0 {
			continue
		}
		fromSec := int(after.Sub(day).Seconds())
		if fromSec < 0 {
			continue
		}
		toSec := fromSec + int(within.Seconds())

		q := `SELECT a.trip_id, a.stop_id, a.seq, a.dep, b.stop_id, b.seq, b.arr,
		             t.service_id, t.mode, COALESCE(t.headsign,''),
		             COALESCE(r.long_name, r.short_name, ''),
		             COALESCE(r.short_name, ''),
		             sa.name, sb.name,
		             COALESCE(sa.platform,''), COALESCE(sb.platform,''),
		             CASE WHEN r.route_id LIKE '%-R:' THEN 1 ELSE 0 END
		      FROM stop_time a
		      JOIN stop_time b ON b.trip_id = a.trip_id AND b.seq > a.seq
		      JOIN trip  t  ON t.trip_id  = a.trip_id
		      JOIN route r  ON r.route_id = t.route_id
		      JOIN stop  sa ON sa.stop_id = a.stop_id
		      JOIN stop  sb ON sb.stop_id = b.stop_id
		      WHERE a.stop_id IN (` + placeholders(len(from)) + `)
		        AND b.stop_id IN (` + placeholders(len(to)) + `)
		        AND a.dep BETWEEN ? AND ?`
		args := make([]any, 0, len(from)+len(to)+4+len(modes))
		for _, id := range from {
			args = append(args, id)
		}
		for _, id := range to {
			args = append(args, id)
		}
		args = append(args, fromSec, toSec)
		if len(modes) > 0 {
			q += " AND t.mode IN (" + placeholders(len(modes)) + ")"
			for _, m := range modes {
				args = append(args, int(m))
			}
		}
		q += " ORDER BY a.dep LIMIT 400"

		rows, err := ix.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("gtfs: direct: %w", err)
		}
		for rows.Next() {
			var l Leg
			var depSec, arrSec, mode, replacement int
			var serviceID string
			if err := rows.Scan(&l.TripID, &l.FromStop, &l.FromSeq, &depSec,
				&l.ToStop, &l.ToSeq, &arrSec, &serviceID, &mode,
				&l.Headsign, &l.RouteName, &l.RouteShort, &l.FromName, &l.ToName,
				&l.FromPlatform, &l.ToPlatform, &replacement); err != nil {
				rows.Close()
				return nil, err
			}
			if !services[serviceID] {
				continue
			}
			l.Mode = Mode(mode)
			l.Replacement = replacement == 1
			l.Depart = Instant(day, depSec, ix.loc)
			l.Arrive = Instant(day, arrSec, ix.loc)
			l.StopsCount = l.ToSeq - l.FromSeq - 1
			out = append(out, Journey{
				Legs: []Leg{l}, Depart: l.Depart, Arrive: l.Arrive, Transfers: 0,
			})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// interchange is a station considered as a place to change, widened to include
// anything a short walk away.
//
// The walk matters because the feed connects platforms within a station and
// nothing else. A bus stop across the road from a station is a different stop
// with no edge to it, so without this an interchange is only ever the station
// itself.
type interchange struct {
	// stopIDs are every platform of the station, plus the platforms of anything
	// within walking distance. Trains have times at platforms, never at the
	// station row, so a station id alone yields an interchange nothing departs
	// from.
	stopIDs []string
	// walkTo is the distance to each stop that is not part of the station
	// itself, so a walking leg can be built and costed if the connection uses
	// one.
	walkTo map[string]float64
}

// openInterchange widens a station into everything reachable from it on foot.
func (ix *Index) openInterchange(ctx context.Context, station string, walk float64) (interchange, error) {
	ids, err := ix.StopIDsForStation(ctx, station)
	if err != nil {
		return interchange{}, err
	}
	ic := interchange{stopIDs: ids, walkTo: map[string]float64{}}
	if walk <= 0 {
		return ic, nil
	}
	near, err := ix.walkableFrom(ctx, ids, walk)
	if err != nil {
		return interchange{}, err
	}
	for _, n := range near {
		platforms, err := ix.StopIDsForStation(ctx, n.ID)
		if err != nil {
			platforms = []string{n.ID}
		}
		for _, id := range platforms {
			if _, seen := ic.walkTo[id]; seen {
				continue
			}
			ic.walkTo[id] = n.Metres
			ic.stopIDs = append(ic.stopIDs, id)
		}
	}
	return ic, nil
}

// catchable returns the onward service arriving soonest that the passenger can
// actually reach from leg1, or nil if none can be made.
//
// "Can be made" is the whole point. A candidate is discarded when the platform
// change takes longer than the gap, which is why the walk is costed per
// platform pair rather than per station: at Flinders Street two platforms of one
// island are a minute apart and the ends of the station are four, and a single
// figure would either discard good connections or offer impossible ones.
func (ix *Index) catchable(ctx context.Context, leg1 Leg, onward []Journey,
	ic interchange, walk float64, buffer time.Duration) (*Journey, time.Duration, error) {

	var best *Journey
	var bestNeed time.Duration
	for i := range onward {
		cand := &onward[i]
		if cand.Legs[0].TripID == leg1.TripID {
			continue // the same service; that is a direct journey, not a change
		}
		if cand.Legs[0].Depart.Before(leg1.Arrive) {
			continue // gone before you arrive
		}
		need, err := ix.TransferTime(ctx, leg1.ToStop, cand.Legs[0].FromStop)
		if err != nil {
			return nil, 0, err
		}
		if m, ok := ix.walkDistance(ctx, ic.walkTo, leg1.ToStop, cand.Legs[0].FromStop); ok {
			// The walk set is seeded from every stop of the interchange, so a
			// measured pair can be further apart than the radius that found it.
			// Honour the radius the caller asked for.
			if m > walk {
				continue
			}
			if w := ix.policy.WalkDuration(m); w > need {
				need = w
			}
		}
		if cand.Legs[0].Depart.Before(leg1.Arrive.Add(need + buffer)) {
			continue // not enough time to cross to the platform
		}
		if best == nil || cand.Arrive.Before(best.Arrive) {
			best, bestNeed = cand, need
		}
	}
	return best, bestNeed, nil
}

// joinLegs assembles a two-service journey, inserting a walk where the change
// leaves the station.
func (ix *Index) joinLegs(ctx context.Context, leg1, leg2 Leg, ic interchange, need time.Duration) Journey {
	legs := []Leg{leg1}
	// A passenger told to change at a station they must first walk to should be
	// told about the walk, so it is a leg of its own rather than silent time.
	if m, ok := ix.walkDistance(ctx, ic.walkTo, leg1.ToStop, leg2.FromStop); ok {
		legs = append(legs, Leg{
			Walk: true, WalkMetres: m, Mode: leg1.Mode,
			FromStop: leg1.ToStop, FromName: leg1.ToName,
			ToStop: leg2.FromStop, ToName: leg2.FromName,
			Depart: leg1.Arrive, Arrive: leg1.Arrive.Add(ix.policy.WalkDuration(m)),
		})
	}
	legs = append(legs, leg2)

	// The wait is what is left after walking, not the raw gap between services.
	// Reporting three minutes for a change containing a two-minute walk
	// describes time the passenger does not have.
	var onFoot time.Duration
	for _, l := range legs {
		if l.Walk {
			onFoot += l.Arrive.Sub(l.Depart)
		}
	}
	// A walk that became its own leg is already accounted for in onFoot; only
	// what is left of the change belongs in TransferNeeded, or it is charged
	// twice.
	if need -= onFoot; need < 0 {
		need = 0
	}
	return Journey{
		Legs:           legs,
		Depart:         leg1.Depart,
		Arrive:         leg2.Arrive,
		Transfers:      1,
		WaitAtTransfer: []time.Duration{leg2.Depart.Sub(leg1.Arrive) - onFoot},
		TransferNeeded: []time.Duration{need},
	}
}

// journeysWithOneChange finds journeys made of two services and a change
// between them.
//
// There is no such query in a timetable: it knows about individual services and
// nothing about connecting them. So a change has to be constructed — pick a
// station in the middle, find something that reaches it, find something that
// leaves it, and check the connection can physically be made.
//
// The candidate set is bounded rather than searched exhaustively. This is not a
// general router: RAPTOR and the Connection Scan Algorithm solve that problem
// properly and this does not attempt to. It answers the common case, one
// change, and says so in its name.
func (ix *Index) journeysWithOneChange(ctx context.Context, from, to []string,
	after time.Time, within time.Duration, req PlanRequest) ([]Journey, error) {

	// Candidates are ranked by how soon they can be reached, so the useful ones
	// are at the front and a deep list only costs time. Each one is several
	// queries.
	const maxInterchanges = 12
	walk := req.MaxWalk
	if walk == 0 {
		walk = ix.policy.WalkRadius
	}
	if walk < 0 {
		walk = 0
	}
	stations, err := ix.interchanges(ctx, from, to, after, within, req.Modes, maxInterchanges, walk)
	if err != nil || len(stations) == 0 {
		return nil, err
	}
	if req.Via != "" {
		if stations, err = ix.restrictToVia(ctx, stations, req.Via); err != nil {
			return nil, err
		}
	}

	var out []Journey
	for _, station := range stations {
		ic, err := ix.openInterchange(ctx, station, walk)
		if err != nil {
			continue // a station we cannot resolve is not worth failing the search for
		}

		firsts, err := ix.directJourneys(ctx, from, ic.stopIDs, after, within, req.Modes)
		if err != nil {
			return nil, err
		}
		// Only the earliest few are worth pricing: a later train to the same
		// interchange cannot produce an earlier arrival, and each one costs
		// another onward query.
		const maxFirstsPerInterchange = 4
		if len(firsts) > maxFirstsPerInterchange {
			firsts = firsts[:maxFirstsPerInterchange]
		}
		if len(firsts) == 0 {
			continue
		}

		// Onward services are the same set whichever first leg you take; only
		// the earliest usable one differs. Querying once per interchange rather
		// than once per first leg is the difference between forty of these
		// searches and sixteen, and this query is almost the whole cost of
		// planning.
		soonest := firsts[0].Legs[0].Arrive
		for _, f := range firsts {
			if a := f.Legs[0].Arrive; a.Before(soonest) {
				soonest = a
			}
		}
		fromTime := soonest.Add(ix.policy.DefaultTransferTime + req.TransferBuffer)
		onward, err := ix.directJourneys(ctx, ic.stopIDs, to, fromTime, within-fromTime.Sub(after), req.Modes)
		if err != nil {
			return nil, err
		}
		if len(onward) == 0 {
			continue
		}

		for _, f := range firsts {
			leg1 := f.Legs[0]
			best, need, err := ix.catchable(ctx, leg1, onward, ic, walk, req.TransferBuffer)
			if err != nil {
				return nil, err
			}
			if best == nil {
				continue
			}
			out = append(out, ix.joinLegs(ctx, leg1, best.Legs[0], ic, need))
		}
	}
	return out, nil
}

// restrictToVia narrows the candidates to one the caller named.
//
// Asking to change at a particular station is a constraint, not a hint, so an
// unreachable one yields nothing rather than quietly routing elsewhere.
func (ix *Index) restrictToVia(ctx context.Context, stations []string, via string) ([]string, error) {
	wanted, err := ix.StopIDsForStation(ctx, via)
	if err != nil {
		return nil, fmt.Errorf("gtfs: Plan: Via %q: %w", via, err)
	}
	keep := make(map[string]bool, len(wanted))
	for _, id := range wanted {
		keep[id] = true
	}
	var only []string
	for _, s := range stations {
		if keep[s] {
			only = append(only, s)
		}
	}
	if len(only) == 0 {
		// The candidate search may not have surfaced it; trust the caller.
		return []string{via}, nil
	}
	return only, nil
}

// interchanges returns station stop ids reachable from the origin that also
// reach the destination.
func (ix *Index) interchanges(ctx context.Context, from, to []string, after time.Time,
	within time.Duration, modes []Mode, limit int, walk float64) ([]string, error) {

	day := midnight(after, ix.loc)
	services, err := ix.ServiceIDsOn(ctx, day)
	if err != nil || len(services) == 0 {
		return nil, err
	}
	fromSec := int(after.Sub(day).Seconds())
	toSec := fromSec + int(within.Seconds())

	// Stations this journey can reach from the origin, busiest first.
	reachable, err := ix.stationsFrom(ctx, from, fromSec, toSec, services, modes, 60)
	if err != nil {
		return nil, err
	}
	// Stations from which the destination can still be reached.
	serving, err := ix.stationsServing(ctx, to, fromSec, toSec, services, modes)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, limit)
	var needWalk []string
	for _, st := range reachable {
		if serving[st] {
			out = append(out, st)
			if len(out) >= limit {
				return out, nil
			}
			continue
		}
		needWalk = append(needWalk, st)
	}
	if walk <= 0 || len(out) >= limit {
		return out, nil
	}

	// Nothing here connects directly. A bus that sets down outside a station
	// shares no edge with it, so the walk has to be considered before the
	// interchange is dismissed: this is the difference between "no journey" and
	// a sixty-metre stroll onto the platform.
	for _, st := range needWalk {
		ids, err := ix.StopIDsForStation(ctx, st)
		if err != nil {
			continue
		}
		near, err := ix.walkableFrom(ctx, ids, walk)
		if err != nil {
			return nil, err
		}
		for _, n := range near {
			root := n.Parent
			if root == "" {
				root = n.ID
			}
			if serving[root] || serving[n.ID] {
				out = append(out, st)
				break
			}
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// stationsFrom lists stations reachable from a set of stops within the window,
// most onward services first.
func (ix *Index) stationsFrom(ctx context.Context, from []string, fromSec, toSec int,
	services map[string]bool, modes []Mode, limit int) ([]string, error) {

	q := `SELECT COALESCE(NULLIF(sb.parent,''), sb.stop_id) AS station, COUNT(*) AS n, MIN(b.arr) AS first_arr
	      FROM stop_time a
	      JOIN stop_time b ON b.trip_id = a.trip_id AND b.seq > a.seq
	      JOIN trip t ON t.trip_id = a.trip_id
	      JOIN stop sb ON sb.stop_id = b.stop_id
	      WHERE a.stop_id IN (` + placeholders(len(from)) + `)
	        AND a.dep BETWEEN ? AND ?
	        AND t.service_id IN (` + placeholders(len(services)) + `)`
	args := make([]any, 0, len(from)+len(services)+3+len(modes))
	for _, id := range from {
		args = append(args, id)
	}
	args = append(args, fromSec, toSec)
	for id := range services {
		args = append(args, id)
	}
	if len(modes) > 0 {
		q += " AND t.mode IN (" + placeholders(len(modes)) + ")"
		for _, m := range modes {
			args = append(args, int(m))
		}
	}
	// Rank by how soon the interchange can be reached, not by how many services
	// it offers. Service count favours large stations far past the destination,
	// because a terminus timetable is busy and being on the way counts for
	// nothing in a COUNT(*): a short city-to-suburb trip was routed through
	// three stations well beyond the destination in preference to the junction
	// two stops away.
	q += " GROUP BY station ORDER BY MIN(b.arr) ASC LIMIT ?"
	args = append(args, limit)

	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gtfs: interchanges: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		var n, firstArr int
		if err := rows.Scan(&id, &n, &firstArr); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// stationsServing lists stations from which the destination is reachable in the
// window, as a set for membership tests.
func (ix *Index) stationsServing(ctx context.Context, to []string, fromSec, toSec int,
	services map[string]bool, modes []Mode) (map[string]bool, error) {

	q := `SELECT DISTINCT COALESCE(NULLIF(sc.parent,''), sc.stop_id)
	      FROM stop_time c
	      JOIN stop_time d ON d.trip_id = c.trip_id AND d.seq > c.seq
	      JOIN trip u ON u.trip_id = c.trip_id
	      JOIN stop sc ON sc.stop_id = c.stop_id
	      WHERE d.stop_id IN (` + placeholders(len(to)) + `)
	        AND c.dep BETWEEN ? AND ?
	        AND u.service_id IN (` + placeholders(len(services)) + `)`
	args := make([]any, 0, len(to)+len(services)+2+len(modes))
	for _, id := range to {
		args = append(args, id)
	}
	args = append(args, fromSec, toSec)
	for id := range services {
		args = append(args, id)
	}
	if len(modes) > 0 {
		q += " AND u.mode IN (" + placeholders(len(modes)) + ")"
		for _, m := range modes {
			args = append(args, int(m))
		}
	}
	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gtfs: interchanges serving: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// defaultTransfer applies when the feed publishes no minimum for a pair. One
// minute is enough to change platforms at a station laid out for it, and this
// is only a fallback: transfers.txt covers the interchanges that matter.

func rankJourneys(j []Journey, r Rank) {
	switch r {
	case RankFewestTransfers:
		sort.SliceStable(j, func(a, b int) bool {
			if j[a].Transfers != j[b].Transfers {
				return j[a].Transfers < j[b].Transfers
			}
			return j[a].Arrive.Before(j[b].Arrive)
		})
	case RankShortest:
		sort.SliceStable(j, func(a, b int) bool {
			da, db := j[a].Duration(), j[b].Duration()
			if da != db {
				return da < db
			}
			// Same length: the earlier one is more useful.
			return j[a].Depart.Before(j[b].Depart)
		})
	case RankLeaveLatest:
		sort.SliceStable(j, func(a, b int) bool {
			if !j[a].Arrive.Equal(j[b].Arrive) {
				return j[a].Arrive.Before(j[b].Arrive)
			}
			return j[a].Depart.After(j[b].Depart)
		})
	default: // RankFastest
		sort.SliceStable(j, func(a, b int) bool {
			if !j[a].Arrive.Equal(j[b].Arrive) {
				return j[a].Arrive.Before(j[b].Arrive)
			}
			return j[a].Transfers < j[b].Transfers
		})
	}
}

func midnight(t time.Time, loc *time.Location) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// CityLoopStations are the three underground stations of Melbourne's City Loop.
//
// Southern Cross and Flinders Street are on the loop too, and are deliberately
// absent. The count measures the tunnel section because that is the part which
// is optional: Flagstaff, Melbourne Central and Parliament are reached only by
// going round, while the other two are destinations in their own right that
// many services reach without entering the loop at all.
//
// Including Southern Cross would misreport nearly half its traffic. Of the
// trips calling there, 5,929 of 12,154 never touch the three tunnel stations —
// the Craigieburn, Upfield, Werribee and Williamstown lines pass through at
// surface level. A further 1,931 trips run the tunnels and skip Southern Cross,
// so it is not reliably part of a loop run in either direction.
var CityLoopStations = []string{
	"vic:rail:FGS", // Flagstaff
	"vic:rail:MCE", // Melbourne Central
	"vic:rail:PAR", // Parliament
}

// CityLoopStops counts the City Loop stations a journey passes through without
// starting or ending at one.
//
// The loop reverses direction between morning and afternoon, so whether it is
// on the way or a detour depends on the time of day as much as the destination.
// Riding it out of the city can add three stations to a trip that a change at
// Richmond or Flinders Street avoids entirely, which is the difference this
// number is for.
func (ix *Index) CityLoopStops(ctx context.Context, j Journey) (int, error) {
	loop := make(map[string]bool, len(CityLoopStations))
	for _, s := range CityLoopStations {
		loop[s] = true
	}

	total := 0
	for _, l := range j.Legs {
		rows, err := ix.db.QueryContext(ctx,
			`SELECT COALESCE(NULLIF(s.parent,''), s.stop_id)
			 FROM stop_time st JOIN stop s ON s.stop_id = st.stop_id
			 WHERE st.trip_id = ? AND st.seq > ? AND st.seq < ?`,
			l.TripID, l.FromSeq, l.ToSeq)
		if err != nil {
			return 0, fmt.Errorf("gtfs: CityLoopStops: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, err
			}
			if loop[id] {
				total++
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
	}
	return total, nil
}

// Walking between stops.
//
// Victoria's feed connects platforms within a station and nothing else. A bus
// that sets down 66 metres from a railway station shares no edge with it:
// different mode, no common parent, no transfers.txt row, no pathway. Without a
// synthetic walking edge the planner cannot see a connection every passenger
// makes without thinking about it, and a suburban bus stop appears to reach
// nothing at all.
//
// The edge is built from stop coordinates, which the feed does carry for every
// stop. It is an estimate and is labelled as one: a straight line between two
// points says nothing about the road, the crossing, or the railway line in
// between.

// walkableFrom returns stops within radius of any of the given stops, excluding
// the stops themselves and their own station siblings.
//
// One query covers the whole station. Asking per stop instead meant 59 geo
// lookups for Flinders Street, each re-reading much the same pages, and tripled
// the cost of planning from a large interchange.
func (ix *Index) walkableFrom(ctx context.Context, stopIDs []string, radius float64) ([]NearbyStop, error) {
	if radius <= 0 || len(stopIDs) == 0 {
		return nil, nil
	}
	exclude := make(map[string]bool, len(stopIDs))
	origins := make([][2]float64, 0, len(stopIDs))
	for _, id := range stopIDs {
		exclude[id] = true
		var lat, lon float64
		if err := ix.db.QueryRowContext(ctx,
			`SELECT lat,lon FROM stop WHERE stop_id=?`, id).Scan(&lat, &lon); err != nil {
			continue
		}
		if lat != 0 || lon != 0 {
			origins = append(origins, [2]float64{lat, lon})
		}
	}
	if len(origins) == 0 {
		return nil, nil
	}

	// A single box around every platform of the station, grown by the radius.
	minLat, maxLat := origins[0][0], origins[0][0]
	minLon, maxLon := origins[0][1], origins[0][1]
	for _, o := range origins {
		minLat = math.Min(minLat, o[0])
		maxLat = math.Max(maxLat, o[0])
		minLon = math.Min(minLon, o[1])
		maxLon = math.Max(maxLon, o[1])
	}
	const degPerMetreLat = 1.0 / 111320.0
	dLat := radius * degPerMetreLat
	cosLat := math.Cos(minLat * math.Pi / 180)
	if cosLat < 0.01 {
		cosLat = 0.01
	}
	dLon := dLat / cosLat

	rows, err := ix.db.QueryContext(ctx,
		`SELECT stop_id,name,lat,lon,COALESCE(parent,''),mode,COALESCE(platform,'')
		 FROM stop
		 WHERE lat BETWEEN ? AND ? AND lon BETWEEN ? AND ? AND lat <> 0`,
		minLat-dLat, maxLat+dLat, minLon-dLon, maxLon+dLon)
	if err != nil {
		return nil, fmt.Errorf("gtfs: walkableFrom: %w", err)
	}
	defer rows.Close()

	var candidates []Stop
	for rows.Next() {
		var st Stop
		var m int
		if err := rows.Scan(&st.ID, &st.Name, &st.Lat, &st.Lon, &st.Parent, &m, &st.Platform); err != nil {
			return nil, err
		}
		st.Mode = Mode(m)
		candidates = append(candidates, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []NearbyStop
	for _, st := range dedupeByStation(candidates) {
		if exclude[st.ID] {
			continue
		}
		// A stop of the same station is reached by platform expansion, not on
		// foot; walking to it is not a separate leg.
		if st.Parent != "" && exclude[st.Parent] {
			continue
		}
		best := math.MaxFloat64
		for _, o := range origins {
			if d := haversineMetres(o[0], o[1], st.Lat, st.Lon); d < best {
				best = d
			}
		}
		if best <= radius {
			out = append(out, NearbyStop{Stop: st, Metres: best})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Metres < out[b].Metres })
	return out, nil
}

// walkDistance reports the walk between the two stops a connection actually
// uses. Same stop, or two platforms of one station, is not a walk in this
// sense: [Index.TransferTime] already covers those.
//
// The distance is measured between those two stops rather than read from the
// candidate set, because the set is seeded from every stop of the interchange
// and the nearest one is rarely the one the journey uses.
func (ix *Index) walkDistance(ctx context.Context, walkTo map[string]float64, from, to string) (float64, bool) {
	if from == to {
		return 0, false
	}
	_, a := walkTo[from]
	_, b := walkTo[to]
	if !a && !b {
		return 0, false
	}
	var alat, alon, blat, blon float64
	if err := ix.db.QueryRowContext(ctx, `SELECT lat,lon FROM stop WHERE stop_id=?`, from).Scan(&alat, &alon); err != nil {
		return 0, false
	}
	if err := ix.db.QueryRowContext(ctx, `SELECT lat,lon FROM stop WHERE stop_id=?`, to).Scan(&blat, &blon); err != nil {
		return 0, false
	}
	if alat == 0 || blat == 0 {
		return 0, false
	}
	return haversineMetres(alat, alon, blat, blon), true
}

// totalWalk is how far a journey asks the traveller to walk.
func totalWalk(j Journey) float64 {
	var m float64
	for _, l := range j.Legs {
		if l.Walk {
			m += l.WalkMetres
		}
	}
	return m
}

// filterDominated removes journeys that are worse than another on every axis a
// traveller cares about.
//
// Without this the planner offers the same train several times over: board a
// different service at Flinders Street, ride one stop into the City Loop, get
// off, and board the train you could have stayed on. Same arrival, an extra
// change, and sometimes an earlier departure to achieve nothing. Each is a
// valid path through the timetable and none is worth showing.
//
// A journey is dominated when another leaves no earlier, arrives no later, and
// needs no more changes, with at least one of those strictly better. Ties on
// all three keep the first, so an equally good alternative still survives.
func filterDominated(in []Journey) []Journey {
	if len(in) < 2 {
		return in
	}
	keep := make([]bool, len(in))
	for i := range keep {
		keep[i] = true
	}
	for i, a := range in {
		if !keep[i] {
			continue
		}
		for j, b := range in {
			if i == j || !keep[j] {
				continue
			}
			if dominates(a, b) {
				keep[j] = false
			}
		}
	}
	out := make([]Journey, 0, len(in))
	for i, j := range in {
		if keep[i] {
			out = append(out, j)
		}
	}
	return out
}

// dominates reports whether a is at least as good as b everywhere and better
// somewhere. Leaving later is better: it is time not spent waiting.
func dominates(a, b Journey) bool {
	notWorse := !a.Depart.Before(b.Depart) &&
		!a.Arrive.After(b.Arrive) &&
		a.Transfers <= b.Transfers
	if !notWorse {
		return false
	}
	return a.Depart.After(b.Depart) || a.Arrive.Before(b.Arrive) || a.Transfers < b.Transfers
}

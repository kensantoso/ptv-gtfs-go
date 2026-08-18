package gtfs

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Index is a handle for querying a database built by [Build]. It holds no
// state of its own beyond the connection, the network timezone and the
// [Policy], and is safe for concurrent use.
type Index struct {
	db     *sql.DB
	loc    *time.Location
	policy Policy
	// paths is the station walking graph, loaded on first use because most
	// queries never need it.
	paths pathGraph
}

// Open wraps an already-built database. loc is the network's local timezone; GTFS
// times are local to the operator, never UTC.
func Open(db *sql.DB, loc *time.Location, opts ...Option) *Index {
	if loc == nil {
		loc = time.Local
	}
	ix := &Index{db: db, loc: loc, policy: DefaultPolicy()}
	for _, o := range opts {
		o(ix)
	}
	return ix
}

// Policy reports the judgements this handle is operating under.
func (ix *Index) Policy() Policy { return ix.policy }

// Stop is a stop or platform.
type Stop struct {
	ID       string
	Name     string
	Lat, Lon float64
	Parent   string
	Mode     Mode
	Platform string
}

// FindStops matches stops by name, case-insensitively.
//
// Results are grouped by parent station where one exists, because a rail
// station appears once per platform and a caller almost always means the
// station rather than platform 3.
func (ix *Index) FindStops(ctx context.Context, name string, modes ...Mode) ([]Stop, error) {
	q := `SELECT stop_id,name,COALESCE(lat,0),COALESCE(lon,0),COALESCE(parent,''),mode,COALESCE(platform,'')
	      FROM stop WHERE name LIKE ? COLLATE NOCASE`
	args := []any{"%" + name + "%"}
	if len(modes) > 0 {
		q += " AND mode IN (" + placeholders(len(modes)) + ")"
		for _, m := range modes {
			args = append(args, int(m))
		}
	}
	q += " ORDER BY LENGTH(name), name LIMIT 400"

	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gtfs: FindStops: %w", err)
	}
	defer rows.Close()

	var all []Stop
	for rows.Next() {
		var s Stop
		var m int
		if err := rows.Scan(&s.ID, &s.Name, &s.Lat, &s.Lon, &s.Parent, &m, &s.Platform); err != nil {
			return nil, err
		}
		s.Mode = Mode(m)
		all = append(all, s)
	}
	return dedupeByStation(all), rows.Err()
}

// dedupeByStation collapses a station's platforms into one result.
//
// Keying on parent alone is not enough: a feed contains both the platform rows
// (parent set) and the station row itself (parent empty, id equal to that
// parent), so they land under different keys and the same station is returned
// twice. Keying children by their parent and parents by their own id makes them
// collide, and the station record then wins because it is the one a caller
// means.
func dedupeByStation(in []Stop) []Stop {
	key := func(s Stop) string {
		if s.Parent != "" {
			return s.Parent
		}
		if isStationID(s.ID) {
			return s.ID
		}
		// Stops with no station grouping at all, such as roadside bus and tram
		// stops, group by name and mode so both directions do not both appear.
		return s.Name + "|" + fmt.Sprint(s.Mode)
	}

	best := map[string]Stop{}
	var order []string
	for _, s := range in {
		k := key(s)
		prev, seen := best[k]
		if !seen {
			best[k] = s
			order = append(order, k)
			continue
		}
		// Prefer the station record over one of its platforms.
		if s.ID == k && prev.ID != k {
			best[k] = s
		}
	}
	out := make([]Stop, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

// isStationID reports whether an id looks like a parent station rather than a
// platform. Victoria uses a "vic:rail:XXX" form for stations and bare numbers
// for platforms.
func isStationID(id string) bool { return strings.Contains(id, ":") }

// StopIDsForStation returns every platform-level stop id belonging to the same
// station as stopID.
//
// Departure queries must span all of them: a train leaves from a platform, not
// from the station, so querying a single stop id silently misses most services.
func (ix *Index) StopIDsForStation(ctx context.Context, stopID string) ([]string, error) {
	var parent, name string
	var mode int
	err := ix.db.QueryRowContext(ctx,
		`SELECT COALESCE(parent,''),name,mode FROM stop WHERE stop_id=?`, stopID).
		Scan(&parent, &name, &mode)
	if err != nil {
		return nil, fmt.Errorf("gtfs: stop %s: %w", stopID, err)
	}

	// Resolve to the station root first. The caller may pass either a platform
	// or the station itself, and both must expand to the same set.
	root := parent
	if root == "" {
		root = stopID
	}

	rows, err := ix.db.QueryContext(ctx,
		`SELECT stop_id FROM stop WHERE parent=? OR stop_id=?`, root, root)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Only one result means the stop has no station grouping, which is normal
	// for roadside bus and tram stops. Fall back to name and mode so both
	// directions of the same stop are covered.
	//
	// This fallback must not be used for stations: their platform rows carry a
	// different name from the station row ("Flinders Street Station" against
	// "Flinders Street Railway Station"), so name matching finds nothing and every
	// departure is silently missed.
	if len(ids) <= 1 {
		byName, err := ix.db.QueryContext(ctx,
			`SELECT stop_id FROM stop WHERE name=? AND mode=?`, name, mode)
		if err != nil {
			return nil, err
		}
		defer byName.Close()
		ids = ids[:0]
		for byName.Next() {
			var id string
			if err := byName.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		if err := byName.Err(); err != nil {
			return nil, err
		}
	}
	if len(ids) == 0 {
		ids = []string{stopID}
	}
	return ids, nil
}

// ServiceIDsOn returns the services running on a date, applying calendar_dates
// exceptions over the regular calendar pattern.
func (ix *Index) ServiceIDsOn(ctx context.Context, date time.Time) (map[string]bool, error) {
	ymd := date.Format("20060102")
	dow := int(date.Weekday())

	out := map[string]bool{}
	rows, err := ix.db.QueryContext(ctx,
		`SELECT service_id FROM calendar WHERE dow=? AND start_date<=? AND end_date>=?`,
		dow, ymd, ymd)
	if err != nil {
		return nil, fmt.Errorf("gtfs: calendar: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Exceptions override, and can name a service absent from calendar.txt.
	ex, err := ix.db.QueryContext(ctx,
		`SELECT service_id,added FROM calendar_date WHERE date=?`, ymd)
	if err != nil {
		return nil, fmt.Errorf("gtfs: calendar_date: %w", err)
	}
	defer ex.Close()
	for ex.Next() {
		var id string
		var added int
		if err := ex.Scan(&id, &added); err != nil {
			return nil, err
		}
		if added == 1 {
			out[id] = true
		} else {
			delete(out, id)
		}
	}
	return out, ex.Err()
}

// Departure is one service leaving one stop.
type Departure struct {
	TripID    string
	RouteID   string
	StopID    string
	Seq       int
	DepartSec int // seconds after service-day midnight
	Depart    time.Time
	Headsign  string
	RouteName string
	Mode      Mode
	Platform  string
	// Replacement marks a bus running in place of the train it replaces. See
	// [Leg.Replacement]: these are published on the train mode.
	Replacement bool
	// Alts are other trip ids the feed publishes for this same physical
	// service; see [dedupeDepartures]. Realtime describes only one of them, so
	// anything joining live data must try these too.
	Alts []AltTrip
}

// AltTrip is a duplicate publication of a service, kept so realtime can still
// be matched against it. Seq travels with the id because the variants often
// number their stops differently.
type AltTrip struct {
	TripID string
	Seq    int
}

// DeparturesRequest selects departures from a station.
type DeparturesRequest struct {
	StopID string    // any stop id at the station; siblings are included
	After  time.Time // defaults to now
	Within time.Duration
	Modes  []Mode
	Limit  int
}

// Departures returns services leaving a station, soonest first.
//
// The window is evaluated against two service days, not one. A query at 00:30
// must see trips the previous day's timetable expresses as 24:30, and looking
// only at today would report the last train as having already gone.
func (ix *Index) Departures(ctx context.Context, req DeparturesRequest) ([]Departure, error) {
	if req.StopID == "" {
		return nil, fmt.Errorf("gtfs: Departures: StopID is required")
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
		within = 2 * time.Hour
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}

	ids, err := ix.StopIDsForStation(ctx, req.StopID)
	if err != nil {
		return nil, err
	}

	var out []Departure
	// Today, and yesterday for after-midnight services still running.
	for _, dayOffset := range []int{0, -1} {
		day := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, ix.loc).
			AddDate(0, 0, dayOffset)
		services, err := ix.ServiceIDsOn(ctx, day)
		if err != nil {
			return nil, err
		}
		if len(services) == 0 {
			continue
		}
		fromSec := int(after.Sub(day).Seconds())
		toSec := fromSec + int(within.Seconds())
		if fromSec < 0 {
			continue
		}

		deps, err := ix.departuresOnDay(ctx, ids, day, services, fromSec, toSec, req.Modes, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, deps...)
	}

	sortDepartures(out)
	// Dedupe before the limit, or the limit is spent on duplicates and a
	// request for ten departures returns three real ones.
	out = dedupeDepartures(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// dedupeDepartures collapses the several trips Victoria publishes for one
// physical service.
//
// A single Saturday train appears in the feed as trips 02-BEG--1-T2-3026,
// 02-BEG--62-T2-3026 and 02-BEG--63-T2-3026: the same run number under
// different stopping patterns, each with its own calendar entry, and on any
// given date more than one of those calendars is active. Left alone this
// triples every metro departure count, so "trains left tonight" and every
// journey list read roughly three times too high.
//
// Two services cannot leave the same stop at the same second on the same route
// towards the same place, so that combination identifies the physical service
// without relying on the shape of an id. The survivor keeps the others as
// [AltTrip]s because realtime publishes under whichever variant it chooses.
//
// Headsign stays in the key even though the run number alone would collapse
// more. Run 3627 is published both as a Bayswater service of 24 stops and a
// Belgrave service of 30, and nothing in the static feed says which is real.
// Merging them would mean announcing a destination the train may not reach,
// so both survive here and [Live.Disambiguate] resolves them where realtime
// can.
func dedupeDepartures(in []Departure) []Departure {
	if len(in) < 2 {
		return in
	}
	type key struct {
		stop     string
		at       int64
		route    string
		headsign string
	}
	seen := make(map[key]int, len(in))
	out := make([]Departure, 0, len(in))
	for _, d := range in {
		k := key{d.StopID, d.Depart.Unix(), d.RouteID, d.Headsign}
		if i, ok := seen[k]; ok {
			out[i].Alts = append(out[i].Alts, AltTrip{TripID: d.TripID, Seq: d.Seq})
			continue
		}
		seen[k] = len(out)
		out = append(out, d)
	}
	return out
}

func (ix *Index) departuresOnDay(ctx context.Context, stopIDs []string, day time.Time,
	services map[string]bool, fromSec, toSec int, modes []Mode, limit int) ([]Departure, error) {

	q := `SELECT st.trip_id, t.route_id, st.stop_id, st.seq, st.dep,
	             COALESCE(t.headsign,''), COALESCE(r.long_name, r.short_name, ''), t.mode,
	             COALESCE(s.platform,''), t.service_id,
	             CASE WHEN t.route_id LIKE '%-R:' THEN 1 ELSE 0 END
	      FROM stop_time st
	      JOIN trip  t ON t.trip_id  = st.trip_id
	      JOIN route r ON r.route_id = t.route_id
	      JOIN stop  s ON s.stop_id  = st.stop_id
	      WHERE st.stop_id IN (` + placeholders(len(stopIDs)) + `)
	        AND st.dep BETWEEN ? AND ?`
	args := make([]any, 0, len(stopIDs)+4+len(modes))
	for _, id := range stopIDs {
		args = append(args, id)
	}
	args = append(args, fromSec, toSec)
	if len(modes) > 0 {
		q += " AND t.mode IN (" + placeholders(len(modes)) + ")"
		for _, m := range modes {
			args = append(args, int(m))
		}
	}
	// Over-fetch generously. Most candidate rows belong to services not running
	// on this day and are filtered after the query, so a small multiplier
	// silently returns fewer departures than asked for. A busy interchange can
	// have dozens of inactive trips per active one.
	overfetch := limit * 60
	if overfetch > 5000 {
		overfetch = 5000
	}
	q += " ORDER BY st.dep LIMIT ?"
	args = append(args, overfetch)

	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gtfs: departures: %w", err)
	}
	defer rows.Close()

	var out []Departure
	for rows.Next() {
		var d Departure
		var m, replacement int
		var serviceID string
		if err := rows.Scan(&d.TripID, &d.RouteID, &d.StopID, &d.Seq, &d.DepartSec,
			&d.Headsign, &d.RouteName, &m, &d.Platform, &serviceID, &replacement); err != nil {
			return nil, err
		}
		d.Replacement = replacement == 1
		if !services[serviceID] {
			continue // not running on this service day
		}
		d.Mode = Mode(m)
		d.Depart = Instant(day, d.DepartSec, ix.loc)
		out = append(out, d)
	}
	return out, rows.Err()
}

func sortDepartures(d []Departure) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j].Depart.Before(d[j-1].Depart); j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// StopName returns a stop's display name.
// Stop returns one stop by id.
//
// [Index.FindStops] searches by name and [Index.StopsNear] by proximity; this is
// the plain lookup for an id you already hold, which is what a caller has after
// any of the other queries.
func (ix *Index) Stop(ctx context.Context, stopID string) (Stop, error) {
	var s Stop
	var mode int
	var parent, platform sql.NullString
	err := ix.db.QueryRowContext(ctx,
		`SELECT stop_id, name, lat, lon, parent, mode, platform FROM stop WHERE stop_id=?`,
		stopID).Scan(&s.ID, &s.Name, &s.Lat, &s.Lon, &parent, &mode, &platform)
	if err != nil {
		return Stop{}, fmt.Errorf("gtfs: stop %s: %w", stopID, err)
	}
	s.Parent, s.Platform, s.Mode = parent.String, platform.String, Mode(mode)
	return s, nil
}

func (ix *Index) StopName(ctx context.Context, stopID string) (string, error) {
	var name string
	err := ix.db.QueryRowContext(ctx, `SELECT name FROM stop WHERE stop_id=?`, stopID).Scan(&name)
	if err != nil {
		return stopID, fmt.Errorf("gtfs: stop %s: %w", stopID, err)
	}
	return name, nil
}

// RouteNames maps route id to line name, for turning the ids realtime uses
// into the words a passenger uses.
func (ix *Index) RouteNames(ctx context.Context, modes ...Mode) (map[string]string, error) {
	q := `SELECT route_id, COALESCE(NULLIF(long_name,''), short_name, '') FROM route`
	var args []any
	if len(modes) > 0 {
		q += " WHERE mode IN (" + placeholders(len(modes)) + ")"
		for _, m := range modes {
			args = append(args, int(m))
		}
	}
	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gtfs: RouteNames: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// RouteIDForTrip returns the route a trip belongs to. Realtime route ids are
// unreliable on the bus feed, so this is how a leg's route should be resolved.
func (ix *Index) RouteIDForTrip(ctx context.Context, tripID string) (string, error) {
	var id string
	err := ix.db.QueryRowContext(ctx,
		`SELECT route_id FROM trip WHERE trip_id=?`, tripID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("gtfs: RouteIDForTrip %q: %w", tripID, err)
	}
	return id, nil
}

// NearbyStop is a stop with its distance from a point.
type NearbyStop struct {
	Stop
	// Metres is straight-line distance. Real walking is longer, and a river or a
	// rail corridor can make it much longer, so this bounds the walk rather than
	// estimating it.
	Metres float64
}

// StopsNear returns stops closest to a point, nearest first.
//
// Distance is great-circle, not walking distance: the feed carries no path
// between stations, only within them. Callers presenting this to a person should
// say so rather than imply a route has been found.
func (ix *Index) StopsNear(ctx context.Context, lat, lon float64, radiusMetres float64, limit int, modes ...Mode) ([]NearbyStop, error) {
	if radiusMetres <= 0 {
		radiusMetres = 1000
	}
	if limit <= 0 {
		limit = 10
	}
	// A bounding box first so the geo index does the narrowing; the trigonometry
	// then only runs over candidates rather than all 30,967 stops.
	const degPerMetreLat = 1.0 / 111320.0
	dLat := radiusMetres * degPerMetreLat
	cosLat := math.Cos(lat * math.Pi / 180)
	if cosLat < 0.01 {
		cosLat = 0.01
	}
	dLon := dLat / cosLat

	q := `SELECT stop_id,name,lat,lon,COALESCE(parent,''),mode,COALESCE(platform,'')
	      FROM stop
	      WHERE lat BETWEEN ? AND ? AND lon BETWEEN ? AND ? AND lat <> 0`
	args := []any{lat - dLat, lat + dLat, lon - dLon, lon + dLon}
	if len(modes) > 0 {
		q += " AND mode IN (" + placeholders(len(modes)) + ")"
		for _, m := range modes {
			args = append(args, int(m))
		}
	}

	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gtfs: StopsNear: %w", err)
	}
	defer rows.Close()

	var all []Stop
	for rows.Next() {
		var s Stop
		var m int
		if err := rows.Scan(&s.ID, &s.Name, &s.Lat, &s.Lon, &s.Parent, &m, &s.Platform); err != nil {
			return nil, err
		}
		s.Mode = Mode(m)
		all = append(all, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Collapse platforms to their station before measuring, so a rail station
	// appears once rather than once per platform.
	out := make([]NearbyStop, 0, len(all))
	for _, s := range dedupeByStation(all) {
		d := haversineMetres(lat, lon, s.Lat, s.Lon)
		if d <= radiusMetres {
			out = append(out, NearbyStop{Stop: s, Metres: d})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Metres < out[b].Metres })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func haversineMetres(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	p := math.Pi / 180
	dLat := (lat2 - lat1) * p
	dLon := (lon2 - lon1) * p
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*p)*math.Cos(lat2*p)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return r * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// Call is one station a leg passes, whether it stops there or not.
type Call struct {
	StopID   string
	Name     string
	Platform string
	// Time is when this service calls. Zero when Skipped.
	Time time.Time
	// Skipped marks a station the service runs through without stopping. It is
	// what makes an express legible: "eleven stops" and "runs through eleven
	// stations" are different journeys and the timetable shows only the first.
	Skipped bool
}

// LegCalls lists the stations a leg passes between two points of a trip,
// marking those it runs through.
//
// Stations skipped are found by comparing against the fullest stopping pattern
// any service on the same route makes over the same span. The feed has no
// notion of "the all-stations service", so the longest one observed stands in
// for it.
func (ix *Index) LegCalls(ctx context.Context, tripID string, fromSeq, toSeq int) ([]Call, error) {
	rows, err := ix.db.QueryContext(ctx,
		`SELECT st.stop_id, s.name, COALESCE(s.platform,''), st.dep, st.seq
		 FROM stop_time st JOIN stop s ON s.stop_id = st.stop_id
		 WHERE st.trip_id = ? AND st.seq BETWEEN ? AND ? ORDER BY st.seq`,
		tripID, fromSeq, toSeq)
	if err != nil {
		return nil, fmt.Errorf("gtfs: LegCalls: %w", err)
	}
	defer rows.Close()

	day := midnight(time.Now().In(ix.loc), ix.loc)
	var made []Call
	var stations []string
	for rows.Next() {
		var c Call
		var dep, seq int
		if err := rows.Scan(&c.StopID, &c.Name, &c.Platform, &dep, &seq); err != nil {
			return nil, err
		}
		c.Time = Instant(day, dep, ix.loc)
		made = append(made, c)
		stations = append(stations, c.StopID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(made) < 2 {
		return made, nil
	}

	// The fullest pattern over the same span, from any service on this route.
	var routeID string
	if err := ix.db.QueryRowContext(ctx,
		`SELECT route_id FROM trip WHERE trip_id=?`, tripID).Scan(&routeID); err != nil {
		return made, nil
	}
	first, last := made[0], made[len(made)-1]
	var refTrip string
	err = ix.db.QueryRowContext(ctx,
		`SELECT a.trip_id
		 FROM stop_time a
		 JOIN stop_time b ON b.trip_id = a.trip_id AND b.seq > a.seq
		 JOIN trip t ON t.trip_id = a.trip_id
		 JOIN stop sa ON sa.stop_id = a.stop_id
		 JOIN stop sb ON sb.stop_id = b.stop_id
		 WHERE t.route_id = ?
		   AND COALESCE(NULLIF(sa.parent,''),sa.stop_id) = (SELECT COALESCE(NULLIF(parent,''),stop_id) FROM stop WHERE stop_id=?)
		   AND COALESCE(NULLIF(sb.parent,''),sb.stop_id) = (SELECT COALESCE(NULLIF(parent,''),stop_id) FROM stop WHERE stop_id=?)
		 ORDER BY (b.seq - a.seq) DESC LIMIT 1`,
		routeID, first.StopID, last.StopID).Scan(&refTrip)
	if err != nil || refTrip == tripID {
		return made, nil
	}

	ref, err := ix.db.QueryContext(ctx,
		`SELECT s.name, COALESCE(NULLIF(s.parent,''),s.stop_id)
		 FROM stop_time st JOIN stop s ON s.stop_id = st.stop_id
		 WHERE st.trip_id = ? ORDER BY st.seq`, refTrip)
	if err != nil {
		return made, nil
	}
	defer ref.Close()

	calledAt := make(map[string]bool, len(made))
	for _, c := range made {
		var root string
		ix.db.QueryRowContext(ctx,
			`SELECT COALESCE(NULLIF(parent,''),stop_id) FROM stop WHERE stop_id=?`, c.StopID).Scan(&root)
		calledAt[root] = true
	}

	// Walk the reference pattern, slotting skipped stations between the calls
	// this service does make.
	var out []Call
	i := 0
	var started bool
	for ref.Next() {
		var name, root string
		if err := ref.Scan(&name, &root); err != nil {
			break
		}
		if calledAt[root] {
			if !started {
				started = true
			}
			if i < len(made) {
				out = append(out, made[i])
				i++
			}
			continue
		}
		if started && i < len(made) {
			out = append(out, Call{Name: name, StopID: root, Skipped: true})
		}
	}
	for ; i < len(made); i++ {
		out = append(out, made[i])
	}
	if len(out) == 0 {
		return made, nil
	}
	return out, nil
}

// FeedRangeError reports a query outside the period the feed describes.
//
// It exists because the alternative is silence. A GTFS feed covers a fixed
// timetable period, and past its end every query matches no services and
// returns an empty result with no error: the caller cannot tell "nothing runs
// then" from "my data ran out in November". That is the same class of mistake
// as reporting absent realtime as on time, and it fails on a date rather than
// gradually, so nothing warns you first.
type FeedRangeError struct {
	Date time.Time
	From time.Time
	To   time.Time
}

func (e *FeedRangeError) Error() string {
	switch {
	case e.From.IsZero() || e.To.IsZero():
		return "gtfs: the database records no timetable period; rebuild it"
	case e.Date.After(e.To):
		return fmt.Sprintf("gtfs: the timetable ran out on %s and this query is for %s; rebuild the database",
			e.To.Format("2 Jan 2006"), e.Date.Format("2 Jan 2006"))
	default:
		return fmt.Sprintf("gtfs: the timetable starts on %s and this query is for %s",
			e.From.Format("2 Jan 2006"), e.Date.Format("2 Jan 2006"))
	}
}

// Validity is the period the indexed feed describes.
//
// Read from the database rather than recomputed, so it costs nothing per query.
// Falls back to scanning the calendar for a database built before this was
// recorded.
func (ix *Index) Validity(ctx context.Context) (from, to time.Time, err error) {
	var a, b string
	err = ix.db.QueryRowContext(ctx,
		`SELECT
		   (SELECT value FROM meta WHERE key='valid_from'),
		   (SELECT value FROM meta WHERE key='valid_to')`).Scan(&a, &b)
	if err != nil || a == "" || b == "" {
		if err2 := ix.db.QueryRowContext(ctx,
			`SELECT MIN(start_date), MAX(end_date) FROM calendar`).Scan(&a, &b); err2 != nil {
			return time.Time{}, time.Time{}, nil // nothing to say; treat as unbounded
		}
	}
	from, _ = time.ParseInLocation("20060102", a, ix.loc)
	to, _ = time.ParseInLocation("20060102", b, ix.loc)
	if !to.IsZero() {
		// end_date is inclusive: services run to the end of that day.
		to = to.Add(24*time.Hour - time.Second)
	}
	return from, to, nil
}

// checkRange rejects a query outside the feed's period.
func (ix *Index) checkRange(ctx context.Context, when time.Time) error {
	from, to, err := ix.Validity(ctx)
	if err != nil || from.IsZero() || to.IsZero() {
		return nil
	}
	// A query at 00:30 legitimately reaches back to the previous service day.
	if when.Before(from.Add(-24*time.Hour)) || when.After(to) {
		return &FeedRangeError{Date: when, From: from, To: to}
	}
	return nil
}

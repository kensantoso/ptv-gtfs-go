package realtime

import (
	"fmt"
	"time"

	pb "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

// Data is one decoded feed message.
//
// A feed message may in principle carry any mix of the three entity types.
// Victoria publishes them on separate endpoints, so in practice exactly one of
// these slices is populated, but decoding all three keeps callers from having
// to know which endpoint they fetched.
type Data struct {
	// At is the publisher's timestamp for the snapshot, not the time it was
	// fetched. Staleness is measured against this.
	At          time.Time
	TripUpdates []TripUpdate
	Alerts      []Alert
	Vehicles    []VehiclePosition
	Incremental bool
}

// TripUpdate is realtime progress for one scheduled trip.
//
// TripID joins directly to the static feed. RouteID does not, reliably: on the
// bus feed the realtime route ids use a different scheme from the static ones
// and match nothing, so anything needing a route should resolve it from the
// trip instead.
type TripUpdate struct {
	TripID    string
	RouteID   string
	StartDate string // service date as YYYYMMDD
	StartTime string // scheduled start as HH:MM:SS
	Canceled  bool
	At        time.Time
	Stops     []StopUpdate // ordered by Sequence
}

// StopEvent is a predicted arrival or departure. Set distinguishes a genuine
// zero delay from no prediction at all, which is the difference between "on
// time" and "not known".
type StopEvent struct {
	Set   bool
	Delay time.Duration // signed; negative is early
	Time  time.Time     // absolute prediction, zero if not supplied
}

// StopUpdate is the prediction for one call of a trip.
type StopUpdate struct {
	StopID    string
	Sequence  int
	Skipped   bool // trip runs but will not call here
	Arrival   StopEvent
	Departure StopEvent
}

// Alert is a service disruption.
//
// Cause and Effect are frequently UNKNOWN_CAUSE and UNKNOWN_EFFECT even for
// real incidents, and Header is often a bare category such as "Minor Delay" or
// "PlannedOccupation". Description carries the actual substance and is the
// field worth showing.
type Alert struct {
	Cause       string
	Effect      string
	Header      string
	Description string
	URL         string
	Routes      []string
	Stops       []string
	Trips       []string
	Start       time.Time // zero means no stated start
	End         time.Time // zero means no stated end
}

// ActiveAt reports whether the alert applies at t. An alert with no stated
// period is treated as always active, which is how the tram feed publishes
// most of its notices.
func (a Alert) ActiveAt(t time.Time) bool {
	if !a.Start.IsZero() && t.Before(a.Start) {
		return false
	}
	if !a.End.IsZero() && t.After(a.End) {
		return false
	}
	return true
}

// VehiclePosition is where a vehicle currently is.
type VehiclePosition struct {
	TripID    string
	RouteID   string
	VehicleID string
	Label     string
	Lat, Lon  float64
	Bearing   float32
	Speed     float32
	StopID    string
	Status    string
	At        time.Time
}

// Decode parses a protobuf feed body, as returned by
// [Client.Fetch].
func Decode(body []byte) (*Data, error) {
	var msg pb.FeedMessage
	if err := proto.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("realtime: decode: %w", err)
	}
	h := msg.GetHeader()
	out := &Data{
		At:          time.Unix(int64(h.GetTimestamp()), 0),
		Incremental: h.GetIncrementality() == pb.FeedHeader_DIFFERENTIAL,
	}
	for _, e := range msg.GetEntity() {
		switch {
		case e.TripUpdate != nil:
			out.TripUpdates = append(out.TripUpdates, decodeTripUpdate(e.TripUpdate))
		case e.Alert != nil:
			out.Alerts = append(out.Alerts, decodeAlert(e.Alert))
		case e.Vehicle != nil:
			out.Vehicles = append(out.Vehicles, decodeVehicle(e.Vehicle))
		}
	}
	return out, nil
}

func decodeTripUpdate(tu *pb.TripUpdate) TripUpdate {
	t := tu.GetTrip()
	out := TripUpdate{
		TripID:    t.GetTripId(),
		RouteID:   t.GetRouteId(),
		StartDate: t.GetStartDate(),
		StartTime: t.GetStartTime(),
		Canceled:  t.GetScheduleRelationship() == pb.TripDescriptor_CANCELED,
		At:        time.Unix(int64(tu.GetTimestamp()), 0),
	}
	for _, s := range tu.GetStopTimeUpdate() {
		out.Stops = append(out.Stops, StopUpdate{
			StopID:    s.GetStopId(),
			Sequence:  int(s.GetStopSequence()),
			Skipped:   s.GetScheduleRelationship() == pb.TripUpdate_StopTimeUpdate_SKIPPED,
			Arrival:   decodeEvent(s.Arrival),
			Departure: decodeEvent(s.Departure),
		})
	}
	return out
}

func decodeEvent(e *pb.TripUpdate_StopTimeEvent) StopEvent {
	if e == nil {
		return StopEvent{}
	}
	// Delay and Time are separately optional. Reading them through the getters
	// alone would turn an absent delay into an on-time prediction.
	var out StopEvent
	if e.Delay != nil {
		out.Set = true
		out.Delay = time.Duration(e.GetDelay()) * time.Second
	}
	if e.Time != nil {
		out.Set = true
		out.Time = time.Unix(e.GetTime(), 0)
	}
	return out
}

func decodeAlert(a *pb.Alert) Alert {
	out := Alert{
		Cause:       a.GetCause().String(),
		Effect:      a.GetEffect().String(),
		Header:      translated(a.GetHeaderText()),
		Description: translated(a.GetDescriptionText()),
		URL:         translated(a.GetUrl()),
	}
	seen := map[string]bool{}
	for _, ie := range a.GetInformedEntity() {
		// The same route or stop is commonly listed several times, once per
		// direction or per affected trip.
		if r := ie.GetRouteId(); r != "" && !seen["r"+r] {
			seen["r"+r] = true
			out.Routes = append(out.Routes, r)
		}
		if s := ie.GetStopId(); s != "" && !seen["s"+s] {
			seen["s"+s] = true
			out.Stops = append(out.Stops, s)
		}
		if t := ie.GetTrip().GetTripId(); t != "" && !seen["t"+t] {
			seen["t"+t] = true
			out.Trips = append(out.Trips, t)
		}
	}
	if p := a.GetActivePeriod(); len(p) > 0 {
		if s := p[0].GetStart(); s != 0 {
			out.Start = time.Unix(int64(s), 0)
		}
		if e := p[0].GetEnd(); e != 0 {
			out.End = time.Unix(int64(e), 0)
		}
	}
	return out
}

func decodeVehicle(v *pb.VehiclePosition) VehiclePosition {
	out := VehiclePosition{
		TripID:    v.GetTrip().GetTripId(),
		RouteID:   v.GetTrip().GetRouteId(),
		VehicleID: v.GetVehicle().GetId(),
		Label:     v.GetVehicle().GetLabel(),
		StopID:    v.GetStopId(),
		Status:    v.GetCurrentStatus().String(),
		At:        time.Unix(int64(v.GetTimestamp()), 0),
	}
	if p := v.GetPosition(); p != nil {
		out.Lat, out.Lon = float64(p.GetLatitude()), float64(p.GetLongitude())
		out.Bearing, out.Speed = p.GetBearing(), p.GetSpeed()
	}
	return out
}

func translated(t *pb.TranslatedString) string {
	if t == nil {
		return ""
	}
	for _, tr := range t.GetTranslation() {
		if l := tr.GetLanguage(); l == "" || l == "en" {
			return tr.GetText()
		}
	}
	if tr := t.GetTranslation(); len(tr) > 0 {
		return tr[0].GetText()
	}
	return ""
}

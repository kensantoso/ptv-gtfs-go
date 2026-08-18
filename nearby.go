package gtfs

import (
	"context"
	"sort"
	"sync"
)

// ModeBucket collapses the feed's modes to the three a passenger names.
//
// Victoria publishes eight, distinguishing regional coaches from metro buses and
// TeleBus from both. That matters when indexing and not at all when someone is
// deciding whether to walk to the station or the tram stop.
func ModeBucket(m Mode) string {
	switch m {
	case ModeMetroTrain, ModeRegionalTrain:
		return "train"
	case ModeMetroTram:
		return "tram"
	}
	return "bus"
}

// ModeGroup holds the nearest stops of one mode, nearest first.
type ModeGroup struct {
	// Mode is a bucket name from [ModeBucket], not a feed mode.
	Mode  string
	Stops []NearbyStop
}

// NearbyRequest bounds a search around a point.
type NearbyRequest struct {
	Lat, Lon float64
	// RadiusMetres defaults to DefaultNearbyRadius.
	RadiusMetres float64
	// PerMode is how many stops of each mode to keep. Defaults to 1.
	PerMode int
	// Modes restricts which feed modes are considered. Empty means all.
	Modes []Mode
}

// DefaultNearbyRadius is far enough to reach a station from most streets and
// short enough that the answer is still somewhere you would walk.
const DefaultNearbyRadius = 1200

func (r NearbyRequest) withDefaults() NearbyRequest {
	if r.RadiusMetres <= 0 {
		r.RadiusMetres = DefaultNearbyRadius
	}
	if r.PerMode <= 0 {
		r.PerMode = 1
	}
	return r
}

// StopsNearByMode groups the closest stops by mode, so one mode cannot crowd
// out the others.
//
// A plain nearest-N is useless here: roadside bus stops outnumber stations about
// seven to one, so the nearest dozen anywhere in Melbourne is almost always
// twelve bus stops and no station. The search therefore over-fetches and then
// takes PerMode from each bucket. Groups are ordered by their closest stop, so a
// station across the road outranks a bus stop two streets away.
func (ix *Index) StopsNearByMode(ctx context.Context, req NearbyRequest) ([]ModeGroup, error) {
	req = req.withDefaults()
	// Deep enough that the buckets other than the dominant one are populated.
	const overFetch = 200
	stops, err := ix.StopsNear(ctx, req.Lat, req.Lon, req.RadiusMetres, overFetch, req.Modes...)
	if err != nil {
		return nil, err
	}
	byMode := map[string][]NearbyStop{}
	for _, s := range stops {
		b := ModeBucket(s.Mode)
		if len(byMode[b]) >= req.PerMode {
			continue
		}
		byMode[b] = append(byMode[b], s)
	}
	out := make([]ModeGroup, 0, len(byMode))
	for _, b := range []string{"train", "tram", "bus"} {
		if g := byMode[b]; len(g) > 0 {
			out = append(out, ModeGroup{Mode: b, Stops: g})
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		return out[a].Stops[0].Metres < out[b].Stops[0].Metres
	})
	return out, nil
}

// OriginPlan is a candidate departure stop and the best journey from it.
type OriginPlan struct {
	Stop NearbyStop
	// Journey is nil when nothing reaches the destination from this stop.
	Journey *Journey
}

// OriginGroup holds the candidate origins of one mode, best journey first.
type OriginGroup struct {
	Mode    string
	Origins []OriginPlan
}

// PlanFromNearby answers "which stop should I leave from".
//
// Two stops within walking distance are not interchangeable: an express station
// several hundred metres away can beat the local one underfoot once the change
// it saves is counted. Answering that means planning from each candidate rather
// than assuming the nearest is best, which is why this exists rather than being
// left to callers — every client that asks the question needs the same fan-out,
// the same ranking, and the same decision to discard a mode that reaches nothing.
//
// req.From and req.To on the plan are ignored: To comes from toStopID and From is
// filled per candidate.
func (ix *Index) PlanFromNearby(ctx context.Context, near NearbyRequest, toStopID string, plan PlanRequest) ([]OriginGroup, error) {
	groups, err := ix.StopsNearByMode(ctx, near)
	if err != nil {
		return nil, err
	}
	plan.ToStopID = toStopID
	if plan.Limit <= 0 {
		plan.Limit = 1
	}

	// Independent queries against an immutable file, so run them together: a
	// dozen candidates done one at a time is a dozen round trips of latency for
	// no reason.
	type result struct {
		gi, si int
		j      *Journey
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		res      []result
		firstErr error
	)
	for gi := range groups {
		for si := range groups[gi].Stops {
			wg.Add(1)
			go func(gi, si int) {
				defer wg.Done()
				p := plan
				p.FromStopID = groups[gi].Stops[si].ID
				js, err := ix.Plan(ctx, p)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					return
				}
				if len(js) > 0 {
					res = append(res, result{gi, si, &js[0]})
				}
			}(gi, si)
		}
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	found := map[[2]int]*Journey{}
	for _, r := range res {
		found[[2]int{r.gi, r.si}] = r.j
	}

	// A mode that reaches nothing is noise rather than an option: naming a stop
	// that goes nowhere useful invites walking to it.
	out := make([]OriginGroup, 0, len(groups))
	for gi, g := range groups {
		var keep []OriginPlan
		for si, st := range g.Stops {
			if j, ok := found[[2]int{gi, si}]; ok {
				keep = append(keep, OriginPlan{Stop: st, Journey: j})
			}
		}
		if len(keep) == 0 {
			continue
		}
		sort.SliceStable(keep, func(a, b int) bool {
			return keep[a].Journey.Arrive.Before(keep[b].Journey.Arrive)
		})
		out = append(out, OriginGroup{Mode: g.Mode, Origins: keep})
	}
	sort.SliceStable(out, func(a, b int) bool {
		return out[a].Origins[0].Journey.Arrive.Before(out[b].Origins[0].Journey.Arrive)
	})
	return out, nil
}

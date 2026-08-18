package gtfs

import "time"

// StopPair identifies a change between two stops, for [Policy.TransferTimes].
// The ids are stop ids, which for trains are platforms rather than stations.
type StopPair struct{ From, To string }

// Policy holds the judgements this package makes on a caller's behalf.
//
// Everything here is a choice rather than a fact about the data, which is why
// it is configurable. A wheelchair user needs longer at an interchange than the
// default allows; a punctuality dashboard wants nothing rounded away as "on
// time"; a caller who cannot walk wants no walking connections at all. Baked in
// as constants, none of them could disagree with me.
//
// The zero value is not usable. Start from [DefaultPolicy] and change what you
// need.
type Policy struct {
	// OnTimeThreshold is how far from schedule a service may run before it is
	// reported as early or late. The feeds give exact seconds and a train
	// twenty seconds behind is on time to any passenger.
	OnTimeThreshold time.Duration

	// DefaultTransferTime is allowed for a change where the station publishes no
	// usable path, and is the floor for one that does. Deliberately not
	// generous: an unknown station is usually a small one where the change is a
	// footbridge, and inflating it discards workable connections.
	DefaultTransferTime time.Duration

	// MaxTransferTime caps what is believed from the pathway graph. Nothing in
	// the current feed comes close: the worst platform pair measured is under six
	// minutes and none reach this cap. It is a guard rather than a working limit,
	// for a graph that grows a pathological edge or routes a change out through
	// the street.
	MaxTransferTime time.Duration

	// WalkRadius is how far, in metres, a connecting walk may be. Kept short on
	// purpose: beyond a few hundred metres a "connection" is really a separate
	// decision the traveller should make deliberately. Zero or less disables
	// walking connections.
	WalkRadius float64

	// WalkDetourFactor scales straight-line distance to allow for the route
	// being longer than the line, and WalkMetresPerSecond is the pace assumed.
	// A connection that cannot be made is worse than one that is comfortable.
	WalkDetourFactor    float64
	WalkMetresPerSecond float64

	// TransferTimes overrides how long a specific change takes, keyed by the
	// pair of stops. It is consulted before the feed, because it is the one
	// source that knows something the data cannot: a regular passenger knows
	// their own interchange better than a graph derived from segment times.
	//
	// The pathway graph is deliberately cautious — it routes through concourses
	// and counts stairs at a walking pace chosen not to strand anyone — so a
	// change a local makes in ninety seconds can be published as three minutes.
	// Left alone that discards workable connections.
	//
	// Lookup is symmetric: a time given for one direction applies to the
	// reverse unless that direction is also set.
	//
	//	Policy{TransferTimes: map[StopPair]time.Duration{
	//	    {From: "12260", To: "12255"}: 90 * time.Second, // Richmond 8 -> 3
	//	}}
	TransferTimes map[StopPair]time.Duration

	// RealtimeHorizon is roughly how far ahead the feeds describe services.
	// Beyond it there is nothing to know, which is different from everything
	// being fine. Measured against the published feed: the ninetieth percentile
	// of the last covered stop is about 57 minutes out.
	RealtimeHorizon time.Duration
}

// DefaultPolicy returns the values this package uses when a caller expresses no
// preference. They are measurements and judgements from Victoria's feed, not
// universal truths.
func DefaultPolicy() Policy {
	return Policy{
		OnTimeThreshold:     time.Minute,
		DefaultTransferTime: 90 * time.Second,
		MaxTransferTime:     8 * time.Minute,
		WalkRadius:          350,
		WalkDetourFactor:    1.35,
		WalkMetresPerSecond: 1.25,
		RealtimeHorizon:     time.Hour,
	}
}

// WalkDuration is how long to allow for a walk of the given straight-line
// distance under this policy.
func (p Policy) WalkDuration(metres float64) time.Duration {
	if p.WalkMetresPerSecond <= 0 {
		return 0
	}
	return time.Duration(metres*p.WalkDetourFactor/p.WalkMetresPerSecond) * time.Second
}

// withDefaults fills anything a caller left unset, so a partially populated
// Policy behaves sensibly rather than treating every zero as an instruction.
func (p Policy) withDefaults() Policy {
	d := DefaultPolicy()
	if p.OnTimeThreshold == 0 {
		p.OnTimeThreshold = d.OnTimeThreshold
	}
	if p.DefaultTransferTime == 0 {
		p.DefaultTransferTime = d.DefaultTransferTime
	}
	if p.MaxTransferTime == 0 {
		p.MaxTransferTime = d.MaxTransferTime
	}
	if p.WalkRadius == 0 {
		p.WalkRadius = d.WalkRadius
	}
	if p.WalkDetourFactor == 0 {
		p.WalkDetourFactor = d.WalkDetourFactor
	}
	if p.WalkMetresPerSecond == 0 {
		p.WalkMetresPerSecond = d.WalkMetresPerSecond
	}
	if p.RealtimeHorizon == 0 {
		p.RealtimeHorizon = d.RealtimeHorizon
	}
	return p
}

// Option configures an Index.
type Option func(*Index)

// WithPolicy replaces the default judgements. Fields left zero keep their
// default rather than being taken literally.
func WithPolicy(p Policy) Option {
	return func(ix *Index) { ix.policy = p.withDefaults() }
}

// transferOverride reports a caller-stated time for this change, if there is
// one. Lookup is symmetric: crossing a footbridge takes the same time whichever
// way you walk, so only one direction need be given, and an explicit reverse
// entry still wins over the implied one.
func (p Policy) transferOverride(from, to string) (time.Duration, bool) {
	if len(p.TransferTimes) == 0 {
		return 0, false
	}
	if d, ok := p.TransferTimes[StopPair{From: from, To: to}]; ok {
		return d, true
	}
	d, ok := p.TransferTimes[StopPair{From: to, To: from}]
	return d, ok
}

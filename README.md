# ptv-gtfs-go

Go library for Victoria's public transport data: the GTFS schedule feed, and the
GTFS-Realtime feeds for delays and vehicle positions.

The schedule feed needs no credential. Realtime needs a key, self-service at
[opendata.transport.vic.gov.au](https://opendata.transport.vic.gov.au).

```sh
go get github.com/kensantoso/ptv-gtfs-go
```

You supply the `*sql.DB`; the library never opens one. Queries use SQLite
syntax, so the driver should be a SQLite one — [modernc.org/sqlite] is pure Go
and needs no cgo.

## Quick start

```go
import (
    "database/sql"

    gtfs "github.com/kensantoso/ptv-gtfs-go"
    _ "modernc.org/sqlite"
)

melbourne, _ := time.LoadLocation("Australia/Melbourne")
db, _ := sql.Open("sqlite", "ptv.db")

// Once: fetch the feed and load it into the database.
gtfs.Download(ctx, "", "gtfs.zip", nil)
modes, _ := gtfs.OpenBundle("gtfs.zip", gtfs.AllModes)
gtfs.Build(ctx, db, modes, nil)

// Then query it.
ix := gtfs.Open(db, melbourne)

stops, _ := ix.FindStops(ctx, "Flinders Street", gtfs.ModeMetroTrain)
deps, _ := ix.Departures(ctx, gtfs.DeparturesRequest{StopID: stops[0].ID, Limit: 5})
```

## Building the database

The library reads GTFS from a SQLite database rather than from the CSVs, because
answering the same question repeatedly against a 12-million-row `stop_times.txt`
means rescanning it every time. Loading it once turns each lookup into an index
seek. The result is an ordinary SQLite file you can open with any client.

Three steps, deliberately separate so you can cache the download, load into any
database you like, and rebuild without refetching.

```go
gtfs.Download(ctx, url, dst, progress)      // url "" uses gtfs.DefaultFeedURL
gtfs.OpenBundle(zipPath, modes)             // unzips, keeps the files it needs
gtfs.Build(ctx, db, modeFiles, progress)    // creates the schema, loads the CSVs
```

`Build` creates the schema and loads the CSVs into the database you pass. It is
idempotent, and records `built_at`, `valid_from` and `valid_to` in a `meta`
table. Both callbacks may be nil.

### Letting store do it

Most callers want the same thing around those three steps: put the database in a
sensible place, build it when it is missing, notice when it has gone stale.
[store] is that lifecycle, so it does not have to be written again per client.

```go
import "github.com/kensantoso/ptv-gtfs-go/store"

m := &store.Manager{Dir: store.DefaultDir()}   // ~/Library/Caches/ptv-mcp, ~/.cache/ptv-mcp
db, err := m.EnsureBuilt(ctx, func(p store.Progress) {
    log.Println(p.Stage, p.Mode, p.Rows)
})
```

`Manager` also has `Status` for age and size without building, `Build` to force
a rebuild, and `Modes` to narrow what is loaded. A stale database is used rather
than rebuilt — blocking someone's first question for minutes to refresh a
schedule that is days old, not wrong, is a bad trade — so rebuild explicitly.

Unlike the rest of the package, `store` opens the file itself under the driver
name `sqlite`, so register one: `import _ "modernc.org/sqlite"`.

### Choosing modes

This is the setting that decides how big the database is. PTV ships a zip of zips,
one archive per mode, and `OpenBundle` only unpacks the ones you name — the
rest are never parsed or inserted.

```go
// Everything except Interstate and SkyBus.
modes, _ := gtfs.OpenBundle("gtfs.zip", gtfs.AllModes)

// Trains and trams only: a fraction of the size, built in a fraction of the time.
modes, _ := gtfs.OpenBundle("gtfs.zip", []gtfs.Mode{
    gtfs.ModeMetroTrain, gtfs.ModeRegionalTrain, gtfs.ModeMetroTram,
})
```

It matters more than it looks, because the modes are wildly uneven. Share of
`stop_times` rows in a recent feed:

| Mode | Constant | Rows | Share |
|---|---|---:|---:|
| Metro bus | `ModeMetroBus` | 7,496,198 | 58.8% |
| Metro tram | `ModeMetroTram` | 4,293,018 | 33.7% |
| Metro train | `ModeMetroTrain` | 642,338 | 5.0% |
| Regional train | `ModeRegionalTrain` | 153,477 | 1.2% |
| Regional coach | `ModeRegionalCoach` | 89,923 | 0.7% |
| Regional bus | `ModeRegionalBus` | 70,437 | 0.6% |

Bus and tram are 92% of the data. Loading everything gives roughly a 1.5 GB
database; trains alone is about 60 MB, trains and trams about 475 MB. Also
available: `ModeNightBus`, `ModeInterstate`, `ModeSkyBus`.

`Download` always fetches the whole bundle regardless of modes — the saving is
in build time and disk, not bandwidth.

Modes can also be narrowed per query, so loading widely and filtering later is
equally valid. Load narrow to save disk; filter per query to answer a narrower
question against everything.

## Querying

```go
ix := gtfs.Open(db, melbourne)
```

`Open` does no I/O and is safe for concurrent use. Give the pool more than one
connection or parallel queries serialise behind a single handle.

### Finding stops

Ids are not guessable, so most work starts here.

```go
stops, _ := ix.FindStops(ctx, "Flinders", gtfs.ModeMetroTrain)   // modes optional
near, _ := ix.StopsNear(ctx, -37.8183, 144.9671, 400, 10)        // metres, limit
```

`Stop` carries `ID`, `Name`, `Lat`, `Lon`, `Parent`, `Mode` and `Platform`;
`NearbyStop` adds straight-line `Metres`. Results are deduplicated to one row
per station rather than one per platform. Pass any platform id to the query
methods below and its siblings are included automatically.

### Departures

```go
deps, _ := ix.Departures(ctx, gtfs.DeparturesRequest{
    StopID: "vic:rail:FSS",
    After:  time.Now(),           // zero means now
    Within: 2 * time.Hour,
    Modes:  []gtfs.Mode{gtfs.ModeMetroTrain},
    Limit:  20,
})
```

Each `Departure` has `TripID`, `RouteID`, `Depart`, `Headsign`, `RouteName`,
`Mode`, `Platform`, and:

- `Replacement` — a bus running in place of a train. These are published on the
  train mode, so a departure can say "metro train" and be a bus at the kerb.
- `Alts` — other trip ids the feed publishes for this same physical service.
  Realtime describes only one of them, so anything joining live data must try
  these too.

### Journey planning

```go
journeys, _ := ix.Plan(ctx, gtfs.PlanRequest{
    FromStopID:   from.ID,
    ToStopID:     to.ID,
    After:        time.Now(),
    MaxTransfers: 1,
    Rank:         gtfs.RankFastest,
    Limit:        5,
})
```

Direct and one-change journeys are searched together and ranked as one list.
The options worth knowing:

| Field | Effect |
|---|---|
| `Rank` | `RankFastest` sorts by arrival; `RankShortest` by time spent travelling |
| `MaxWait` | Bounds how long the traveller will wait. Pairs with `RankShortest` |
| `MaxTransfers` | 0 for direct only, 1 for one change |
| `Modes` | Empty means every mode in the database |
| `MaxWalk` | Metres a connecting walk may cover. Negative refuses walking |
| `Via` | Only journeys changing at this station. Excludes directs |
| `TransferBuffer` | Added to the published minimum change time |
| `Within` | How far ahead to search |

`Rank` and `MaxWait` are the pair that answer a real question. `RankShortest`
alone will happily wait ninety minutes to save three; bounded by `MaxWait` it
answers "I'm free for the next half hour, what's the quickest run I can
actually catch, even if I wait for it".

A `Journey` holds `Legs`, `Depart`, `Arrive`, `Transfers`, `WaitAtTransfer`,
`TransferNeeded` (how long each change itself takes) and `Duration()`. A `Leg` carries the trip, route, endpoints, platforms,
`StopsCount`, and `Walk` with `WalkMetres` when it is made on foot. Walking
legs are always their own leg, so a walk is never silently assumed.

```go
// Expand a leg into its calls, including the stations it runs through.
calls, _ := ix.LegCalls(ctx, leg.TripID, leg.FromSeq, leg.ToSeq)
for _, c := range calls {
    if c.Skipped {
        fmt.Println(c.Name, "(runs through)")
    }
}

// How many City Loop tunnel stations a journey sits through.
n, _ := ix.CityLoopStops(ctx, journeys[0])
```

### Which stop to leave from

Two stops within walking distance are not interchangeable: an express station a
few hundred metres away can beat the local one underfoot once the change it
saves is counted. [Index.PlanFromNearby] plans from each candidate and ranks
them, rather than assuming the nearest is best.

```go
groups, _ := ix.PlanFromNearby(ctx,
    gtfs.NearbyRequest{Lat: lat, Lon: lon, PerMode: 2},
    to.ID,
    gtfs.PlanRequest{MaxTransfers: 1, Rank: gtfs.RankFastest},
)
for _, g := range groups {          // "train", "tram", "bus", best first
    for _, o := range g.Origins {
        fmt.Println(g.Mode, o.Stop.Name, o.Stop.Metres, o.Journey.Arrive)
    }
}
```

Roadside bus stops outnumber stations about seven to one, so a plain nearest-N
returns nothing but bus stops. [Index.StopsNearByMode] does the grouping alone,
without planning, and [ModeBucket] collapses the feed's eight modes to the three
a passenger names.

Modes that reach nothing are dropped: naming a stop that goes nowhere useful
invites walking to it.

### Other queries

```go
ix.Stop(ctx, stopID)                              // one stop, by id
ix.StopName(ctx, stopID)                          // just its name
ix.StopIDsForStation(ctx, stopID)                 // every platform
ix.RouteNames(ctx, gtfs.ModeMetroTram)            // route id -> name
ix.RouteIDForTrip(ctx, tripID)
ix.ServiceIDsOn(ctx, date)                        // service ids running that day
ix.TransferTime(ctx, fromStop, toStop)            // published, or walked, or default
ix.WalkTime(ctx, fromStop, toStop)                // via pathways.txt
ix.Continuation(ctx, fromTrip, toTrip, atStop)    // is it the same vehicle?
ix.Validity(ctx)                                  // the feed's timetable period
```

### Feed expiry

A GTFS feed covers a fixed period, and past its end every query matches nothing.
Rather than returning an empty result, queries outside the period fail with a
[FeedRangeError], so a caller can tell "nothing runs then" from "my data ran out
in November".

```go
if journeys, err := ix.Plan(ctx, req); err != nil {
    var fre *gtfs.FeedRangeError
    if errors.As(err, &fre) {
        // rebuild the database
    }
}
```

## Realtime

Realtime lives in its own package, documented in [realtime/](realtime/). The two
halves have opposite shapes: the schedule is a 268 MB download that changes
weekly and needs no credential, realtime is 45 KB that changes every thirty
seconds and needs a key. A client that only wants live delays should not pull in
a feed loader.

```go
import "github.com/kensantoso/ptv-gtfs-go/realtime"

rt, _ := realtime.NewClient(os.Getenv("PTV_REALTIME_KEY"))
live, _ := gtfs.FetchLive(ctx, rt, gtfs.ModeMetroTrain, gtfs.ModeMetroTram)

for _, d := range live.Disambiguate(live.Departures(deps)) {
    if d.Status.Known() {
        fmt.Println(d.Headsign, d.Status, d.Estimated.Format("15:04"))
    }
}

lj := live.Journey(journeys[0])
if lj.BrokenTransfer {
    // the connection no longer exists at current delays
}
```

`Live` also offers `Alerts`, `AlertsFor`, `Vehicle`, `Covers` and `Age`. Feeds
are network-wide: one request covers every service for a mode, so cost does not
grow with the number of stops you care about. `realtime.Client.Fetch` returns
raw protobuf if you would rather decode it yourself.

Fetching per request spends the quota on identical bytes: the gateway caches
each feed for thirty seconds and allows 24 requests a minute per mode. [live]
holds one snapshot behind that, refreshed no faster than the gateway changes it.

```go
import "github.com/kensantoso/ptv-gtfs-go/live"

c := live.New(rt, gtfs.ModeMetroTrain, gtfs.ModeMetroTram)
snap, _ := c.Get(ctx)   // a *gtfs.Live, at most live.TTL old
```

A nil `*Cache` is valid and always yields a nil snapshot, which is how a server
runs with no realtime key without guarding every use.

Note the cache is per process. Several instances behind a load balancer each
keep their own, so the effective request rate is multiplied by however many are
running.

**No data is never reported as on time.** Realtime reaches about an hour ahead
(measured: p90 of 57 minutes), so most of the schedule has no prediction at any
moment. `LiveStatus` is a tri-state whose zero value is `StatusUnknown`, and
anything that cannot make a determination says so rather than defaulting to
reassurance. Status values are machine names — `on_time`, `late`, `cancelled`,
`unknown` — because an assistant, a web page and a platform indicator all phrase
these differently, and one of them is in another language.

## Policy

Judgements live in [Policy] rather than in constants, because they are choices
rather than facts. A wheelchair user may need longer at an interchange than the
default; a punctuality dashboard wants nothing rounded away.

```go
ix := gtfs.Open(db, melbourne, gtfs.WithPolicy(gtfs.Policy{
    DefaultTransferTime: 4 * time.Minute, // more time to change
    WalkRadius:          -1,              // no walking connections
}))
```

Fields left zero keep their default rather than being read as an instruction.
Settable: `OnTimeThreshold`, `DefaultTransferTime`, `MaxTransferTime`,
`WalkRadius`, `WalkDetourFactor`, `WalkMetresPerSecond`, `RealtimeHorizon`.

### Stating your own change times

The pathway graph is cautious on purpose — it routes through concourses and
counts stairs at a pace chosen not to strand anyone — so a change a regular
passenger makes in ninety seconds can be published as three minutes, and
workable connections get discarded.

Five sources are consulted for how long a change takes, most authoritative
first:

| Source | |
|---|---|
| `PlanRequest.TransferTimes` | stated for this request |
| `Policy.TransferTimes` | stated for this process |
| `transfers.txt` | an operator minimum. Victoria publishes none today |
| `pathways.txt` | the walk graph. **This is what you normally get** |
| `Policy.DefaultTransferTime` | 90s, where the graph cannot answer |

Neither stated source is floored by the default: ninety seconds means ninety
seconds. Lookup is symmetric, so one direction covers both unless the reverse is
given too — worth doing where stairs down are not stairs up.

Per process, for a server with one set of figures:

```go
gtfs.Open(db, melbourne, gtfs.WithPolicy(gtfs.Policy{
    TransferTimes: map[gtfs.StopPair]time.Duration{
        {From: "12260", To: "12255"}: 90 * time.Second, // Richmond p8 -> p3
    },
}))
```

Per request, so one shared `Index` can answer differently for each caller — a
browser keeping its own figures, say, without an `Index` per session:

```go
ix.Plan(ctx, gtfs.PlanRequest{
    FromStopID: from, ToStopID: to,
    TransferTimes: map[gtfs.StopPair]time.Duration{...},
})
```

Ids are stop ids, which for trains are platforms rather than stations. Nobody
knows theirs, so [LoadTransferTimes] takes station names and platform numbers
and resolves them:

```go
f, _ := os.Open("transfers.json")
stated, err := gtfs.LoadTransferTimes(ctx, db, f)   // before Open: no Index yet
```

```json
{
  "Richmond":        {"8-3": "90s", "*": "2m"},
  "Flinders Street": {"2-5": "90s"}
}
```

`"*"` sets every change at that station and a specific pair overrides it. An
unknown station, an unknown platform or an unparseable duration is an error
rather than a silent no-op — an override that quietly fails to apply is worse
than one that refuses to load, because the symptom is a journey you cannot
explain months later.

## What it handles for you

The parts of GTFS that are easy to get wrong and then trust. Each of these
produced a confident, wrong answer during manual analysis before this existed.

| | |
|---|---|
| **Direction** | A trip calling at both A and B says nothing about which way it travels. Only `stop_sequence` does |
| **Service days** | `calendar.txt` for the pattern, `calendar_dates.txt` for exceptions, which both add and remove. A service can exist only as an exception |
| **After-midnight times** | 1:30am on Friday's timetable is `25:30:00`, not `01:30:00`. Read as a wall clock, every late-night service vanishes |
| **Stations vs platforms** | Trains depart platforms, not stations. Querying one stop id silently misses most services |
| **Interchanges** | Matched by station, not platform — you arrive on one and leave from another. Comparing stop ids finds no connection at exactly the stations where changing is most useful |
| **Duplicate publication** | One Saturday train appears as three trip ids under different stopping patterns, with more than one calendar active. Left alone it returns the same train five times as five options |
| **Walking connections** | The feed has none across modes: a bus setting down 66 m from a station shares no edge with it. Edges are synthesised from coordinates and surfaced as their own leg |
| **Change times** | `transfers.txt` where the operator states a minimum, `pathways.txt` walked as a graph where it does not, a configured default otherwise — asked in that order, so a feed that starts publishing real minimums is picked up without a code change |
| **Through-running** | `transfers.txt` type 4 pairs an arriving trip with the departing trip that is the same vehicle. Presented as a change, it is in fact the friendliest kind: there is nothing to do |
| **Replacement buses** | 3,313 trips are buses published on the train mode, marked by an `-R` route id suffix. Sending someone to a platform when the service leaves from the forecourt strands them |
| **Loop counting** | `CityLoopStops` counts the tunnel stations a journey sits through. Southern Cross and Flinders Street are deliberately not counted: the tunnel is the optional part, and 5,929 of 12,154 trips calling at Southern Cross never enter it |
| **Feed expiry** | Queries past the timetable period return an error rather than an empty result |

## Data

> Source: Licensed from Public Transport Victoria under a Creative Commons
> Attribution 4.0 International Licence.

Unofficial. Not affiliated with or endorsed by Public Transport Victoria.

## Licence

MIT

[modernc.org/sqlite]: https://pkg.go.dev/modernc.org/sqlite
[store]: https://pkg.go.dev/github.com/kensantoso/ptv-gtfs-go/store
[live]: https://pkg.go.dev/github.com/kensantoso/ptv-gtfs-go/live

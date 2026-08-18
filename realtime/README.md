# realtime

Victoria's GTFS-Realtime feeds: what is happening on the network right now.

```go
import (
    "context"
    "os"

    "github.com/kensantoso/ptv-gtfs-go/realtime"
)

c, err := realtime.NewClient(os.Getenv("PTV_REALTIME_KEY"))
if err != nil {
    return err // the key is required; NewClient says so rather than failing later
}

body, err := c.Fetch(ctx, realtime.MetroTrainTripUpdates)
if err != nil {
    return err
}
data, err := realtime.Decode(body)
if err != nil {
    return err
}

for _, tu := range data.TripUpdates {
    // tu.TripID joins to the static feed. tu.RouteID does not, on the bus feed.
    for _, s := range tu.Stops {
        if s.Departure.Set {
            // s.Departure.Delay is signed; negative is early.
            // Check Set rather than reading Delay, or an absent prediction
            // becomes an on-time one.
        }
    }
}
```

A key is required, and is self-service at
[opendata.transport.vic.gov.au](https://opendata.transport.vic.gov.au). The
schedule half of this module needs no credential.

## Why it is a separate package

The two halves of GTFS have opposite shapes.

| | schedule | realtime |
|---|---|---|
| size | 268 MB download, 63 MB indexed | 45 KB per fetch |
| changes | weekly, and expires | every 30 seconds |
| credential | none | a key |
| storage | a SQLite index | nothing |

A client that only wants live delays should not pull in an indexer, and a server
that only serves timetables should not pull in protobuf. Importing this package
alone links neither SQLite nor the journey planner: a realtime-only binary comes
out around 10 MB with zero SQLite symbols in it.

The join between the two — applying delays to a planned journey — lives in the
parent package, because it needs both halves and belongs to neither.

## What the feeds are, and are not

**Protobuf, not CSV.** Decoding needs the published bindings, which is the one
dependency here.

**A snapshot, not a file.** Every fetch is the complete current state, marked
`FULL_DATASET`; there is nothing to merge with what came before. Polling the same
URL thirty seconds apart returns different bytes.

**Network-wide.** One request covers every service for a mode, so cost does not
grow with the number of stops you care about. Asking about one stop and asking
about five hundred are the same request.

**Self-dating.** Every message carries the publisher's timestamp. Staleness is
stated by the feed rather than guessed from when you fetched it.

## Things the format will let you get wrong

**A delay of zero is a prediction.** `Delay` and `Time` are separately optional,
and the generated getters return `0` for both "on time" and "not set". Reading
them through `GetDelay()` turns an absent prediction into an on-time one. Use
`StopEvent.Set`, which this package populates by checking presence.

**Trip ids repeat daily.** A snapshot taken today matches tomorrow's timetable by
id alone. Check `TripUpdate.StartDate` before applying anything, or predictions
land a day out.

**Route ids do not join reliably.** `TripID` matches the static feed exactly.
`RouteID` matches for train and tram but not for bus, where realtime uses a
different scheme entirely. Resolve the route from the trip.

**Cause and effect are usually useless.** They read `OTHER_CAUSE` and
`OTHER_EFFECT` on real incidents, and `Header` is often a bare category such as
"Minor Delay" or "PlannedOccupation". The substance is in `Description`.

**One incident arrives several times**, once per affected route. Merging them is
the parent package's `Live.Alerts`.

## Authentication

A `KeyID` header. Note this contradicts the published OpenAPI documents, which
declare `Ocp-Apim-Subscription-Key`; that header returns 401 against the live
gateway. Verified against it, not assumed.

The gateway caches each feed for 30 seconds and allows 24 requests per minute per
mode, so polling faster than every 30 seconds returns identical bytes and spends
quota.

## Coverage

Trip updates and service alerts are published for metro train, tram and bus.
Vehicle positions exist for train and bus; the tram vehicle feed has been
observed returning HTTP 500 for extended periods. Regional trains, coaches and
Skybus are in the schedule but have no realtime publication at all — treat a
missing feed as unknown rather than as nothing wrong.

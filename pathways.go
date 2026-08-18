package gtfs

import (
	"container/heap"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Walking times inside stations come from pathways.txt, not transfers.txt.
//
// Victoria's transfers.txt is not what its name suggests. Every one of its rows
// has from_stop equal to to_stop, transfer_type 4 and a minimum time of zero:
// they are in-seat continuations, recording that one trip becomes another at a
// platform so a passenger can stay aboard. They say nothing about the time it
// takes to walk between platforms, and a planner that reads them for that
// purpose learns nothing and quietly falls back to a guess.
//
// pathways.txt does describe the walk. It is a graph of platforms, concourses,
// stairs, lifts and entrances with a traversal time on each segment, covering
// 224 stations. Shortest path across it turns "change at Richmond" into a number
// that reflects the actual station: two platforms of one island are a minute
// apart, the mean across its ten platforms is under three minutes, and the
// furthest pair is four.

type pathEdge struct {
	to  string
	sec int
}

type pathGraph struct {
	once sync.Once
	adj  map[string][]pathEdge
	err  error
}

// walkGraph returns the station pathway graph, loading it on first use.
func (ix *Index) walkGraph(ctx context.Context) (map[string][]pathEdge, error) {
	ix.paths.once.Do(func() {
		adj := make(map[string][]pathEdge)
		rows, err := ix.db.QueryContext(ctx,
			`SELECT from_stop, to_stop, bidir, seconds FROM pathway`)
		if err != nil {
			ix.paths.err = fmt.Errorf("gtfs: pathways: %w", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var from, to string
			var bidir, sec int
			if err := rows.Scan(&from, &to, &bidir, &sec); err != nil {
				ix.paths.err = err
				return
			}
			adj[from] = append(adj[from], pathEdge{to, sec})
			if bidir != 0 {
				adj[to] = append(adj[to], pathEdge{from, sec})
			}
		}
		ix.paths.err = rows.Err()
		ix.paths.adj = adj
	})
	return ix.paths.adj, ix.paths.err
}

// WalkTime returns how long it takes to walk between two stops inside a station.
//
// The second result reports whether the pathway graph could answer at all.
// Callers should treat a false as "unknown" and apply their own default rather
// than as "no time required".
func (ix *Index) WalkTime(ctx context.Context, from, to string) (time.Duration, bool, error) {
	if from == to {
		return 0, true, nil
	}
	adj, err := ix.walkGraph(ctx)
	if err != nil {
		return 0, false, err
	}
	if len(adj) == 0 {
		return 0, false, nil
	}

	// Dijkstra. The graph is small and the search is bounded by MaxTransferTime,
	// so this stays local to the station rather than wandering the network.
	const unreachable = 1 << 30
	dist := map[string]int{from: 0}
	pq := &nodeHeap{{node: from, cost: 0}}
	limit := int(ix.policy.MaxTransferTime / time.Second)

	for pq.Len() > 0 {
		cur := heap.Pop(pq).(nodeCost)
		if cur.node == to {
			return time.Duration(cur.cost) * time.Second, true, nil
		}
		if cur.cost > dist[cur.node] || cur.cost > limit {
			continue
		}
		for _, e := range adj[cur.node] {
			next := cur.cost + e.sec
			if next > limit {
				continue
			}
			if d, ok := dist[e.to]; !ok || next < d {
				dist[e.to] = next
				heap.Push(pq, nodeCost{node: e.to, cost: next})
			}
		}
	}
	return 0, false, nil
}

// TransferTime is the time to allow for a change, from the platform of arrival
// to the platform of departure.
//
// Four sources, in order of authority:
//
//  0. [Policy.TransferTimes], where the caller has stated a time for this
//     change. Nothing else knows the station as well as someone who uses it.
//  1. transfers.txt, where the operator states a minimum. That is their own
//     figure for their own station and beats anything derived.
//  2. pathways.txt, walked as a graph. Measured, but only a distance.
//  3. the policy default, which is a guess.
//
// The order matters for more than tidiness. Victoria currently publishes
// transfers.txt as in-seat continuations with no times, so today every answer
// comes from (2) or (3). If they ever populate it properly, this picks the
// better data up without a code change — which is the point of asking in this
// order rather than hardcoding the source that happens to be useful now.
//
// A published time is taken as given, not floored: the operator saying ninety
// seconds is not improved by my opinion. The floor applies only to a distance
// we derived, where a path of a few seconds describes two points on one
// concourse rather than a change a passenger makes with luggage and a crowd.
func (ix *Index) TransferTime(ctx context.Context, from, to string) (time.Duration, error) {
	// A time the caller stated for this change wins outright. It is not floored
	// by the default: someone saying ninety seconds has measured it.
	if d, ok := ix.policy.transferOverride(from, to); ok {
		return d, nil
	}
	if d, ok, err := ix.publishedTransfer(ctx, from, to); err != nil {
		return ix.policy.DefaultTransferTime, err
	} else if ok {
		return d, nil
	}

	d, ok, err := ix.WalkTime(ctx, from, to)
	if err != nil {
		return ix.policy.DefaultTransferTime, err
	}
	if !ok || d < ix.policy.DefaultTransferTime {
		return ix.policy.DefaultTransferTime, nil
	}
	return d, nil
}

// publishedTransfer reads an operator-stated minimum from transfers.txt.
//
// Only types 0, 1 and 2 carry a meaningful time. Type 3 is "transfer not
// possible", reported as a duration long enough that no connection survives the
// check. Types 4 and 5 are in-seat continuations and say nothing about walking:
// a blank time there means you do not move, not that the change is free.
func (ix *Index) publishedTransfer(ctx context.Context, from, to string) (time.Duration, bool, error) {
	var typ, secs sql.NullInt64
	err := ix.db.QueryRowContext(ctx,
		`SELECT type, min_time FROM transfer
		 WHERE from_stop=? AND to_stop=? AND type IN (0,1,2,3)
		 ORDER BY CASE WHEN type=3 THEN 0 ELSE 1 END, min_time DESC
		 LIMIT 1`, from, to).Scan(&typ, &secs)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("gtfs: transfers: %w", err)
	}
	if typ.Int64 == 3 {
		return ix.policy.MaxTransferTime * 10, true, nil // effectively impossible
	}
	if !secs.Valid || secs.Int64 <= 0 {
		return 0, false, nil
	}
	return time.Duration(secs.Int64) * time.Second, true, nil
}

// Continuation reports whether one trip becomes another at a stop, so a
// passenger stays in their seat.
//
// This is what transfers.txt actually carries in Victoria's feed: type 4 rows
// pairing an arriving trip with the departing trip that is the same vehicle.
// Melbourne through-runs services, so a Mernda train reaches Flinders Street
// and leaves as a Hurstbridge one. Presented as a change it is the least
// friendly kind of connection; it is in fact the friendliest, because there is
// nothing to do.
func (ix *Index) Continuation(ctx context.Context, fromTrip, toTrip, atStop string) (bool, error) {
	var n int
	err := ix.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM transfer
		 WHERE type IN (4,5) AND from_trip=? AND to_trip=? AND from_stop=?`,
		fromTrip, toTrip, atStop).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("gtfs: continuation: %w", err)
	}
	return n > 0, nil
}

type nodeCost struct {
	node string
	cost int
}

type nodeHeap []nodeCost

func (h nodeHeap) Len() int           { return len(h) }
func (h nodeHeap) Less(i, j int) bool { return h[i].cost < h[j].cost }
func (h nodeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *nodeHeap) Push(x any)        { *h = append(*h, x.(nodeCost)) }
func (h *nodeHeap) Pop() any          { o := *h; n := len(o); x := o[n-1]; *h = o[:n-1]; return x }

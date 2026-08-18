package gtfs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// TransferTimesFile is the shape of a transfer-time override file.
//
// Keys are station names; inner keys are platform pairs like "8-3", or "*" for
// every change at that station. Values are Go durations: "90s", "2m".
//
//	{
//	  "Richmond":        {"8-3": "90s", "*": "2m"},
//	  "Flinders Street": {"2-5": "90s"}
//	}
//
// Platform numbers rather than stop ids, because ids are opaque and nobody
// knows theirs. A specific pair beats the station-wide "*".
type TransferTimesFile map[string]map[string]string

// LoadTransferTimes reads override durations and resolves them to stop ids for
// [Policy.TransferTimes].
//
// Takes a *sql.DB rather than an [Index] because the result is needed to build
// the [Policy] that Open is given, so no Index exists yet.
//
// Unknown stations and platforms are an error rather than a silent no-op: an
// override that quietly fails to apply is worse than one that refuses to load,
// because the symptom is a journey you cannot explain months later.
func LoadTransferTimes(ctx context.Context, db *sql.DB, r io.Reader) (map[StopPair]time.Duration, error) {
	var f TransferTimesFile
	if err := json.NewDecoder(r).Decode(&f); err != nil {
		return nil, fmt.Errorf("gtfs: transfer times: %w", err)
	}
	out := map[StopPair]time.Duration{}
	for station, pairs := range f {
		plats, err := platformsOf(ctx, db, station)
		if err != nil {
			return nil, err
		}
		// "*" first, so an explicit pair written alongside it wins.
		if spec, ok := pairs["*"]; ok {
			d, err := time.ParseDuration(spec)
			if err != nil {
				return nil, fmt.Errorf("gtfs: transfer times: %s \"*\": %w", station, err)
			}
			for a, ida := range plats {
				for b, idb := range plats {
					if a != b {
						out[StopPair{From: ida, To: idb}] = d
					}
				}
			}
		}
		for pair, spec := range pairs {
			if pair == "*" {
				continue
			}
			a, b, ok := strings.Cut(pair, "-")
			if !ok {
				return nil, fmt.Errorf("gtfs: transfer times: %s %q: want a platform pair like \"8-3\"", station, pair)
			}
			ida, oka := plats[strings.TrimSpace(a)]
			idb, okb := plats[strings.TrimSpace(b)]
			if !oka || !okb {
				return nil, fmt.Errorf("gtfs: transfer times: %s has no platform %q or %q", station, a, b)
			}
			d, err := time.ParseDuration(spec)
			if err != nil {
				return nil, fmt.Errorf("gtfs: transfer times: %s %q: %w", station, pair, err)
			}
			out[StopPair{From: ida, To: idb}] = d
		}
	}
	return out, nil
}

// platformsOf maps a station's platform numbers to their stop ids.
func platformsOf(ctx context.Context, db *sql.DB, station string) (map[string]string, error) {
	// Exact name first: "Richmond" must not resolve to East Richmond because
	// that row happens to sort earlier.
	var id string
	err := db.QueryRowContext(ctx, `
		SELECT stop_id FROM stop
		WHERE (name = ? COLLATE NOCASE OR name LIKE ? COLLATE NOCASE)
		  AND COALESCE(parent,'') = ''
		ORDER BY CASE WHEN name = ? COLLATE NOCASE THEN 0 ELSE 1 END, LENGTH(name)
		LIMIT 1`, station, station+"%", station).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("gtfs: transfer times: no station named %q", station)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT platform, stop_id FROM stop WHERE parent = ? AND COALESCE(platform,'') != ''`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, s string
		if err := rows.Scan(&p, &s); err != nil {
			return nil, err
		}
		out[p] = s
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("gtfs: transfer times: %q has no numbered platforms", station)
	}
	return out, rows.Err()
}

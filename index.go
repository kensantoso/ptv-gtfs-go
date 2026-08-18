package gtfs

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Schema is the database layout.
//
// SQLite rather than in-memory structures: the retained feed is about 1GB of
// CSV across all modes, which is too much to hold, and an MCP server starts and
// stops with its client so it cannot spend that long parsing on every launch.
// Built once, queried in microseconds thereafter.
//
// Times are stored as seconds after midnight of the service day, never as
// timestamps. A service departing at 25:30 belongs to the previous day's
// timetable, and converting to an instant during indexing loses that.
const Schema = `
CREATE TABLE IF NOT EXISTS stop (
  stop_id    TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  lat        REAL,
  lon        REAL,
  parent     TEXT,          -- parent_station; platforms share one
  mode       INTEGER NOT NULL,
  platform   TEXT
);
CREATE INDEX IF NOT EXISTS stop_name_idx   ON stop(name);
CREATE INDEX IF NOT EXISTS stop_parent_idx ON stop(parent);
CREATE INDEX IF NOT EXISTS stop_geo_idx    ON stop(lat, lon);

CREATE TABLE IF NOT EXISTS route (
  route_id   TEXT PRIMARY KEY,
  short_name TEXT,
  long_name  TEXT,
  mode       INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS trip (
  trip_id    TEXT PRIMARY KEY,
  route_id   TEXT NOT NULL,
  service_id TEXT NOT NULL,
  headsign   TEXT,
  direction  INTEGER,
  mode       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS trip_service_idx ON trip(service_id);
CREATE INDEX IF NOT EXISTS trip_route_idx   ON trip(route_id);

-- One row per call. seq is stop_sequence and is the only thing that says which
-- way a trip is travelling: a trip calling at both A and B says nothing about
-- direction until you compare their sequence numbers.
CREATE TABLE IF NOT EXISTS stop_time (
  trip_id  TEXT NOT NULL,
  stop_id  TEXT NOT NULL,
  seq      INTEGER NOT NULL,
  arr      INTEGER NOT NULL,   -- seconds after service-day midnight
  dep      INTEGER NOT NULL,
  PRIMARY KEY (trip_id, seq)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS st_stop_dep_idx ON stop_time(stop_id, dep);
CREATE INDEX IF NOT EXISTS st_trip_idx     ON stop_time(trip_id, seq);

-- Official minimum transfer times, so a connection is judged on published data
-- rather than a guessed buffer.
CREATE TABLE IF NOT EXISTS transfer (
  from_trip  TEXT,
  to_trip    TEXT,
  from_route TEXT,
  to_route   TEXT,
  from_stop TEXT NOT NULL,
  to_stop   TEXT NOT NULL,
  type      INTEGER,
  min_time  INTEGER
);
CREATE INDEX IF NOT EXISTS transfer_from_idx ON transfer(from_stop);

-- Walking segments inside a station, in seconds. This is what makes a
-- "minimise walking" answer honest rather than straight-line.
CREATE TABLE IF NOT EXISTS pathway (
  from_stop TEXT NOT NULL,
  to_stop   TEXT NOT NULL,
  bidir     INTEGER,
  seconds   INTEGER
);
CREATE INDEX IF NOT EXISTS pathway_from_idx ON pathway(from_stop);

CREATE TABLE IF NOT EXISTS calendar (
  service_id TEXT NOT NULL,
  dow        INTEGER NOT NULL,  -- 0=Sunday, matching time.Weekday
  start_date TEXT NOT NULL,
  end_date   TEXT NOT NULL,
  PRIMARY KEY (service_id, dow)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS calendar_date (
  service_id TEXT NOT NULL,
  date       TEXT NOT NULL,
  added      INTEGER NOT NULL,  -- 1 = added, 0 = removed
  PRIMARY KEY (service_id, date)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT
);
`

// BuildProgress reports indexing progress. Building takes minutes on the full
// bundle, which is long enough that silence reads as a hang.
type BuildProgress struct {
	Mode  Mode
	File  string
	Rows  int
	Total int
}

// Build creates the schema and loads extracted mode files into db.
//
// Callers pass an open *sql.DB so the driver stays their choice: this package
// deliberately does not import one, keeping it dependency-free and letting the
// caller pick cgo or pure-Go SQLite.
func Build(ctx context.Context, db *sql.DB, modes []ModeFiles, progress func(BuildProgress)) error {
	if _, err := db.ExecContext(ctx, Schema); err != nil {
		return fmt.Errorf("gtfs: schema: %w", err)
	}
	// Bulk load settings. Durability is irrelevant for derived data that can
	// be rebuilt from the feed at any time, and these make it several times
	// faster.
	for _, p := range []string{
		"PRAGMA journal_mode=OFF", "PRAGMA synchronous=OFF",
		"PRAGMA cache_size=-200000", "PRAGMA temp_store=MEMORY",
	} {
		if _, err := db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("gtfs: %s: %w", p, err)
		}
	}

	for _, mf := range modes {
		if err := buildMode(ctx, db, mf, progress); err != nil {
			return fmt.Errorf("gtfs: mode %s: %w", mf.Mode, err)
		}
	}

	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key,value) VALUES('built_at',?)`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}

	// Record the period this feed actually describes, so a query past the end
	// can say so instead of returning nothing. When the build ran is a different
	// fact from when the timetable stops being true, and only the second one
	// determines whether an answer is worth anything.
	//
	// Exception dates count: a service can be added on a date outside every
	// calendar row's range, and the feed is valid for it.
	_, err := db.ExecContext(ctx, `
		INSERT OR REPLACE INTO meta(key,value)
		SELECT 'valid_from', MIN(d) FROM (
		  SELECT MIN(start_date) AS d FROM calendar
		  UNION ALL SELECT MIN(date) FROM calendar_date WHERE added=1
		) WHERE d IS NOT NULL`)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
		INSERT OR REPLACE INTO meta(key,value)
		SELECT 'valid_to', MAX(d) FROM (
		  SELECT MAX(end_date) AS d FROM calendar
		  UNION ALL SELECT MAX(date) FROM calendar_date WHERE added=1
		) WHERE d IS NOT NULL`)
	return err
}

func buildMode(ctx context.Context, db *sql.DB, mf ModeFiles, progress func(BuildProgress)) error {
	m := int(mf.Mode)

	type loader struct {
		file string
		stmt string
		// row maps a CSV record to statement arguments. Returning nil skips it.
		row func(map[string]string) []any
	}

	loaders := []loader{
		{"stops.txt",
			`INSERT OR REPLACE INTO stop(stop_id,name,lat,lon,parent,mode,platform) VALUES(?,?,?,?,?,?,?)`,
			func(r map[string]string) []any {
				return []any{r["stop_id"], r["stop_name"], num(r["stop_lat"]), num(r["stop_lon"]),
					r["parent_station"], m, r["platform_code"]}
			}},
		{"routes.txt",
			`INSERT OR REPLACE INTO route(route_id,short_name,long_name,mode) VALUES(?,?,?,?)`,
			func(r map[string]string) []any {
				return []any{r["route_id"], r["route_short_name"], r["route_long_name"], m}
			}},
		{"trips.txt",
			`INSERT OR REPLACE INTO trip(trip_id,route_id,service_id,headsign,direction,mode) VALUES(?,?,?,?,?,?)`,
			func(r map[string]string) []any {
				return []any{r["trip_id"], r["route_id"], r["service_id"],
					r["trip_headsign"], intOr(r["direction_id"], -1), m}
			}},
		{"stop_times.txt",
			`INSERT OR REPLACE INTO stop_time(trip_id,stop_id,seq,arr,dep) VALUES(?,?,?,?,?)`,
			func(r map[string]string) []any {
				arr, err1 := ParseGTFSTime(r["arrival_time"])
				dep, err2 := ParseGTFSTime(r["departure_time"])
				if err1 != nil || err2 != nil {
					// Times are optional for non-timepoint stops. Such a row
					// cannot answer a departure question, so it is dropped
					// rather than stored as a misleading zero.
					return nil
				}
				return []any{r["trip_id"], r["stop_id"], intOr(r["stop_sequence"], 0), arr, dep}
			}},
		{"transfers.txt",
			`INSERT INTO transfer(from_stop,to_stop,from_trip,to_trip,from_route,to_route,type,min_time)
			 VALUES(?,?,?,?,?,?,?,?)`,
			func(r map[string]string) []any {
				return []any{r["from_stop_id"], r["to_stop_id"],
					r["from_trip_id"], r["to_trip_id"], r["from_route_id"], r["to_route_id"],
					intOr(r["transfer_type"], 0), intOr(r["min_transfer_time"], 0)}
			}},
		{"pathways.txt",
			`INSERT INTO pathway(from_stop,to_stop,bidir,seconds) VALUES(?,?,?,?)`,
			func(r map[string]string) []any {
				return []any{r["from_stop_id"], r["to_stop_id"],
					intOr(r["is_bidirectional"], 0), intOr(r["traversal_time"], 0)}
			}},
		{"calendar_dates.txt",
			`INSERT OR REPLACE INTO calendar_date(service_id,date,added) VALUES(?,?,?)`,
			func(r map[string]string) []any {
				added := 0
				if r["exception_type"] == "1" {
					added = 1
				}
				return []any{r["service_id"], r["date"], added}
			}},
	}

	for _, l := range loaders {
		data, ok := mf.Files[l.file]
		if !ok {
			continue
		}
		if err := loadCSV(ctx, db, data, l.stmt, l.row, func(n int) {
			if progress != nil {
				progress(BuildProgress{Mode: mf.Mode, File: l.file, Rows: n})
			}
		}); err != nil {
			return fmt.Errorf("%s: %w", l.file, err)
		}
	}

	// calendar.txt is denormalised to one row per active weekday, so a day
	// lookup is an equality match rather than seven column tests.
	if data, ok := mf.Files["calendar.txt"]; ok {
		if err := loadCalendar(ctx, db, data); err != nil {
			return fmt.Errorf("calendar.txt: %w", err)
		}
	}
	return nil
}

// loadCSV streams one file into the database inside a single transaction.
//
// Streaming matters: stop_times.txt for bus alone is ~500MB, and reading it
// into a slice of maps first would multiply that several times over.
func loadCSV(ctx context.Context, db *sql.DB, data []byte, stmt string,
	row func(map[string]string) []any, progress func(int)) error {

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	ps, err := tx.PrepareContext(ctx, stmt)
	if err != nil {
		return err
	}
	defer ps.Close()

	cr := csv.NewReader(strings.NewReader(string(data)))
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true

	head, err := cr.Read()
	if err != nil {
		return err
	}
	cols := make([]string, len(head))
	for i, h := range head {
		cols[i] = strings.TrimPrefix(strings.TrimSpace(h), "\ufeff")
	}

	rec := map[string]string{}
	n := 0
	for {
		fields, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		for k := range rec {
			delete(rec, k)
		}
		for i, c := range cols {
			if i < len(fields) {
				rec[c] = fields[i]
			}
		}
		args := row(rec)
		if args == nil {
			continue
		}
		if _, err := ps.ExecContext(ctx, args...); err != nil {
			return err
		}
		n++
		if progress != nil && n%250000 == 0 {
			progress(n)
		}
	}
	if progress != nil {
		progress(n)
	}
	return tx.Commit()
}

func loadCalendar(ctx context.Context, db *sql.DB, data []byte) error {
	rows, err := readCSV(strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	ps, err := tx.PrepareContext(ctx,
		`INSERT OR REPLACE INTO calendar(service_id,dow,start_date,end_date) VALUES(?,?,?,?)`)
	if err != nil {
		return err
	}
	defer ps.Close()

	cols := []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
	for _, r := range rows {
		for dow, c := range cols {
			if r[c] != "1" {
				continue
			}
			if _, err := ps.ExecContext(ctx, r["service_id"], dow, r["start_date"], r["end_date"]); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func num(s string) any {
	if s == "" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return f
}

func intOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// Package gtfs loads a GTFS static feed into a SQLite database and answers
// timetable questions against it.
//
// The fiddly parts of GTFS are all here, deliberately, because they are the
// parts that are easy to get subtly wrong and then trust:
//
//   - Service days. Whether a trip runs on a given date depends on calendar.txt
//     for the regular pattern and calendar_dates.txt for exceptions, and the
//     exceptions both add and remove.
//   - Direction. A trip that calls at two stations says nothing about which way
//     it is going. Only stop_sequence does.
//   - After-midnight times. GTFS expresses 1:30am on Friday's timetable as
//     25:30:00 on Friday, not 01:30:00 on Saturday.
//
// Each of those produced a plausible, confident, wrong answer during manual
// analysis before this package existed.
package gtfs

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// ServiceDays answers whether a service_id runs on a given date.
type ServiceDays struct {
	// regular[serviceID] holds the weekday pattern and date range.
	regular map[string]regularService
	// added and removed hold calendar_dates.txt exceptions, keyed by
	// "serviceID|YYYYMMDD" so a lookup is one map hit.
	added   map[string]bool
	removed map[string]bool
}

type regularService struct {
	days  [7]bool // Sunday..Saturday, matching time.Weekday
	start string  // YYYYMMDD inclusive
	end   string  // YYYYMMDD inclusive
}

// LoadServiceDays reads calendar.txt and calendar_dates.txt. Either may be
// absent: a feed can express its whole schedule through exceptions alone, and
// some publishers do.
func LoadServiceDays(calendar, calendarDates io.Reader) (*ServiceDays, error) {
	s := &ServiceDays{
		regular: map[string]regularService{},
		added:   map[string]bool{},
		removed: map[string]bool{},
	}

	if calendar != nil {
		rows, err := readCSV(calendar)
		if err != nil {
			return nil, fmt.Errorf("calendar.txt: %w", err)
		}
		// Column order follows time.Weekday: Sunday is 0.
		cols := []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
		for _, r := range rows {
			var rs regularService
			for i, c := range cols {
				rs.days[i] = r[c] == "1"
			}
			rs.start, rs.end = r["start_date"], r["end_date"]
			s.regular[r["service_id"]] = rs
		}
	}

	if calendarDates != nil {
		rows, err := readCSV(calendarDates)
		if err != nil {
			return nil, fmt.Errorf("calendar_dates.txt: %w", err)
		}
		for _, r := range rows {
			k := r["service_id"] + "|" + r["date"]
			switch r["exception_type"] {
			case "1":
				s.added[k] = true
			case "2":
				s.removed[k] = true
			}
		}
	}
	return s, nil
}

// Runs reports whether serviceID operates on the given date.
//
// Order matters: a removal in calendar_dates.txt overrides the regular pattern,
// and an addition applies even when calendar.txt has no entry at all.
func (s *ServiceDays) Runs(serviceID string, date time.Time) bool {
	k := serviceID + "|" + date.Format("20060102")
	if s.removed[k] {
		return false
	}
	if s.added[k] {
		return true
	}
	rs, ok := s.regular[serviceID]
	if !ok {
		return false
	}
	if !rs.days[int(date.Weekday())] {
		return false
	}
	ymd := date.Format("20060102")
	return rs.start <= ymd && ymd <= rs.end
}

// ServiceIDs returns every service running on a date. Callers filtering a large
// stop_times file want this once up front rather than calling Runs per row.
func (s *ServiceDays) ServiceIDs(date time.Time) map[string]bool {
	out := map[string]bool{}
	for id := range s.regular {
		if s.Runs(id, date) {
			out[id] = true
		}
	}
	// Additions may name a service with no calendar.txt row at all.
	ymd := date.Format("20060102")
	for k := range s.added {
		if id, d, ok := splitKey(k); ok && d == ymd && !s.removed[k] {
			out[id] = true
		}
	}
	return out
}

func splitKey(k string) (id, date string, ok bool) {
	i := strings.LastIndex(k, "|")
	if i < 0 {
		return "", "", false
	}
	return k[:i], k[i+1:], true
}

// ParseGTFSTime converts a GTFS time to seconds after midnight of the service
// day. Values at or beyond 24:00:00 are normal and meaningful: a service that
// departs at 25:30 belongs to the previous day's timetable, which is how a
// Friday-night train at 1:30am is expressed.
//
// Returning seconds rather than a time.Time is deliberate. The value is an
// offset within a service day, not an instant, and converting too early is how
// after-midnight trips get silently dropped or shifted a day.
func ParseGTFSTime(s string) (int, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("gtfs: bad time %q", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("gtfs: bad hour in %q", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("gtfs: bad minute in %q", s)
	}
	sec, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, fmt.Errorf("gtfs: bad second in %q", s)
	}
	return h*3600 + m*60 + sec, nil
}

// FormatGTFSTime renders seconds-after-midnight as clock time, wrapping past
// 24h so 25:30 reads as 01:30.
func FormatGTFSTime(secs int) string {
	return fmt.Sprintf("%02d:%02d", (secs/3600)%24, (secs%3600)/60)
}

// Instant converts an offset within a service day to a real instant.
func Instant(serviceDay time.Time, secs int, loc *time.Location) time.Time {
	y, m, d := serviceDay.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc).Add(time.Duration(secs) * time.Second)
}

// readCSV reads a whole GTFS CSV into maps. GTFS files are UTF-8 with a BOM
// often present on the first header, which would otherwise corrupt the first
// column name and make every lookup of it fail silently.
func readCSV(r io.Reader) ([]map[string]string, error) {
	cr := csv.NewReader(r)
	cr.ReuseRecord = false
	cr.FieldsPerRecord = -1 // some publishers pad rows unevenly

	head, err := cr.Read()
	if err != nil {
		return nil, err
	}
	for i := range head {
		head[i] = strings.TrimPrefix(strings.TrimSpace(head[i]), "\ufeff")
	}

	var out []map[string]string
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		m := make(map[string]string, len(head))
		for i, h := range head {
			if i < len(rec) {
				m[h] = rec[i]
			}
		}
		out = append(out, m)
	}
	return out, nil
}

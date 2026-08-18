package gtfs

import (
	"strings"
	"testing"
	"time"
)

// Monday 17 Aug 2026 through Sunday 23 Aug 2026.
func date(day int) time.Time {
	return time.Date(2026, 8, day, 0, 0, 0, 0, time.UTC)
}

const calendarCSV = `service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date
WEEKDAY,1,1,1,1,1,0,0,20260101,20261231
WEEKEND,0,0,0,0,0,1,1,20260101,20261231
EXPIRED,1,1,1,1,1,1,1,20250101,20250601
`

const calendarDatesCSV = `service_id,date,exception_type
WEEKDAY,20260818,2
WEEKEND,20260819,1
SPECIAL,20260820,1
`

func load(t *testing.T) *ServiceDays {
	t.Helper()
	s, err := LoadServiceDays(strings.NewReader(calendarCSV), strings.NewReader(calendarDatesCSV))
	if err != nil {
		t.Fatalf("LoadServiceDays: %v", err)
	}
	return s
}

func TestRegularWeekdayPattern(t *testing.T) {
	s := load(t)
	if !s.Runs("WEEKDAY", date(17)) { // Monday
		t.Error("WEEKDAY should run on Monday")
	}
	if s.Runs("WEEKDAY", date(22)) { // Saturday
		t.Error("WEEKDAY should not run on Saturday")
	}
	if !s.Runs("WEEKEND", date(22)) {
		t.Error("WEEKEND should run on Saturday")
	}
}

// An exception_type of 2 removes a day the regular pattern would have included.
// Getting this backwards silently adds services that are not running.
func TestExceptionRemovesRegularDay(t *testing.T) {
	s := load(t)
	if s.Runs("WEEKDAY", date(18)) { // Tuesday, explicitly removed
		t.Error("WEEKDAY was removed on 20260818 and must not run")
	}
	if !s.Runs("WEEKDAY", date(19)) { // Wednesday, unaffected
		t.Error("removal must not leak to adjacent days")
	}
}

// exception_type 1 adds a day the pattern excludes.
func TestExceptionAddsDay(t *testing.T) {
	s := load(t)
	if !s.Runs("WEEKEND", date(19)) { // Wednesday, explicitly added
		t.Error("WEEKEND was added on 20260819 and must run")
	}
}

// A service can exist only in calendar_dates.txt, with no calendar.txt row.
// Feeds that express everything through exceptions are legal and do occur.
func TestServiceWithNoRegularEntry(t *testing.T) {
	s := load(t)
	if !s.Runs("SPECIAL", date(20)) {
		t.Error("SPECIAL exists only as an exception and must run on its date")
	}
	if s.Runs("SPECIAL", date(21)) {
		t.Error("SPECIAL must not run on any other date")
	}
}

func TestDateRangeIsRespected(t *testing.T) {
	s := load(t)
	if s.Runs("EXPIRED", date(17)) {
		t.Error("EXPIRED ended in 2025 and must not run in 2026")
	}
}

func TestServiceIDsMatchesRuns(t *testing.T) {
	s := load(t)
	for _, d := range []int{17, 18, 19, 20, 22} {
		ids := s.ServiceIDs(date(d))
		for _, id := range []string{"WEEKDAY", "WEEKEND", "SPECIAL", "EXPIRED"} {
			if got, want := ids[id], s.Runs(id, date(d)); got != want {
				t.Errorf("day %d service %s: ServiceIDs=%v Runs=%v", d, id, got, want)
			}
		}
	}
}

// 25:30:00 is 1:30am on the *previous* day's timetable, not 1:30am today.
// Treating it as a wall clock drops or misplaces every after-midnight service,
// which on a Friday night is most of what someone is asking about.
func TestParseGTFSTimeHandlesAfterMidnight(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"00:00:00", 0},
		{"08:16:00", 8*3600 + 16*60},
		{"23:59:59", 23*3600 + 59*60 + 59},
		{"24:00:00", 24 * 3600},
		{"25:30:00", 25*3600 + 30*60},
		{"27:05:00", 27*3600 + 5*60},
	}
	for _, tc := range tests {
		got, err := ParseGTFSTime(tc.in)
		if err != nil {
			t.Errorf("ParseGTFSTime(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseGTFSTime(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if _, err := ParseGTFSTime("not a time"); err == nil {
		t.Error("want an error for an unparseable value")
	}
}

func TestFormatWrapsPastMidnight(t *testing.T) {
	for _, tc := range []struct {
		secs int
		want string
	}{
		{8*3600 + 16*60, "08:16"},
		{25*3600 + 30*60, "01:30"},
		{24 * 3600, "00:00"},
	} {
		if got := FormatGTFSTime(tc.secs); got != tc.want {
			t.Errorf("FormatGTFSTime(%d) = %q, want %q", tc.secs, got, tc.want)
		}
	}
}

// An after-midnight offset must land on the following calendar day.
func TestInstantCrossesMidnight(t *testing.T) {
	melb, err := time.LoadLocation("Australia/Melbourne")
	if err != nil {
		t.Skip("no tzdata")
	}
	serviceDay := time.Date(2026, 8, 14, 0, 0, 0, 0, melb) // Friday
	got := Instant(serviceDay, 25*3600+30*60, melb)        // 25:30 = 1:30am Saturday
	want := time.Date(2026, 8, 15, 1, 30, 0, 0, melb)
	if !got.Equal(want) {
		t.Errorf("Instant = %v, want %v", got, want)
	}
}

// GTFS files routinely carry a UTF-8 BOM. Left in place it corrupts the first
// header, so every lookup of that column returns "" and the failure is silent.
func TestReadCSVStripsBOM(t *testing.T) {
	rows, err := readCSV(strings.NewReader("\ufeffservice_id,monday\nX,1\n"))
	if err != nil {
		t.Fatalf("readCSV: %v", err)
	}
	if len(rows) != 1 || rows[0]["service_id"] != "X" {
		t.Errorf("BOM not stripped: %+v", rows)
	}
}

package gtfs

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLoadTransferTimesResolvesPlatformNames(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,mode,parent,platform) VALUES
		 ('RMD','Richmond Railway Station',2,'',''),
		 ('r8','Richmond Railway Station',2,'RMD','8'),
		 ('r3','Richmond Railway Station',2,'RMD','3'),
		 ('r9','Richmond Railway Station',2,'RMD','9'),
		 ('ERM','East Richmond Railway Station',2,'',''),
		 ('e1','East Richmond Railway Station',2,'ERM','1');
	`)
	ctx := context.Background()

	got, err := LoadTransferTimes(ctx, db, strings.NewReader(`
		{"Richmond": {"8-3": "90s"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if d := got[StopPair{From: "r8", To: "r3"}]; d != 90*time.Second {
		t.Errorf("8-3 = %v, want 90s (resolved to stop ids)", d)
	}
	if len(got) != 1 {
		t.Errorf("got %d entries, want just the one pair: %v", len(got), got)
	}
	// "Richmond" must not resolve to East Richmond.
	if _, ok := got[StopPair{From: "e1", To: "r3"}]; ok {
		t.Error("resolved to East Richmond; exact name should win")
	}
}

// "*" saves enumerating every pair at a big station, and a specific pair
// written alongside it still wins.
func TestLoadTransferTimesStationWildcard(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,mode,parent,platform) VALUES
		 ('RMD','Richmond Railway Station',2,'',''),
		 ('r8','Richmond',2,'RMD','8'),('r3','Richmond',2,'RMD','3'),('r9','Richmond',2,'RMD','9');
	`)
	got, err := LoadTransferTimes(context.Background(), db, strings.NewReader(`
		{"Richmond": {"*": "2m", "8-3": "30s"}}`))
	if err != nil {
		t.Fatal(err)
	}
	// 3 platforms -> 6 ordered pairs.
	if len(got) != 6 {
		t.Errorf("got %d pairs, want 6 from three platforms", len(got))
	}
	if d := got[StopPair{From: "r8", To: "r3"}]; d != 30*time.Second {
		t.Errorf("8-3 = %v, want the specific 30s to beat the wildcard", d)
	}
	if d := got[StopPair{From: "r9", To: "r3"}]; d != 2*time.Minute {
		t.Errorf("9-3 = %v, want the wildcard 2m", d)
	}
}

// A typo must fail loudly. An override that silently does nothing produces a
// journey nobody can explain later.
func TestLoadTransferTimesRejectsMistakes(t *testing.T) {
	db := testDB(t, `
		INSERT INTO stop(stop_id,name,mode,parent,platform) VALUES
		 ('RMD','Richmond Railway Station',2,'',''),('r8','Richmond',2,'RMD','8');
	`)
	ctx := context.Background()
	for _, c := range []struct{ name, in string }{
		{"unknown station", `{"Nowhere": {"1-2": "60s"}}`},
		{"unknown platform", `{"Richmond": {"8-99": "60s"}}`},
		{"malformed pair", `{"Richmond": {"eight to three": "60s"}}`},
		{"bad duration", `{"Richmond": {"8-8": "soon"}}`},
		{"not json", `nope`},
	} {
		if _, err := LoadTransferTimes(ctx, db, strings.NewReader(c.in)); err == nil {
			t.Errorf("%s: loaded without error", c.name)
		}
	}
}

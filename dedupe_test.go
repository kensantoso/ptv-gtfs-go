package gtfs

import "testing"

// Victoria publishes one Saturday train as three trips under one run number.
func TestDedupeCollapsesDuplicatePublications(t *testing.T) {
	in := []Departure{
		{TripID: "02-BEG--1-T2-3026", Seq: 22, StopID: "12241", Depart: at("12:35"),
			RouteID: "BEG", Headsign: "Flinders Street"},
		{TripID: "02-BEG--62-T2-3026", Seq: 22, StopID: "12241", Depart: at("12:35"),
			RouteID: "BEG", Headsign: "Flinders Street"},
		{TripID: "02-BEG--63-T2-3026", Seq: 16, StopID: "12241", Depart: at("12:35"),
			RouteID: "BEG", Headsign: "Flinders Street"},
	}
	got := dedupeDepartures(in)
	if len(got) != 1 {
		t.Fatalf("got %d departures, want 1", len(got))
	}
	if len(got[0].Alts) != 2 {
		t.Fatalf("got %d alternates, want 2", len(got[0].Alts))
	}
	if got[0].Alts[1].Seq != 16 {
		t.Errorf("alternate seq = %d, want 16 preserved from the variant", got[0].Alts[1].Seq)
	}
}

// Genuinely different services must survive. Each of these differs from the
// first in exactly one component of the key.
func TestDedupeKeepsDistinctServices(t *testing.T) {
	in := []Departure{
		{TripID: "a", StopID: "1", Depart: at("12:35"), RouteID: "BEG", Headsign: "Flinders Street"},
		{TripID: "b", StopID: "1", Depart: at("12:36"), RouteID: "BEG", Headsign: "Flinders Street"},
		{TripID: "c", StopID: "1", Depart: at("12:35"), RouteID: "LIL", Headsign: "Flinders Street"},
		{TripID: "d", StopID: "1", Depart: at("12:35"), RouteID: "BEG", Headsign: "Belgrave"},
		{TripID: "e", StopID: "2", Depart: at("12:35"), RouteID: "BEG", Headsign: "Flinders Street"},
	}
	if got := dedupeDepartures(in); len(got) != 5 {
		t.Fatalf("got %d departures, want all 5 kept", len(got))
	}
}

// Same clock time on different days is not a duplicate.
func TestDedupeIsDateAware(t *testing.T) {
	a := Departure{TripID: "a", StopID: "1", Depart: at("00:30"), RouteID: "BEG", Headsign: "X"}
	b := a
	b.TripID, b.Depart = "b", a.Depart.AddDate(0, 0, -1)
	if got := dedupeDepartures([]Departure{a, b}); len(got) != 2 {
		t.Fatalf("got %d, want 2: these are a day apart", len(got))
	}
}

// Three publications of one train must plan as one option, not three.
func TestDedupeJourneysCollapsesIdenticalTravel(t *testing.T) {
	mk := func(trip string, fromSeq, toSeq int) Journey {
		return Journey{
			Legs: []Leg{{
				TripID: trip, FromStop: "A", ToStop: "B", FromSeq: fromSeq, ToSeq: toSeq,
				Depart: at("14:52"), Arrive: at("15:12"),
			}},
			Depart: at("14:52"), Arrive: at("15:12"),
		}
	}
	got := dedupeJourneys([]Journey{
		mk("02-BEG--1-T2-3026", 1, 9),
		mk("02-BEG--62-T2-3026", 1, 9),
		mk("02-BEG--63-T2-3026", 5, 13),
	})
	if len(got) != 1 {
		t.Fatalf("got %d journeys, want 1", len(got))
	}
	alts := got[0].Legs[0].Alts
	if len(alts) != 2 {
		t.Fatalf("got %d alternates, want 2", len(alts))
	}
	if alts[1].FromSeq != 5 || alts[1].ToSeq != 13 {
		t.Errorf("alternate seqs = %d/%d, want 5/13 preserved", alts[1].FromSeq, alts[1].ToSeq)
	}
}

// Different departure times are different journeys.
func TestDedupeJourneysKeepsDistinctTimes(t *testing.T) {
	mk := func(trip, dep, arr string) Journey {
		return Journey{
			Legs:   []Leg{{TripID: trip, FromStop: "A", ToStop: "B", Depart: at(dep), Arrive: at(arr)}},
			Depart: at(dep), Arrive: at(arr),
		}
	}
	got := dedupeJourneys([]Journey{
		mk("a", "14:52", "15:12"),
		mk("b", "15:12", "15:32"),
	})
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

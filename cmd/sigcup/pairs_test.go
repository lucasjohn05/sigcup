package main

import (
	"testing"

	"sigcup/internal/sim"
)

func TestPairRelationships(t *testing.T) {
	races := []sim.Race{
		{Key: "TX-SEN", Kind: "sen", State: "TX", AntiParty: "D", AntiID: "1", GOPID: "2"},
		{Key: "NE-SEN", Kind: "sen", State: "NE", AntiParty: "I", AntiID: "3", GOPID: "4"},
		{Key: "RI-GOV", Kind: "gov", State: "RI", AntiParty: "D", AntiID: "5", GOPID: "6", Note: "has an Independent market too"},
		{Key: "GA-GOV", Kind: "gov", State: "GA", AntiParty: "D", AntiID: "7", GOPID: "8"},
		{Key: "US-SEN", Kind: "control", State: "US", AntiParty: "D", AntiID: "9", GOPID: "10"},
		{Key: "XX-01", Kind: "house", State: "XX", AntiParty: "D", AntiID: "", GOPID: "12"},
	}
	rels := pairRelationships(races)
	if len(rels) != 5 {
		t.Fatalf("want 5 (one skipped for missing id), got %d", len(rels))
	}
	want := map[string]bool{"pair-TX-SEN": true, "pair-NE-SEN": false, "pair-RI-GOV": false, "pair-GA-GOV": false, "pair-US-SEN": false}
	for _, r := range rels {
		if r.IsExhaustive != want[r.ID] {
			t.Errorf("%s exhaustive = %v, want %v", r.ID, r.IsExhaustive, want[r.ID])
		}
		if r.Type != "mutually_exclusive" || len(r.Nodes) != 2 {
			t.Errorf("%s malformed: %+v", r.ID, r)
		}
	}
}

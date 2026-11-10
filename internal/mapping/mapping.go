// Package mapping loads the hand-maintained SUSQ -> Kalshi contract map.
package mapping

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Row struct {
	ExchangeID   string // SUSQ exchange (tradable contract) id
	KalshiTicker string
	Invert       bool // SUSQ YES == Kalshi NO
	Note         string
	// Verified skips the suspect-gap check. Set it only after reading both
	// Info tabs and confirming a large SUSQ/Kalshi gap is real.
	Verified bool
	// Fair is a fixed fair value from fair.csv (0 = price off Kalshi).
	Fair float64
}

func Load(path string) ([]Row, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.Comment = '#'
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	var rows []Row
	seen := map[string]bool{}
	for i, rec := range recs {
		if len(rec) < 2 || strings.TrimSpace(rec[0]) == "" {
			continue
		}
		id := strings.TrimSpace(rec[0])
		if id == "susq_exchange_id" {
			continue
		}
		if seen[id] {
			return nil, fmt.Errorf("mapping line %d: duplicate exchange %s", i+1, id)
		}
		seen[id] = true
		row := Row{ExchangeID: id, KalshiTicker: strings.TrimSpace(rec[1])}
		if len(rec) > 2 {
			row.Invert = strings.EqualFold(strings.TrimSpace(rec[2]), "true")
		}
		if len(rec) > 3 {
			row.Note = strings.TrimSpace(rec[3])
		}
		if len(rec) > 4 {
			row.Verified = strings.EqualFold(strings.TrimSpace(rec[4]), "true")
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// LoadFair reads fair.csv: exchange_id,p_yes,source,verified. Use it for
// contracts with no liquid Kalshi market, e.g. House seats priced from
// Cook/Sabato/Inside Elections ratings. p_yes is the YES probability of that
// SUSQ contract.
func LoadFair(path string) ([]Row, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.Comment = '#'
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	var rows []Row
	seen := map[string]bool{}
	for i, rec := range recs {
		if len(rec) < 2 || strings.TrimSpace(rec[0]) == "" || strings.TrimSpace(rec[0]) == "exchange_id" {
			continue
		}
		id := strings.TrimSpace(rec[0])
		if seen[id] {
			return nil, fmt.Errorf("fair line %d: duplicate exchange %s", i+1, id)
		}
		seen[id] = true
		p, err := strconv.ParseFloat(strings.TrimSpace(rec[1]), 64)
		if err != nil || p <= 0 || p >= 1 {
			return nil, fmt.Errorf("fair line %d: p_yes must be in (0,1)", i+1)
		}
		row := Row{ExchangeID: id, Fair: p}
		if len(rec) > 2 {
			row.Note = strings.TrimSpace(rec[2])
		}
		if len(rec) > 3 {
			row.Verified = strings.EqualFold(strings.TrimSpace(rec[3]), "true")
		}
		rows = append(rows, row)
	}
	return rows, nil
}

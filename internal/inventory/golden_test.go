package inventory

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// update regenerates the golden files instead of asserting against them:
//
//	go test ./internal/inventory -run Golden -update
//
// Registering a custom flag in a test file is the standard Go idiom for this.
// The testing package parses flags it does not recognise into the flag package,
// so no extra wiring is needed.
//
// The discipline that makes golden files worth having: when a diff appears, read
// it and decide whether the change is intended. Running -update reflexively
// turns the test into a rubber stamp that asserts the code equals itself.
var update = flag.Bool("update", false, "regenerate golden files")

func TestGoldenRecommendation(t *testing.T) {
	// A realistic perishable: fresh bagels with weekend spikes, day-to-day noise,
	// a stockout stretch mid-window, a 3-day shelf life, and a 12-unit case.
	// Chosen because the payload exercises censoring, seasonality, a non-zero
	// safety stock and case rounding in one pass, so the golden file is a genuine
	// end-to-end record of the pipeline rather than a snapshot of arithmetic
	// already covered by the unit tests.
	history := buildBagelHistory()

	stats, err := ComputeDemandStats(history, DefaultHistoryWindowDays, DefaultAlpha)
	if err != nil {
		t.Fatalf("ComputeDemandStats: %v", err)
	}

	product := &Product{
		SKU:                  "BAGEL-PLAIN-6PK",
		Name:                 "Plain Bagels, 6 pack",
		LeadTimeDays:         2,
		ReviewPeriodDays:     1,
		CaseSize:             12,
		MinimumOrderQuantity: 12,
		ShelfLifeDays:        3,
		TargetServiceLevel:   1.65, // ~95% cycle service level
		OnHandUnits:          14,
		OnOrderUnits:         0,
	}

	rec, err := Recommend(product, stats)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}

	// MarshalIndent so the golden file is readable and its diffs are line-scoped.
	got, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	golden := filepath.Join("testdata", "recommendation_bagel.golden.json")

	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", golden)
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}

	if string(got) != string(want) {
		t.Errorf("recommendation payload differs from golden file.\n"+
			"--- want (%s)\n%s\n--- got\n%s\n"+
			"If this change is intended, rerun with -update.", golden, want, got)
	}
}

// jitter is a fixed 28-element noise sequence, written out rather than
// generated so the fixture is reproducible without seeding a PRNG and without
// depending on any particular Go version's random implementation.
//
// It exists because a perfectly periodic series deseasonalizes to a flat line,
// which drives sigma to exactly zero and produces a golden payload with no
// safety stock — a snapshot that would not exercise the term at all.
var jitter = [28]int{
	0, 2, -1, 1, -2, 3, -1,
	1, -3, 2, 0, -1, 2, 1,
	-2, 1, 3, -1, 0, 2, -3,
	1, 0, -2, 2, -1, 1, 0,
}

// buildBagelHistory produces 28 days of deterministic sales for the golden test.
//
// Weekends sell roughly double. Days 10 through 12 are stockouts: the recorded
// units are the truncated figure we managed to sell before the shelf emptied,
// and they must not drag the demand estimate down.
func buildBagelHistory() []SalesDay {
	start := time.Date(2026, 5, 4, 0, 0, 0, 0, time.UTC) // a Monday

	weekdayUnits := []int{18, 16, 17, 19, 22} // Mon..Fri
	weekendUnits := []int{34, 31}             // Sat, Sun

	days := make([]SalesDay, 0, 28)
	for i := 0; i < 28; i++ {
		date := start.AddDate(0, 0, i)

		var units int
		switch wd := date.Weekday(); wd {
		case time.Saturday:
			units = weekendUnits[0]
		case time.Sunday:
			units = weekendUnits[1]
		default:
			units = weekdayUnits[int(wd)-1]
		}
		units += jitter[i]

		day := SalesDay{Date: date, UnitsSold: units}

		// A three-day stockout run: sold out early each day, so the recorded
		// figure understates true demand.
		if i >= 10 && i <= 12 {
			day.UnitsSold = units / 2
			day.StockOutOccurred = true
		}

		days = append(days, day)
	}
	return days
}

package main

import (
	"math/rand/v2"
	"testing"
	"time"
)

// The seed generator is demo code, but it is demo code an interviewer will run.
// A seed that quietly produces zeros, or violates a database constraint halfway
// through loading, wastes the one chance to show the logic working. These tests
// need no Docker and no database.

func newRNG() *rand.Rand {
	return rand.New(rand.NewPCG(20260601, 20260601^0x9e3779b97f4a7c15))
}

// endDate is fixed rather than derived from time.Now, so a test cannot pass on
// a Tuesday and fail on a Sunday.
var seedEnd = time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC) // a Tuesday

func TestGenerateIsDeterministic(t *testing.T) {
	// The README quotes real numbers from this seed. If the generator drifts,
	// the documentation silently becomes fiction.
	sku := findSKU(t, "BAGEL-PLAIN-6PK")

	first := generate(sku, 60, seedEnd, newRNG())
	second := generate(sku, 60, seedEnd, newRNG())

	if len(first.Days) != len(second.Days) {
		t.Fatalf("day counts differ: %d vs %d", len(first.Days), len(second.Days))
	}
	for i := range first.Days {
		if first.Days[i] != second.Days[i] {
			t.Fatalf("day %d differs between runs: %+v vs %+v",
				i, first.Days[i], second.Days[i])
		}
	}
	if first.TrueDemandTotal != second.TrueDemandTotal {
		t.Errorf("true demand differs: %d vs %d",
			first.TrueDemandTotal, second.TrueDemandTotal)
	}
}

func TestGeneratedDataSatisfiesDatabaseConstraints(t *testing.T) {
	// Every CHECK constraint in migrations/0001_init.up.sql, asserted against
	// the generated data. Finding a violation here costs a second; finding it
	// during `make seed` means a half-loaded database and a confusing error.
	rng := newRNG()

	for _, s := range catalog {
		t.Run(s.SKU, func(t *testing.T) {
			g := generate(s, 60, seedEnd, rng)

			if err := g.Product.Validate(); err != nil {
				t.Fatalf("generated product fails domain validation: %v", err)
			}
			if g.Product.OnHandUnits < 0 || g.Product.OnOrderUnits < 0 {
				t.Errorf("negative position: on_hand=%d on_order=%d",
					g.Product.OnHandUnits, g.Product.OnOrderUnits)
			}

			seen := make(map[string]bool, len(g.Days))
			for i, day := range g.Days {
				if day.UnitsSold < 0 {
					t.Errorf("day %d has negative units_sold %d", i, day.UnitsSold)
				}
				// Deliberately NOT asserted: that a censored day sold something.
				// A day that opened with an empty shelf sells zero and is fully
				// censored, which is a valid and important row. See the comment
				// on sales_days in migrations/0001_init.up.sql.
				// The (sku, sales_date) primary key.
				key := day.Date.Format(time.DateOnly)
				if seen[key] {
					t.Errorf("duplicate date %s would violate the primary key", key)
				}
				seen[key] = true

				if !day.Date.Equal(day.Date.UTC().Truncate(24 * time.Hour)) {
					t.Errorf("day %d has a non-midnight timestamp: %v", i, day.Date)
				}
			}
		})
	}
}

func TestHistoryEndsOnTheRequestedDate(t *testing.T) {
	// If the newest row falls outside the estimator's history window, every SKU
	// returns 422 and the demo is dead on arrival. This is the single most
	// likely way for a seeder to be silently broken.
	rng := newRNG()

	for _, s := range catalog {
		t.Run(s.SKU, func(t *testing.T) {
			g := generate(s, 60, seedEnd, rng)
			if len(g.Days) == 0 {
				t.Fatal("no days generated")
			}
			last := g.Days[len(g.Days)-1].Date
			if !last.Equal(seedEnd) {
				t.Errorf("history ends %v, want %v", last.Format(time.DateOnly), seedEnd.Format(time.DateOnly))
			}
		})
	}
}

func TestHistoryLength(t *testing.T) {
	rng := newRNG()

	tests := []struct {
		name     string
		sku      string
		days     int
		wantDays int
	}{
		{name: "full window", sku: "MILK-WHOLE-1L", days: 60, wantDays: 60},
		{name: "shorter window", sku: "MILK-WHOLE-1L", days: 28, wantDays: 28},
		{
			name: "newly listed sku is truncated",
			// The 422 demo. HistoryDays caps this one at a single day regardless
			// of the requested window.
			sku: "KIMCHI-JAR-400G", days: 60, wantDays: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := generate(findSKU(t, tt.sku), tt.days, seedEnd, rng)
			if len(g.Days) != tt.wantDays {
				t.Errorf("got %d days, want %d", len(g.Days), tt.wantDays)
			}
		})
	}
}

func TestSupplyGapProducesCensoredDays(t *testing.T) {
	// The whole point of the AVOCADO entry. If the simulation never starves it,
	// the censored-demand story has no worked example behind it.
	rng := newRNG()

	for _, sku := range []string{"AVOCADO-HASS-EA", "BAGEL-PLAIN-6PK"} {
		t.Run(sku, func(t *testing.T) {
			g := generate(findSKU(t, sku), 60, seedEnd, rng)

			// At least three, not merely non-zero. A single censored day is
			// indistinguishable from noise once the estimator excludes it, and
			// it was exactly what an earlier version of this simulation produced
			// while still passing a "> 0" assertion.
			const wantAtLeast = 3
			if g.CensoredDays < wantAtLeast {
				t.Fatalf("only %d censored days, want at least %d; the supply gap "+
					"is not actually starving this SKU", g.CensoredDays, wantAtLeast)
			}
			lost := g.TrueDemandTotal - g.SoldTotal
			if lost <= 0 {
				t.Errorf("lost demand = %d, want > 0", lost)
			}
			t.Logf("%s: %d censored days, %d units of demand lost", sku, g.CensoredDays, lost)

			// Generating a supply gap is not enough — it has to land inside the
			// window the estimator actually reads. A gap in the older half of
			// the history is stored faithfully and then never looked at, which
			// produced a demo where the flagship censored-demand SKU reported
			// zero censored days.
			windowStart := len(g.Days) - defaultWindowDays
			if windowStart < 0 {
				windowStart = 0
			}
			var inWindow int
			for _, day := range g.Days[windowStart:] {
				if day.StockOutOccurred {
					inWindow++
				}
			}
			if inWindow == 0 {
				t.Errorf("all %d censored days fall outside the %d-day estimation "+
					"window; the service will never see them",
					g.CensoredDays, defaultWindowDays)
			}
		})
	}
}

// defaultWindowDays mirrors inventory.DefaultHistoryWindowDays. Restated rather
// than imported so this file stays about the seed data, but if the domain
// default changes these tests should be revisited.
const defaultWindowDays = 28

func TestCensoringSuppressesObservedSales(t *testing.T) {
	// The property that makes the seed data a real test of the estimator: on a
	// censored day, recorded sales must be strictly below true demand. If they
	// were equal, the flag would be decorative.
	g := generate(findSKU(t, "AVOCADO-HASS-EA"), 60, seedEnd, newRNG())

	if g.SoldTotal >= g.TrueDemandTotal {
		t.Errorf("sold %d of %d true demand; censoring suppressed nothing",
			g.SoldTotal, g.TrueDemandTotal)
	}
}

func TestUncensoredSKUsLoseNothing(t *testing.T) {
	// The converse. A SKU with no supply gap should be kept in stock by the
	// simulated buyer, so its recorded sales are its true demand. If these SKUs
	// were also starved, "censored" would stop meaning anything.
	rng := newRNG()

	for _, sku := range []string{"MILK-WHOLE-1L", "RICE-BASMATI-1KG", "CHEESE-CHEDDAR-200G"} {
		t.Run(sku, func(t *testing.T) {
			g := generate(findSKU(t, sku), 60, seedEnd, rng)

			lost := g.TrueDemandTotal - g.SoldTotal
			// A little loss at the very start is expected while the simulation
			// settles, so this is a proportion rather than an exact zero.
			if float64(lost) > 0.05*float64(g.TrueDemandTotal) {
				t.Errorf("lost %d of %d units (%.1f%%); this SKU has no supply gap "+
					"and should stay in stock", lost, g.TrueDemandTotal,
					100*float64(lost)/float64(g.TrueDemandTotal))
			}
		})
	}
}

func TestWeekendLiftIsVisible(t *testing.T) {
	// If the weekend pattern does not survive into the data, the day-of-week
	// factors the estimator computes are all 1.0 and that half of the model is
	// invisible in the demo.
	g := generate(findSKU(t, "BEER-IPA-6PK"), 60, seedEnd, newRNG())

	var weekend, weekday float64
	var weekendN, weekdayN int
	for _, day := range g.Days {
		if day.StockOutOccurred {
			continue // censored days understate demand and would skew this
		}
		switch day.Date.Weekday() {
		case time.Saturday, time.Sunday:
			weekend += float64(day.UnitsSold)
			weekendN++
		default:
			weekday += float64(day.UnitsSold)
			weekdayN++
		}
	}

	if weekendN == 0 || weekdayN == 0 {
		t.Fatal("not enough data")
	}
	weekendAvg := weekend / float64(weekendN)
	weekdayAvg := weekday / float64(weekdayN)

	t.Logf("weekend avg %.1f, weekday avg %.1f, ratio %.2f",
		weekendAvg, weekdayAvg, weekendAvg/weekdayAvg)

	// The catalogue asks for a 2.6x lift; anything below 1.5 means the signal is
	// being swamped by noise or the shape is not being applied.
	if weekendAvg < 1.5*weekdayAvg {
		t.Errorf("weekend average %.1f is not meaningfully above weekday %.1f",
			weekendAvg, weekdayAvg)
	}
}

func TestLongTailSKUIsMostlyZeros(t *testing.T) {
	// A slow mover must produce genuine zero-demand days, not a flat trickle.
	// Zeros are data — the estimator has to handle them — and rounding 0.32 to 0
	// every single day would make this look like a dead SKU instead of a slow one.
	g := generate(findSKU(t, "TRUFFLE-OIL-100ML"), 60, seedEnd, newRNG())

	var zeros, nonzero int
	for _, day := range g.Days {
		if day.UnitsSold == 0 {
			zeros++
		} else {
			nonzero++
		}
	}

	t.Logf("truffle oil: %d zero days, %d selling days, %d units total",
		zeros, nonzero, g.SoldTotal)

	if zeros == 0 {
		t.Error("no zero-demand days; this is not behaving like a long-tail SKU")
	}
	if nonzero == 0 {
		t.Error("never sold anything; this looks dead rather than slow-moving")
	}
	// Roughly one unit every three days over 60 days is ~20 units. Wide bounds,
	// because the point is the shape, not an exact figure.
	if g.SoldTotal < 5 || g.SoldTotal > 60 {
		t.Errorf("sold %d units over 60 days; expected a slow trickle", g.SoldTotal)
	}
}

func TestCatalogIsWellFormed(t *testing.T) {
	seen := make(map[string]bool, len(catalog))

	for _, s := range catalog {
		t.Run(s.SKU, func(t *testing.T) {
			if seen[s.SKU] {
				t.Fatalf("duplicate SKU %q; the second would overwrite the first", s.SKU)
			}
			seen[s.SKU] = true

			if s.Name == "" {
				t.Error("missing name")
			}
			if s.BaseDemand <= 0 {
				t.Errorf("BaseDemand = %v, want > 0", s.BaseDemand)
			}
			if s.WeekendLift <= 0 {
				t.Errorf("WeekendLift = %v, want > 0", s.WeekendLift)
			}
			if s.CaseSize < 1 {
				t.Errorf("CaseSize = %d, want >= 1", s.CaseSize)
			}
			if s.ReviewPeriodDays < 1 {
				t.Errorf("ReviewPeriodDays = %d, want >= 1", s.ReviewPeriodDays)
			}
			// A minimum that is not a whole number of cases is a data-entry
			// error in a real catalogue too.
			if s.MinimumOrderQuantity > 0 && s.CaseSize > 1 &&
				s.MinimumOrderQuantity%s.CaseSize != 0 {
				t.Errorf("MinimumOrderQuantity %d is not a multiple of case size %d",
					s.MinimumOrderQuantity, s.CaseSize)
			}
		})
	}

	t.Run("catalogue covers the interesting cases", func(t *testing.T) {
		// A guard against someone trimming the catalogue and silently removing
		// the reason it exists.
		var shortShelfLife, vendorMinimum, longTail, truncated, overstocked int
		for _, s := range catalog {
			if s.ShelfLifeDays > 0 && s.ShelfLifeDays <= 3 {
				shortShelfLife++
			}
			if s.MinimumOrderQuantity > s.CaseSize {
				vendorMinimum++
			}
			if s.BaseDemand < 1 {
				longTail++
			}
			if s.HistoryDays > 0 {
				truncated++
			}
			if s.FinalOnHand > 500 {
				overstocked++
			}
		}

		checks := []struct {
			name string
			got  int
		}{
			{"SKUs with a shelf life short enough to bind", shortShelfLife},
			{"SKUs whose vendor minimum exceeds a case", vendorMinimum},
			{"long-tail SKUs", longTail},
			{"SKUs with too little history for a recommendation", truncated},
			{"deliberately overstocked SKUs", overstocked},
		}
		for _, c := range checks {
			if c.got == 0 {
				t.Errorf("no %s; the demo can no longer show that behaviour", c.name)
			}
		}
	})
}

// generateAsSeeded reproduces one SKU exactly as `make seed` would produce it.
//
// This matters and is easy to get wrong. Every SKU draws from one shared
// generator, so the numbers a SKU ends up with depend on how many draws the
// SKUs *before* it consumed. Calling generate() on a single entry with a fresh
// RNG produces perfectly valid data that the service will never actually serve.
//
// An earlier version of the censoring test did exactly that, and the figures it
// logged — which were then quoted in the README — did not match what the API
// returned for the same SKU. Tests whose numbers appear in documentation have to
// walk the same path the real thing walks.
func generateAsSeeded(t *testing.T, sku string, days int, end time.Time) generated {
	t.Helper()

	rng := newRNG()
	for _, s := range catalog {
		g := generate(s, days, end, rng) // advances rng for every SKU, in order
		if s.SKU == sku {
			return g
		}
	}
	t.Fatalf("SKU %q is not in the catalogue", sku)
	return generated{}
}

func findSKU(t *testing.T, sku string) seedSKU {
	t.Helper()
	for _, s := range catalog {
		if s.SKU == sku {
			return s
		}
	}
	t.Fatalf("SKU %q is not in the catalogue", sku)
	return seedSKU{}
}

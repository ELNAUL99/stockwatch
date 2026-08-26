package main

import (
	"testing"
	"time"

	"github.com/ELNAUL99/stockwatch/internal/inventory"
)

// TestCensoringChangesTheAnswer is the end-to-end claim of this whole project,
// measured on the seed data.
//
// It runs the real estimator over a starved SKU twice: once on the data as
// recorded, and once with every stockout flag stripped — which is exactly what a
// system that logged sales without tracking availability would have. If the two
// agree, the censored-demand handling is decorative and the README's central
// section is not true.
func TestCensoringChangesTheAnswer(t *testing.T) {
	const target = "AVOCADO-HASS-EA"
	sku := findSKU(t, target)

	// Generated through the same RNG sequence the seeder uses, so the figures
	// this test logs are the figures the API returns for this SKU.
	g := generateAsSeeded(t, target, 60, seedEnd)

	withFlags := g.Days

	// The naive dataset: identical sales figures, no availability information.
	// Note this is a copy — mutating g.Days in place would corrupt the fixture
	// for anything that ran afterwards, since a Go slice is a view over shared
	// memory.
	naive := make([]inventory.SalesDay, len(withFlags))
	copy(naive, withFlags)
	for i := range naive {
		naive[i].StockOutOccurred = false
	}

	honest, err := inventory.ComputeDemandStats(withFlags, 28, inventory.DefaultAlpha)
	if err != nil {
		t.Fatalf("ComputeDemandStats (censoring respected): %v", err)
	}
	blind, err := inventory.ComputeDemandStats(naive, 28, inventory.DefaultAlpha)
	if err != nil {
		t.Fatalf("ComputeDemandStats (censoring ignored): %v", err)
	}

	t.Logf("censoring respected: %.2f units/day over %d uncensored days (%d excluded)",
		honest.AverageDailyDemand, honest.DataPoints, honest.CensoredDays)
	t.Logf("censoring ignored:   %.2f units/day over %d days",
		blind.AverageDailyDemand, blind.DataPoints)

	if honest.CensoredDays == 0 {
		t.Fatal("no censored days in the window; this test is not measuring anything")
	}

	// A truncated observation is by definition at or below true demand, so
	// averaging them in biases the mean downward. This is the claim that holds
	// on the underlying statistics.
	if blind.AverageDailyDemand >= honest.AverageDailyDemand {
		t.Errorf("ignoring censoring gave %.2f units/day, which is not below the "+
			"%.2f from respecting it; the stockout handling is doing nothing",
			blind.AverageDailyDemand, honest.AverageDailyDemand)
	}
	t.Logf("ignoring stockouts understates the mean by %.1f%%",
		100*(1-blind.AverageDailyDemand/honest.AverageDailyDemand))

	// The less obvious half, and the more damaging one: the fabricated low days
	// also inflate sigma, because a drop to zero and back looks like enormous
	// volatility. It is not demand volatility — it is the variance of our own
	// stockouts, being mistaken for uncertainty about customers.
	if blind.DemandStdDev <= honest.DemandStdDev {
		t.Errorf("ignoring censoring gave sigma %.2f, not above the honest %.2f; "+
			"the fabricated low days should look like volatility",
			blind.DemandStdDev, honest.DemandStdDev)
	}
	t.Logf("and inflates sigma by %.0f%% (%.2f -> %.2f)",
		100*(blind.DemandStdDev/honest.DemandStdDev-1),
		honest.DemandStdDev, blind.DemandStdDev)

	// Now the consequence an operator actually feels: a smaller order.
	product := &inventory.Product{
		SKU: sku.SKU, Name: sku.Name,
		LeadTimeDays:       sku.LeadTimeDays,
		ReviewPeriodDays:   sku.ReviewPeriodDays,
		CaseSize:           sku.CaseSize,
		ShelfLifeDays:      sku.ShelfLifeDays,
		TargetServiceLevel: sku.TargetServiceLevel,
		// The position the seeder actually loads, not an invented one, so the
		// order quantities below match what /recommendation returns.
		OnHandUnits:  g.Product.OnHandUnits,
		OnOrderUnits: g.Product.OnOrderUnits,
	}

	honestRec, err := inventory.Recommend(product, honest)
	if err != nil {
		t.Fatalf("Recommend (censoring respected): %v", err)
	}
	blindRec, err := inventory.Recommend(product, blind)
	if err != nil {
		t.Fatalf("Recommend (censoring ignored): %v", err)
	}

	t.Logf("order with censoring respected: %d units (reorder point %.1f)",
		honestRec.RecommendedQuantity, honestRec.ReorderPoint)
	t.Logf("order with censoring ignored:   %d units (reorder point %.1f)",
		blindRec.RecommendedQuantity, blindRec.ReorderPoint)

	// Deliberately NOT asserted: that the blind version orders fewer units.
	//
	// An earlier version of this test did assert that, and it was wrong. The two
	// biases push in opposite directions — the depressed mean lowers the target
	// level, while the inflated sigma raises safety stock — so which one wins
	// depends on the SKU's lead time and service level. On this catalogue the
	// blind estimate under-orders at some stock positions and over-orders at
	// others.
	//
	// That is a worse failure than a consistent bias, not a better one: the
	// error is not a predictable offset a buyer could learn to correct for, it
	// changes sign depending on the item. What can be asserted is that the
	// numbers differ, and the statistics above say why.
	if honestRec.RecommendedQuantity == blindRec.RecommendedQuantity &&
		honestRec.ReorderPoint == blindRec.ReorderPoint {
		t.Error("censoring made no difference to either the order or the reorder point")
	}
}

// TestFullyCensoredDaysAreExcluded pins the bug that the seed generator found.
//
// A day where the shelf was empty at opening records zero sales. If it is not
// flagged as censored, the estimator treats it as genuine zero demand and
// averages a fabricated zero into the mean. That is the worst possible input,
// and an earlier version of the schema made it the only representable one.
func TestFullyCensoredDaysAreExcluded(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// Fourteen days at 30 units, then three days with an empty shelf.
	var history []inventory.SalesDay
	for i := 0; i < 14; i++ {
		history = append(history, inventory.SalesDay{
			Date: base.AddDate(0, 0, i), UnitsSold: 30,
		})
	}
	for i := 14; i < 17; i++ {
		history = append(history, inventory.SalesDay{
			Date: base.AddDate(0, 0, i), UnitsSold: 0, StockOutOccurred: true,
		})
	}

	stats, err := inventory.ComputeDemandStats(history, 28, inventory.DefaultAlpha)
	if err != nil {
		t.Fatalf("ComputeDemandStats: %v", err)
	}

	if stats.CensoredDays != 3 {
		t.Errorf("CensoredDays = %d, want 3", stats.CensoredDays)
	}
	if stats.DataPoints != 14 {
		t.Errorf("DataPoints = %d, want 14", stats.DataPoints)
	}
	// The demand estimate must be unmoved by the empty-shelf days.
	if !approx(stats.AverageDailyDemand, 30, 0.01) {
		t.Errorf("AverageDailyDemand = %.2f, want 30; the empty-shelf days were "+
			"averaged in as genuine zeros", stats.AverageDailyDemand)
	}

	// The counterfactual: the same rows without the flag, which is what the old
	// schema forced. 14 days at 30 plus 3 zeros averages to about 24.7.
	unflagged := make([]inventory.SalesDay, len(history))
	copy(unflagged, history)
	for i := range unflagged {
		unflagged[i].StockOutOccurred = false
	}

	blind, err := inventory.ComputeDemandStats(unflagged, 28, inventory.DefaultAlpha)
	if err != nil {
		t.Fatalf("ComputeDemandStats: %v", err)
	}
	if blind.AverageDailyDemand >= 29 {
		t.Errorf("the unflagged estimate is %.2f; it should be visibly depressed "+
			"by the three fabricated zeros", blind.AverageDailyDemand)
	}
	t.Logf("flagged: %.2f units/day; unflagged: %.2f units/day (%.0f%% lower)",
		stats.AverageDailyDemand, blind.AverageDailyDemand,
		100*(1-blind.AverageDailyDemand/stats.AverageDailyDemand))
}

func approx(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}

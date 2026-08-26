package main

import "time"

// seedSKU is a catalogue entry plus the parameters used to synthesise its sales.
//
// The demand parameters live here rather than in inventory.Product because they
// describe the *simulation*, not the product. The service never sees them: it
// only ever sees the sales rows they produce, exactly as it would see rows from
// a real point-of-sale feed.
type seedSKU struct {
	SKU  string
	Name string

	// Vendor and policy terms, copied onto inventory.Product.
	LeadTimeDays         int
	MinimumOrderQuantity int
	CaseSize             int
	ShelfLifeDays        int
	ReviewPeriodDays     int
	TargetServiceLevel   float64

	// --- Simulation parameters ---

	// BaseDemand is average units per day on a typical weekday.
	BaseDemand float64
	// WeekendLift multiplies Saturday and Sunday demand. Grocery weekend
	// patterns are real and large: a bakery item can double, while milk barely
	// moves. Getting this wrong is what makes synthetic data look synthetic.
	WeekendLift float64
	// Noise is the coefficient of variation of day-to-day demand. Staples are
	// steady, treats are volatile.
	Noise float64
	// Trend is the fractional change in demand across the whole window.
	// +0.30 means demand ends 30% above where it started.
	Trend float64

	// SupplyGapStart and SupplyGapDays force a delivery outage, which starves
	// the SKU and produces a genuine run of censored days. Zero means no gap.
	SupplyGapStart int
	SupplyGapDays  int

	// FinalOnHand overrides the simulated closing position, for SKUs that need
	// to demonstrate a specific decision. Negative means "use the simulation".
	FinalOnHand  int
	FinalOnOrder int

	// HistoryDays truncates this SKU's history, for demonstrating what happens
	// when there is not enough data. Zero means the full window.
	HistoryDays int

	// Note explains what this SKU is here to demonstrate. Printed by the seeder
	// so the demo is self-describing.
	Note string
}

// catalog is 21 SKUs chosen so that a `make seed` demo exercises every branch of
// the decision logic. A seed set where everything returns a plain rounded number
// proves nothing; each entry below is here to make one specific behaviour
// visible.
var catalog = []seedSKU{
	// --- Dairy: steady, high-volume staples ---------------------------------
	{
		SKU: "MILK-WHOLE-1L", Name: "Whole Milk 1L",
		LeadTimeDays: 2, CaseSize: 12, MinimumOrderQuantity: 12,
		ShelfLifeDays: 12, ReviewPeriodDays: 1, TargetServiceLevel: 2.05,
		BaseDemand: 46, WeekendLift: 1.15, Noise: 0.12,
		// Positions are pinned on the SKUs that carry a specific lesson. The
		// simulation leaves most items comfortably stocked, which is realistic —
		// a real buyer does not reorder everything every day — but a demo where
		// nine SKUs in ten answer "hold" teaches nothing.
		FinalOnHand: 100,
		Note:        "high-volume staple; the plainest case-size rounding in the set",
	},
	{
		SKU: "MILK-OAT-1L", Name: "Oat Milk 1L",
		LeadTimeDays: 3, CaseSize: 6, MinimumOrderQuantity: 6,
		ShelfLifeDays: 21, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 17, WeekendLift: 1.25, Noise: 0.20, Trend: 0.35,
		FinalOnHand: -1,
		Note:        "growing category; the trend pulls the estimate above a flat average",
	},
	{
		SKU: "YOGURT-GREEK-500G", Name: "Greek Yogurt 500g",
		LeadTimeDays: 3, CaseSize: 12, MinimumOrderQuantity: 12,
		ShelfLifeDays: 14, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 21, WeekendLift: 1.2, Noise: 0.18,
		FinalOnHand: -1,
	},
	{
		SKU: "CHEESE-CHEDDAR-200G", Name: "Mature Cheddar 200g",
		LeadTimeDays: 4, CaseSize: 12, MinimumOrderQuantity: 12,
		ShelfLifeDays: 45, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 12, WeekendLift: 1.35, Noise: 0.22,
		FinalOnHand: -1,
	},
	{
		SKU: "EGGS-FREERANGE-12", Name: "Free Range Eggs, dozen",
		LeadTimeDays: 2, CaseSize: 12, MinimumOrderQuantity: 12,
		ShelfLifeDays: 21, ReviewPeriodDays: 1, TargetServiceLevel: 2.05,
		BaseDemand: 29, WeekendLift: 1.45, Noise: 0.16,
		FinalOnHand: -1,
	},

	// --- Bakery: short shelf life, strong weekend pattern -------------------
	{
		SKU: "BREAD-SOURDOUGH-800G", Name: "Sourdough Loaf 800g",
		LeadTimeDays: 1, CaseSize: 8, MinimumOrderQuantity: 8,
		ShelfLifeDays: 3, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 26, WeekendLift: 1.7, Noise: 0.20,
		FinalOnHand: -1,
		Note:        "3-day shelf life; watch the shelf_life constraint appear",
	},
	{
		SKU: "BAGEL-PLAIN-6PK", Name: "Plain Bagels, 6 pack",
		LeadTimeDays: 2, CaseSize: 12, MinimumOrderQuantity: 12,
		ShelfLifeDays: 3, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 22, WeekendLift: 1.55, Noise: 0.18,
		SupplyGapStart: 34, SupplyGapDays: 5,
		FinalOnHand: 14,
		Note:        "the worked example in the README: censored days plus case rounding",
	},
	{
		SKU: "CROISSANT-BUTTER-4PK", Name: "Butter Croissants, 4 pack",
		LeadTimeDays: 1, CaseSize: 6, MinimumOrderQuantity: 6,
		ShelfLifeDays: 2, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 13, WeekendLift: 2.1, Noise: 0.28,
		FinalOnHand: 4,
		Note:        "2-day shelf life against a 6-unit case: the tightest constraint in the set",
	},

	// --- Produce: perishable, weather-driven volatility ---------------------
	{
		SKU: "BANANA-LOOSE-KG", Name: "Bananas, loose (kg)",
		LeadTimeDays: 2, CaseSize: 20, MinimumOrderQuantity: 20,
		ShelfLifeDays: 7, ReviewPeriodDays: 1, TargetServiceLevel: 2.05,
		BaseDemand: 54, WeekendLift: 1.3, Noise: 0.15,
		FinalOnHand: -1,
	},
	{
		SKU: "AVOCADO-HASS-EA", Name: "Hass Avocado, each",
		LeadTimeDays: 3, CaseSize: 24, MinimumOrderQuantity: 24,
		ShelfLifeDays: 5, ReviewPeriodDays: 1, TargetServiceLevel: 2.05,
		BaseDemand: 33, WeekendLift: 1.6, Noise: 0.30,
		// Day 43 of 60, deliberately. The estimator's default window is the most
		// recent 28 days — days 32 to 59 — so a gap placed earlier in the
		// history would be generated, stored, and then never looked at. An
		// earlier version of this entry sat at day 22 and produced a demo where
		// the flagship censored-demand SKU showed zero censored days.
		SupplyGapStart: 43, SupplyGapDays: 5,
		// Pinned low so this one orders. It is the flagship censored-demand
		// example and an "ordering_is_inhibited" answer would bury the point.
		FinalOnHand: 60,
		Note:        "five-day supply gap inside the estimation window; the clearest censored-demand example in the set",
	},
	{
		SKU: "SPINACH-BABY-200G", Name: "Baby Spinach 200g",
		LeadTimeDays: 2, CaseSize: 12, MinimumOrderQuantity: 12,
		ShelfLifeDays: 4, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 16, WeekendLift: 1.2, Noise: 0.25,
		FinalOnHand: -1,
	},
	{
		SKU: "TOMATO-VINE-KG", Name: "Vine Tomatoes (kg)",
		LeadTimeDays: 2, CaseSize: 10, MinimumOrderQuantity: 10,
		ShelfLifeDays: 6, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 23, WeekendLift: 1.4, Noise: 0.22,
		FinalOnHand: -1,
	},

	// --- Meat and fish: expensive, tightly managed --------------------------
	{
		SKU: "CHICKEN-BREAST-500G", Name: "Chicken Breast Fillets 500g",
		LeadTimeDays: 2, CaseSize: 10, MinimumOrderQuantity: 10,
		ShelfLifeDays: 4, ReviewPeriodDays: 1, TargetServiceLevel: 2.05,
		BaseDemand: 20, WeekendLift: 1.65, Noise: 0.20,
		FinalOnHand: -1,
	},
	{
		SKU: "SALMON-FILLET-250G", Name: "Salmon Fillet 250g",
		LeadTimeDays: 3, CaseSize: 8, MinimumOrderQuantity: 8,
		ShelfLifeDays: 3, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 9, WeekendLift: 2.2, Noise: 0.35,
		FinalOnHand: -1,
		Note:        "strong weekend skew; day-of-week factors do real work here",
	},

	// --- Pantry: long lead times, big cases, vendor minimums ----------------
	{
		SKU: "PASTA-PENNE-500G", Name: "Penne Pasta 500g",
		LeadTimeDays: 5, CaseSize: 24, MinimumOrderQuantity: 48,
		ShelfLifeDays: 0, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 14, WeekendLift: 1.1, Noise: 0.18,
		// Just below the reorder point, so the raw order is small enough for the
		// 48-unit vendor minimum to lift it.
		FinalOnHand: 85,
		Note:        "48-unit vendor minimum; watch minimum_order_qty bind on a small order",
	},
	{
		SKU: "RICE-BASMATI-1KG", Name: "Basmati Rice 1kg",
		LeadTimeDays: 7, CaseSize: 12, MinimumOrderQuantity: 12,
		ShelfLifeDays: 0, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 9, WeekendLift: 1.15, Noise: 0.20,
		FinalOnHand: -1,
		Note:        "7-day lead time; safety stock scales with sqrt(lead + review)",
	},
	{
		SKU: "COFFEE-BEANS-1KG", Name: "Coffee Beans 1kg",
		LeadTimeDays: 7, CaseSize: 6, MinimumOrderQuantity: 12,
		ShelfLifeDays: 180, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 5, WeekendLift: 1.3, Noise: 0.30,
		FinalOnHand: -1,
	},

	// --- Drinks -------------------------------------------------------------
	{
		SKU: "BEER-IPA-6PK", Name: "Craft IPA, 6 pack",
		LeadTimeDays: 3, CaseSize: 4, MinimumOrderQuantity: 8,
		ShelfLifeDays: 0, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 16, WeekendLift: 2.6, Noise: 0.25,
		FinalOnHand: -1,
		Note:        "the sharpest weekend spike in the catalogue",
	},
	{
		SKU: "WATER-SPARKLING-12PK", Name: "Sparkling Water, 12 pack",
		LeadTimeDays: 4, CaseSize: 12, MinimumOrderQuantity: 12,
		ShelfLifeDays: 0, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 11, WeekendLift: 1.3, Noise: 0.20,
		// Deliberately overstocked: a pallet arrived by mistake.
		FinalOnHand: 900, FinalOnOrder: 0,
		Note: "badly overstocked; ordering_is_inhibited should be true and the order zero",
	},

	// --- Long tail ----------------------------------------------------------
	{
		SKU: "TRUFFLE-OIL-100ML", Name: "White Truffle Oil 100ml",
		LeadTimeDays: 10, CaseSize: 6, MinimumOrderQuantity: 6,
		ShelfLifeDays: 0, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		// Roughly one unit every three days. The interesting property of a
		// long-tail SKU is that most days are a genuine zero, and zero is data —
		// not a missing reading.
		BaseDemand: 0.32, WeekendLift: 1.8, Noise: 0.9,
		FinalOnHand: 4,
		Note:        "slow-moving long tail; mostly zero-demand days, tiny order or none",
	},

	// --- Newly listed -------------------------------------------------------
	{
		SKU: "KIMCHI-JAR-400G", Name: "Kimchi 400g",
		LeadTimeDays: 5, CaseSize: 12, MinimumOrderQuantity: 12,
		ShelfLifeDays: 60, ReviewPeriodDays: 1, TargetServiceLevel: 1.65,
		BaseDemand: 7, WeekendLift: 1.3, Noise: 0.25,
		HistoryDays: 1,
		FinalOnHand: 6,
		Note:        "listed yesterday; one day of history, so /recommendation returns 422",
	},
}

// weekdayShape is the base day-of-week pattern applied to every SKU before its
// own WeekendLift, expressed as multipliers on a typical day.
//
// It encodes a real grocery rhythm rather than a flat line: Monday is quiet
// after the weekend shop, Thursday and Friday build toward it. Without something
// like this the day-of-week factors the estimator computes would all come out at
// 1.0 and that part of the model would be invisible in the demo.
var weekdayShape = map[time.Weekday]float64{
	time.Monday:    0.88,
	time.Tuesday:   0.92,
	time.Wednesday: 0.97,
	time.Thursday:  1.06,
	time.Friday:    1.24,
	time.Saturday:  1.00, // WeekendLift is applied on top of this
	time.Sunday:    1.00,
}

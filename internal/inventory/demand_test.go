package inventory

import (
	"errors"
	"math"
	"testing"
	"time"
)

// Fixed anchor so every test is deterministic. 2026-06-01 is a Monday, which
// makes weekday arithmetic in the fixtures readable.
var anchor = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// floatEq compares with a tolerance. Direct == on floats is a bug waiting to
// happen: 0.1+0.2 != 0.3 in IEEE 754, and our pipeline runs values through
// sqrt and repeated multiplication.
func floatEq(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// flat builds n consecutive days each selling the same number of units.
func flat(n, units int) []SalesDay {
	days := make([]SalesDay, n)
	for i := range days {
		days[i] = SalesDay{Date: anchor.AddDate(0, 0, i), UnitsSold: units}
	}
	return days
}

func TestComputeDemandStats_Errors(t *testing.T) {
	// Table-driven: each case is a row in a slice of anonymous structs, and the
	// loop is the test body. This is the dominant Go test pattern — adding a case
	// is adding a line of data, not a new function.
	tests := []struct {
		name    string
		history []SalesDay
		want    error
	}{
		{
			name:    "empty history",
			history: nil,
			want:    ErrEmptySalesHistory,
		},
		{
			name:    "single data point cannot yield a variance",
			history: flat(1, 10),
			want:    ErrInsufficientHistory,
		},
		{
			name: "every day censored leaves nothing to estimate from",
			history: []SalesDay{
				{Date: anchor, UnitsSold: 12, StockOutOccurred: true},
				{Date: anchor.AddDate(0, 0, 1), UnitsSold: 9, StockOutOccurred: true},
				{Date: anchor.AddDate(0, 0, 2), UnitsSold: 15, StockOutOccurred: true},
			},
			want: ErrInsufficientHistory,
		},
		{
			name: "one uncensored day among stockouts is still insufficient",
			history: []SalesDay{
				{Date: anchor, UnitsSold: 12, StockOutOccurred: true},
				{Date: anchor.AddDate(0, 0, 1), UnitsSold: 9},
				{Date: anchor.AddDate(0, 0, 2), UnitsSold: 15, StockOutOccurred: true},
			},
			want: ErrInsufficientHistory,
		},
	}

	for _, tt := range tests {
		// t.Run creates a subtest with its own name, so a failure reports
		// "TestComputeDemandStats_Errors/empty_history" rather than a line number.
		t.Run(tt.name, func(t *testing.T) {
			_, err := ComputeDemandStats(tt.history, DefaultHistoryWindowDays, DefaultAlpha)
			// errors.Is unwraps, so this keeps working if we later wrap the
			// sentinel with %w for extra context.
			if !errors.Is(err, tt.want) {
				t.Fatalf("got error %v, want %v", err, tt.want)
			}
		})
	}
}

func TestComputeDemandStats_ConstantDemand(t *testing.T) {
	// 28 days of exactly 10 units. Every derived quantity is hand-checkable:
	// the mean is 10, every weekday factor is 10/10 = 1.0, and because no
	// observation deviates from the level the smoothed variance never leaves 0.
	stats, err := ComputeDemandStats(flat(28, 10), 28, DefaultAlpha)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !floatEq(stats.AverageDailyDemand, 10, 1e-9) {
		t.Errorf("AverageDailyDemand = %v, want 10", stats.AverageDailyDemand)
	}
	if !floatEq(stats.DemandStdDev, 0, 1e-9) {
		t.Errorf("DemandStdDev = %v, want 0 for a perfectly flat series", stats.DemandStdDev)
	}
	if stats.DataPoints != 28 {
		t.Errorf("DataPoints = %d, want 28", stats.DataPoints)
	}
	if stats.CensoredDays != 0 {
		t.Errorf("CensoredDays = %d, want 0", stats.CensoredDays)
	}
	for wd := time.Sunday; wd <= time.Saturday; wd++ {
		if !floatEq(stats.DayOfWeekFactors[wd], 1.0, 1e-9) {
			t.Errorf("factor[%v] = %v, want 1.0", wd, stats.DayOfWeekFactors[wd])
		}
	}
}

func TestComputeDemandStats_ExcludesCensoredDays(t *testing.T) {
	// The central claim of the censored-demand design, stated as a test.
	//
	// Twelve days at 20 units, then two stockout days recording 5 units each.
	// Those 5s are not demand — they are what we managed to sell before running
	// out. Including them would drag the mean toward 17.9; excluding them keeps
	// it at 20, which is what the uncensored days actually observed.
	history := flat(12, 20)
	history = append(history,
		SalesDay{Date: anchor.AddDate(0, 0, 12), UnitsSold: 5, StockOutOccurred: true},
		SalesDay{Date: anchor.AddDate(0, 0, 13), UnitsSold: 5, StockOutOccurred: true},
	)

	stats, err := ComputeDemandStats(history, 28, DefaultAlpha)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !floatEq(stats.AverageDailyDemand, 20, 1e-9) {
		t.Errorf("AverageDailyDemand = %v, want 20; censored days appear to have "+
			"been included, which biases demand downward", stats.AverageDailyDemand)
	}
	if stats.DataPoints != 12 {
		t.Errorf("DataPoints = %d, want 12 uncensored", stats.DataPoints)
	}
	if stats.CensoredDays != 2 {
		t.Errorf("CensoredDays = %d, want 2", stats.CensoredDays)
	}

	// Guard the counterfactual explicitly so the test states the bug it prevents.
	const naiveMeanIncludingStockouts = (12*20 + 2*5) / 14.0 // 17.857...
	if floatEq(stats.AverageDailyDemand, naiveMeanIncludingStockouts, 0.01) {
		t.Error("mean matches the naive all-days average; censoring is not being applied")
	}
}

func TestComputeDemandStats_WeekdaySeasonality(t *testing.T) {
	// Four weeks where Saturday and Sunday sell 18 and weekdays sell 10.
	var history []SalesDay
	for i := 0; i < 28; i++ {
		d := anchor.AddDate(0, 0, i)
		units := 10
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			units = 18
		}
		history = append(history, SalesDay{Date: d, UnitsSold: units})
	}

	stats, err := ComputeDemandStats(history, 28, DefaultAlpha)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Overall mean = (20*10 + 8*18)/28 = 344/28 = 12.2857
	const overall = 344.0 / 28.0

	t.Run("weekend factor exceeds 1", func(t *testing.T) {
		got := stats.DayOfWeekFactors[time.Saturday]
		want := 18.0 / overall // 1.465
		if !floatEq(got, want, 1e-9) {
			t.Errorf("Saturday factor = %v, want %v", got, want)
		}
	})

	t.Run("weekday factor falls below 1", func(t *testing.T) {
		got := stats.DayOfWeekFactors[time.Tuesday]
		want := 10.0 / overall // 0.814
		if !floatEq(got, want, 1e-9) {
			t.Errorf("Tuesday factor = %v, want %v", got, want)
		}
	})

	t.Run("deseasonalizing removes structure from the variance", func(t *testing.T) {
		// This is the point of deseasonalizing before smoothing. The raw series
		// swings 10 -> 18, but that swing is predictable structure, not
		// uncertainty. Once divided out, every day maps to the same typical-day
		// value and sigma collapses to zero — so this SKU earns no safety stock.
		if !floatEq(stats.DemandStdDev, 0, 1e-9) {
			t.Errorf("DemandStdDev = %v, want ~0; a perfectly periodic series "+
				"should carry no residual noise after deseasonalizing", stats.DemandStdDev)
		}
	})

	t.Run("ExpectedDemandOn reapplies the factor", func(t *testing.T) {
		// Round-trip: typical-day level scaled back up to a Saturday should
		// recover the 18 units we actually observed on Saturdays.
		if got := stats.ExpectedDemandOn(time.Saturday); !floatEq(got, 18, 1e-6) {
			t.Errorf("ExpectedDemandOn(Saturday) = %v, want 18", got)
		}
		if got := stats.ExpectedDemandOn(time.Tuesday); !floatEq(got, 10, 1e-6) {
			t.Errorf("ExpectedDemandOn(Tuesday) = %v, want 10", got)
		}
	})
}

func TestComputeDemandStats_RecencyWeighting(t *testing.T) {
	// A level shift: 14 days at 10 units, then 14 days at 30 units.
	//
	// A flat average would report 20. Exponential smoothing must land above that
	// because the recent fortnight carries more weight — that is the entire
	// reason for choosing an EWMA over a simple mean.
	var history []SalesDay
	for i := 0; i < 14; i++ {
		history = append(history, SalesDay{Date: anchor.AddDate(0, 0, i), UnitsSold: 10})
	}
	for i := 14; i < 28; i++ {
		history = append(history, SalesDay{Date: anchor.AddDate(0, 0, i), UnitsSold: 30})
	}

	stats, err := ComputeDemandStats(history, 28, DefaultAlpha)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const flatAverage = 20.0
	if stats.AverageDailyDemand <= flatAverage {
		t.Errorf("AverageDailyDemand = %v, want > %v; the estimate is not "+
			"weighting recent days more heavily", stats.AverageDailyDemand, flatAverage)
	}
	// It must also stay below the recent level: alpha=0.10 is deliberately not
	// so reactive that it abandons the older half of the window outright.
	if stats.AverageDailyDemand >= 30 {
		t.Errorf("AverageDailyDemand = %v, want < 30; alpha appears far too "+
			"reactive", stats.AverageDailyDemand)
	}
	// A level shift is genuine unexplained movement, so unlike the seasonal case
	// it must produce non-zero sigma and therefore real safety stock.
	if stats.DemandStdDev <= 0 {
		t.Errorf("DemandStdDev = %v, want > 0 after an unexplained level shift",
			stats.DemandStdDev)
	}
}

func TestComputeDemandStats_HistoryWindow(t *testing.T) {
	// 60 days supplied, 28-day window requested: only the most recent 28 count.
	stats, err := ComputeDemandStats(flat(60, 10), 28, DefaultAlpha)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.DataPoints != 28 {
		t.Errorf("DataPoints = %d, want 28; the history window is not being applied",
			stats.DataPoints)
	}
}

func TestComputeDemandStats_DoesNotMutateCallerSlice(t *testing.T) {
	// Go slices are views over a shared backing array, so an in-place sort inside
	// the function would silently reorder the caller's data. This is the sharpest
	// difference from Python lists and worth pinning down.
	history := []SalesDay{
		{Date: anchor.AddDate(0, 0, 2), UnitsSold: 30},
		{Date: anchor, UnitsSold: 10},
		{Date: anchor.AddDate(0, 0, 1), UnitsSold: 20},
	}
	before := []int{30, 10, 20}

	if _, err := ComputeDemandStats(history, 28, DefaultAlpha); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i, want := range before {
		if history[i].UnitsSold != want {
			t.Errorf("caller slice was mutated at index %d: got %d, want %d",
				i, history[i].UnitsSold, want)
		}
	}
}

func TestComputeDemandStats_ParameterFallbacks(t *testing.T) {
	// Out-of-range tuning parameters fall back to defaults rather than erroring:
	// a bad alpha is a config mistake, not a reason to refuse to replenish.
	tests := []struct {
		name   string
		window int
		alpha  float64
	}{
		{name: "zero window falls back to default", window: 0, alpha: DefaultAlpha},
		{name: "negative window falls back to default", window: -5, alpha: DefaultAlpha},
		{name: "zero alpha falls back to default", window: 28, alpha: 0},
		{name: "alpha above one falls back to default", window: 28, alpha: 1.7},
		{name: "negative alpha falls back to default", window: 28, alpha: -0.3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats, err := ComputeDemandStats(flat(28, 10), tt.window, tt.alpha)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !floatEq(stats.AverageDailyDemand, 10, 1e-9) {
				t.Errorf("AverageDailyDemand = %v, want 10", stats.AverageDailyDemand)
			}
		})
	}
}

func TestComputeDayOfWeekFactors_ThinData(t *testing.T) {
	t.Run("weekday with a single observation gets a neutral factor", func(t *testing.T) {
		// One Saturday in the window cannot distinguish "Saturdays are busy" from
		// "that Saturday had a promotion", so we decline to draw a conclusion.
		observed := []SalesDay{
			{Date: anchor, UnitsSold: 10},                   // Monday
			{Date: anchor.AddDate(0, 0, 5), UnitsSold: 100}, // Saturday, once
			{Date: anchor.AddDate(0, 0, 7), UnitsSold: 10},  // Monday
		}
		factors := computeDayOfWeekFactors(observed, 40)
		if got := factors[time.Saturday]; !floatEq(got, 1.0, 1e-9) {
			t.Errorf("Saturday factor = %v, want 1.0 from a single observation", got)
		}
	})

	t.Run("extreme factors are clamped", func(t *testing.T) {
		// Two Saturdays at 100 against a mean of 10 implies a factor of 10.0,
		// which would then divide the demand estimate by 10. Clamp holds it at 2.0.
		observed := []SalesDay{
			{Date: anchor.AddDate(0, 0, 5), UnitsSold: 100},  // Saturday
			{Date: anchor.AddDate(0, 0, 12), UnitsSold: 100}, // Saturday
			{Date: anchor, UnitsSold: 1},                     // Monday
			{Date: anchor.AddDate(0, 0, 7), UnitsSold: 1},    // Monday
		}
		factors := computeDayOfWeekFactors(observed, 10)
		if got := factors[time.Saturday]; !floatEq(got, 2.0, 1e-9) {
			t.Errorf("Saturday factor = %v, want clamped to 2.0", got)
		}
		if got := factors[time.Monday]; !floatEq(got, 0.5, 1e-9) {
			t.Errorf("Monday factor = %v, want clamped to 0.5", got)
		}
	})

	t.Run("zero mean yields neutral factors without dividing by zero", func(t *testing.T) {
		factors := computeDayOfWeekFactors([]SalesDay{{Date: anchor}}, 0)
		for wd := time.Sunday; wd <= time.Saturday; wd++ {
			if !floatEq(factors[wd], 1.0, 1e-9) {
				t.Errorf("factor[%v] = %v, want 1.0", wd, factors[wd])
			}
		}
	})
}

func TestComputeDemandStats_AllZeroSales(t *testing.T) {
	// A listed-but-never-selling SKU. This must not divide by zero or produce
	// NaN — it must simply report no demand, which downstream means no order.
	stats, err := ComputeDemandStats(flat(28, 0), 28, DefaultAlpha)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !floatEq(stats.AverageDailyDemand, 0, 1e-9) {
		t.Errorf("AverageDailyDemand = %v, want 0", stats.AverageDailyDemand)
	}
	if math.IsNaN(stats.AverageDailyDemand) || math.IsNaN(stats.DemandStdDev) {
		t.Fatal("produced NaN from an all-zero series")
	}
}

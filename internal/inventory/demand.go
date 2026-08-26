package inventory

import (
	"math"
	"sort"
	"time"
)

// DefaultHistoryWindowDays is the lookback for demand estimation.
const DefaultHistoryWindowDays = 28

// DefaultAlpha is the exponential smoothing constant.
//
// Choice of 0.10 comes from the standard relation alpha = 2/(N+1), which makes an
// EWMA behave like a simple moving average of N periods. N=19 gives alpha≈0.10,
// and the half-life is ln(0.5)/ln(1-alpha) ≈ 6.6 days.
//
// Why not higher (0.3, half-life ~2 days)? Grocery demand is noisy at the daily
// level. A reactive alpha chases noise, and because demand feeds safety stock,
// an over-reactive estimate oscillates order sizes week to week — operators lose
// trust in a system whose recommendation swings 40% on a quiet Tuesday.
//
// Why not lower (0.03)? Then a genuine demand shift (a competitor closing, a
// promotion ending) takes a month to show up, and we stock out through the
// transition.
//
// 0.10 keeps ~2/3 of the weight inside the most recent 10 days while retaining a
// tail out to the full 28-day window.
const DefaultAlpha = 0.10

// ComputeDemandStats estimates demand from sales history.
//
// The pipeline is: censor-filter -> day-of-week factors -> deseasonalize ->
// exponentially-weighted mean and variance.
//
// # Censored demand
//
// A day that sold 12 units and ended at zero stock is not evidence that demand
// was 12. It is evidence that demand was AT LEAST 12 — the true figure is
// unobservable because we ran out of product to sell. Statisticians call this a
// right-censored observation.
//
// This implementation EXCLUDES stockout days from both the mean and the variance
// rather than uplifting them by a factor.
//
// Why exclusion over uplift: an uplift (e.g. treat the observation as 1.3x) needs
// a multiplier, and any multiplier we pick is a guess about how much demand we
// failed to observe. That guess has no grounding in the data and it silently
// fabricates variance, which then inflates safety stock through the sigma term.
// Exclusion throws away information but never invents it. The bias direction is
// also the safe one: the surviving days are the days we could serve, so the mean
// is unbiased for uncensored demand rather than dragged downward by truncated
// days.
//
// The cost of exclusion is real and worth stating: for a chronically
// out-of-stock SKU most days are censored, and we end up estimating from very
// few points. DataPoints is returned so callers can judge that, and
// ErrInsufficientHistory fires when fewer than 2 usable days survive.
//
// The rigorous alternative is a censored-regression / Tobit estimator or the
// Nahmias–Lau method, which model the unobserved tail explicitly. Those need a
// distributional assumption and an iterative solver, which is more machinery than
// this service justifies.
func ComputeDemandStats(salesHistory []SalesDay, historyWindowDays int, alpha float64) (*DemandStats, error) {
	if len(salesHistory) == 0 {
		return nil, ErrEmptySalesHistory
	}
	if historyWindowDays <= 0 {
		historyWindowDays = DefaultHistoryWindowDays
	}
	if alpha <= 0 || alpha > 1 {
		alpha = DefaultAlpha
	}

	// Work on a copy: callers own the slice they passed and a sort would mutate it.
	days := make([]SalesDay, len(salesHistory))
	copy(days, salesHistory)
	sort.Slice(days, func(i, j int) bool { return days[i].Date.Before(days[j].Date) })

	// Trim to the history window, counting back from the most recent observation.
	cutoff := days[len(days)-1].Date.AddDate(0, 0, -historyWindowDays)
	windowStart := 0
	for i, d := range days {
		if d.Date.After(cutoff) {
			windowStart = i
			break
		}
	}
	days = days[windowStart:]

	// Drop censored observations. See the doc comment above for why.
	observed := make([]SalesDay, 0, len(days))
	censored := 0
	for _, d := range days {
		if d.StockOutOccurred {
			censored++
			continue
		}
		observed = append(observed, d)
	}
	if len(observed) < 2 {
		return nil, ErrInsufficientHistory
	}

	// Pass 1: a flat mean, used only as the denominator for seasonality factors.
	var flatTotal float64
	for _, d := range observed {
		flatTotal += float64(d.UnitsSold)
	}
	flatMean := flatTotal / float64(len(observed))

	factors := computeDayOfWeekFactors(observed, flatMean)

	// Pass 2: deseasonalize, then smooth exponentially.
	//
	// Deseasonalizing first matters. If we smoothed the raw series, the estimate
	// would depend on which weekday happened to be last — a Saturday-heavy item
	// would read high every Sunday morning and low every Wednesday. Dividing each
	// observation by its weekday factor puts every day on a comparable "typical
	// day" scale before the smoother sees it.
	level := deseasonalize(observed[0], factors)
	variance := 0.0

	for _, d := range observed[1:] {
		x := deseasonalize(d, factors)
		residual := x - level
		// EWMA of squared residuals gives a smoothed variance that tracks the
		// same recency weighting as the level. Standard EWMA-variance recursion.
		variance = (1 - alpha) * (variance + alpha*residual*residual)
		level = level + alpha*residual
	}

	if level < 0 {
		level = 0
	}

	return &DemandStats{
		AverageDailyDemand: level,
		DemandStdDev:       math.Sqrt(variance),
		DayOfWeekFactors:   factors,
		DataPoints:         len(observed),
		CensoredDays:       censored,
	}, nil
}

// deseasonalize converts one observation to the "typical day" scale by dividing
// out its weekday factor.
func deseasonalize(d SalesDay, factors map[time.Weekday]float64) float64 {
	f := factors[d.Date.Weekday()]
	if f <= 0 {
		return float64(d.UnitsSold)
	}
	return float64(d.UnitsSold) / f
}

// minObservationsPerWeekday is how many samples a weekday needs before we trust
// its factor. With one Saturday in the window we cannot distinguish "Saturdays
// are busy" from "that Saturday had a promotion", so we fall back to neutral.
const minObservationsPerWeekday = 2

// computeDayOfWeekFactors returns a multiplier per weekday: the ratio of that
// weekday's mean demand to overall mean demand. Saturday at 1.4 means Saturdays
// run 40% above a typical day.
func computeDayOfWeekFactors(observed []SalesDay, overallMean float64) map[time.Weekday]float64 {
	factors := make(map[time.Weekday]float64, 7)

	if overallMean <= 0 {
		for wd := time.Sunday; wd <= time.Saturday; wd++ {
			factors[wd] = 1.0
		}
		return factors
	}

	totals := make(map[time.Weekday]float64, 7)
	counts := make(map[time.Weekday]int, 7)
	for _, d := range observed {
		wd := d.Date.Weekday()
		totals[wd] += float64(d.UnitsSold)
		counts[wd]++
	}

	for wd := time.Sunday; wd <= time.Saturday; wd++ {
		n := counts[wd]
		if n < minObservationsPerWeekday {
			factors[wd] = 1.0
			continue
		}
		f := (totals[wd] / float64(n)) / overallMean
		// Clamp. An unclamped factor from a sparse window can be extreme, and it
		// divides the demand estimate — a factor near zero would explode it.
		factors[wd] = math.Min(2.0, math.Max(0.5, f))
	}
	return factors
}

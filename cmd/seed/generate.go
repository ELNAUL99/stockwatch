package main

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/ELNAUL99/stockwatch/internal/inventory"
)

// generated is one SKU's synthetic history plus the closing stock position.
type generated struct {
	Product *inventory.Product
	Days    []inventory.SalesDay

	// Diagnostics, printed by the seeder so the demo explains itself.
	TrueDemandTotal int // what customers actually wanted
	SoldTotal       int // what we managed to sell
	CensoredDays    int
}

// generate synthesises one SKU's history by simulating a store.
//
// The important design decision: stockouts are not written directly. The
// simulation tracks on-hand stock, sells against it, and records a censored day
// only when the shelf genuinely empties mid-day. Faking the flag would produce
// data where the stockouts are wherever I put them; simulating produces data
// where they are wherever the supply chain failed, which is the thing the
// estimator has to cope with.
//
// This also means the seed data contains the ground truth — the demand we would
// have seen with infinite stock — which is what makes the README's before/after
// comparison honest rather than illustrative.
func generate(s seedSKU, days int, endDate time.Time, rng *rand.Rand) generated {
	product := &inventory.Product{
		SKU:                  s.SKU,
		Name:                 s.Name,
		LeadTimeDays:         s.LeadTimeDays,
		MinimumOrderQuantity: s.MinimumOrderQuantity,
		CaseSize:             s.CaseSize,
		ReviewPeriodDays:     s.ReviewPeriodDays,
		ShelfLifeDays:        s.ShelfLifeDays,
		TargetServiceLevel:   s.TargetServiceLevel,
	}

	historyDays := days
	if s.HistoryDays > 0 && s.HistoryDays < days {
		historyDays = s.HistoryDays
	}

	// The window ends on endDate, so the newest row is always recent relative to
	// "now". A seed whose newest day is a month old would fall outside the
	// estimator's history window and every SKU would return 422 — which is a
	// tediously common way for a demo to be broken.
	start := endDate.AddDate(0, 0, -(historyDays - 1))

	// Open with a sensible position: enough to cover lead time plus a buffer.
	onHand := int(math.Ceil(s.BaseDemand * float64(s.LeadTimeDays+2) * 1.4))
	if onHand < s.CaseSize {
		onHand = s.CaseSize
	}

	// Outstanding deliveries, keyed by the day index they land on.
	incoming := make(map[int]int)

	out := generated{Product: product, Days: make([]inventory.SalesDay, 0, historyDays)}

	for i := 0; i < historyDays; i++ {
		date := start.AddDate(0, 0, i)

		if qty, ok := incoming[i]; ok {
			delete(incoming, i)
			if inSupplyGap(s, i) {
				// A supplier outage stops deliveries, not just orders. Pushing
				// stock already in transit out to the far side of the gap is
				// what actually empties the shelf — blocking only new orders
				// leaves whatever was ordered beforehand arriving on schedule,
				// which barely dents a buffer and produces a single token
				// stockout instead of a genuine run.
				resume := s.SupplyGapStart + s.SupplyGapDays
				incoming[resume] += qty
			} else {
				onHand += qty
			}
		}

		trueDemand := demandFor(s, date, i, historyDays, rng)

		// Sell what we can. This is where censoring is created: if demand
		// exceeds stock, the recorded figure is the stock we had, not the demand
		// we saw — and nobody counts the customers who found an empty shelf.
		sold := trueDemand
		censored := false
		if sold > onHand {
			sold = onHand
			// Censored whenever stock truncated demand — including when the
			// shelf was empty at opening and sold is 0. That day is the most
			// censored observation there is: the entire day's demand went
			// unseen. Marking only the partial days would leave the fully empty
			// ones looking like genuine zero-demand days, and the estimator
			// would average those fabricated zeros straight into the mean.
			censored = true
		}
		onHand -= sold

		out.Days = append(out.Days, inventory.SalesDay{
			Date:             date,
			UnitsSold:        sold,
			StockOutOccurred: censored,
		})
		out.TrueDemandTotal += trueDemand
		out.SoldTotal += sold
		if censored {
			out.CensoredDays++
		}

		// Reorder, unless this SKU is inside its deliberate supply gap.
		if inSupplyGap(s, i) {
			continue
		}
		if i%replenishEvery == 0 {
			// A crude order-up-to policy — deliberately not the one under test.
			// Using the real Recommend here would make the seed data a fixed
			// point of the algorithm, and the demo would prove only that the
			// algorithm agrees with itself.
			// 1.15 rather than a fatter multiple: a store carrying 50% more than
			// its cover period never runs out of anything, which would make the
			// whole censored-demand story invisible in the seed data. Real
			// grocery replenishment runs far tighter than that.
			target := s.BaseDemand * float64(s.LeadTimeDays+replenishEvery) * 1.15
			position := onHand + pendingTotal(incoming)
			need := int(math.Ceil(target - float64(position)))
			if need > 0 {
				if s.CaseSize > 1 {
					need = ((need + s.CaseSize - 1) / s.CaseSize) * s.CaseSize
				}
				incoming[i+s.LeadTimeDays] = incoming[i+s.LeadTimeDays] + need
			}
		}
	}

	// Closing position: simulated, unless the catalogue pins it to demonstrate a
	// particular decision.
	if s.FinalOnHand >= 0 {
		product.OnHandUnits = s.FinalOnHand
	} else {
		product.OnHandUnits = onHand
	}
	product.OnOrderUnits = s.FinalOnOrder
	if product.OnOrderUnits == 0 && s.FinalOnHand < 0 {
		product.OnOrderUnits = pendingTotal(incoming)
	}

	return out
}

// replenishEvery is how often the simulated buyer places an order. Twice a week
// is typical for a grocery format of this size.
const replenishEvery = 3

func pendingTotal(incoming map[int]int) int {
	total := 0
	for _, qty := range incoming {
		total += qty
	}
	return total
}

func inSupplyGap(s seedSKU, dayIndex int) bool {
	if s.SupplyGapDays == 0 {
		return false
	}
	return dayIndex >= s.SupplyGapStart && dayIndex < s.SupplyGapStart+s.SupplyGapDays
}

// demandFor returns true customer demand for one day.
//
// True demand, not observed sales — the caller truncates it against stock. Every
// factor is multiplicative on the base rate: weekday shape, weekend lift, a
// linear trend across the window, and lognormal noise.
func demandFor(s seedSKU, date time.Time, dayIndex, totalDays int, rng *rand.Rand) int {
	demand := s.BaseDemand

	demand *= weekdayShape[date.Weekday()]
	if wd := date.Weekday(); wd == time.Saturday || wd == time.Sunday {
		demand *= s.WeekendLift
	}

	// Linear trend across the window, centred so the mean is roughly BaseDemand
	// rather than drifting the whole series upward.
	if s.Trend != 0 && totalDays > 1 {
		progress := float64(dayIndex) / float64(totalDays-1)
		demand *= 1 + s.Trend*(progress-0.5)
	}

	// Lognormal noise rather than normal. Demand cannot go negative, and real
	// sales are right-skewed: a quiet day is bounded below by zero, while a good
	// day has no ceiling. Normal noise on a low-volume SKU would produce
	// negative demand that then has to be clamped, which quietly distorts the
	// mean upward.
	if s.Noise > 0 {
		demand *= math.Exp(rng.NormFloat64()*s.Noise - s.Noise*s.Noise/2)
	}

	// Poisson-like rounding for slow movers. A SKU selling 0.32/day must produce
	// mostly zeros with the occasional 1 or 2, not 0.32 rounded to 0 every
	// single day — which would make it look like a dead SKU rather than a slow
	// one, and would hide the long-tail case entirely.
	if demand < 2 {
		return poisson(demand, rng)
	}
	return int(math.Round(demand))
}

// poisson draws from a Poisson distribution by Knuth's method.
//
// Fine for the small rates it is used with here; it loops proportionally to the
// value drawn, so it would be the wrong algorithm for a large lambda.
func poisson(lambda float64, rng *rand.Rand) int {
	if lambda <= 0 {
		return 0
	}
	limit := math.Exp(-lambda)
	product := 1.0
	k := 0
	for {
		product *= rng.Float64()
		if product <= limit {
			return k
		}
		k++
		if k > 50 { // guard against a pathological rate
			return k
		}
	}
}

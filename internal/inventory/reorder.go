package inventory

import (
	"fmt"
	"math"
)

// Constraint names. Exported so the HTTP layer and tests refer to the same
// strings rather than each spelling their own literal.
const (
	ConstraintShelfLife    = "shelf_life"
	ConstraintCaseSize     = "case_size"
	ConstraintMinimumOrder = "minimum_order_qty"
)

// Recommend produces a replenishment decision for one product.
//
//	safety_stock   = z * sigma * sqrt(lead_time + review_period)
//	reorder_point  = avg_daily_demand * lead_time + safety_stock
//	target_level   = avg_daily_demand * (lead_time + review_period) + safety_stock
//	order_units    = target_level - (on_hand + on_order)
//
// The reorder point is a trigger: while position sits above it we have enough
// cover for the lead time and do nothing. Once it is breached we order up to the
// target level, which additionally covers the review period — the gap until we
// next get to look at this SKU.
//
// asOf is not needed by the arithmetic; it is threaded in so the caller's clock
// is the single source of "now" and tests stay deterministic. Phase 3 uses it for
// expiry-window logic.
func Recommend(p *Product, stats *DemandStats) (*Recommendation, error) {
	if p == nil {
		return nil, ErrProductNotFound
	}
	if stats == nil {
		return nil, ErrInvalidDemandStats
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}

	coverDays := float64(p.LeadTimeDays + p.ReviewPeriodDays)

	safetyStock := p.TargetServiceLevel * stats.DemandStdDev * math.Sqrt(coverDays)
	reorderPoint := stats.AverageDailyDemand*float64(p.LeadTimeDays) + safetyStock
	targetLevel := stats.AverageDailyDemand*coverDays + safetyStock

	position := float64(p.OnHandUnits + p.OnOrderUnits)

	rec := &Recommendation{
		SKU:                p.SKU,
		SafetyStock:        safetyStock,
		ReorderPoint:       reorderPoint,
		TargetLevel:        targetLevel,
		AverageDailyDemand: stats.AverageDailyDemand,
		CurrentOnHand:      p.OnHandUnits,
		CurrentOnOrder:     p.OnOrderUnits,
		Constraints:        []RecommendationConstraint{},
	}

	// Above the reorder point: we have cover, so hold. Compared as floats —
	// truncating the reorder point to an int here would order a day early on
	// every SKU whose reorder point has a fractional part.
	if position > reorderPoint {
		rec.OrderingIsInhibited = true
		rec.InhibitReason = fmt.Sprintf(
			"position %.0f units is above reorder point %.1f", position, reorderPoint)
		rec.roundDiagnostics()
		return rec, nil
	}

	// target_level - position. Negative is impossible here (position <=
	// reorderPoint <= targetLevel) but clamped defensively because a caller can
	// hand us a hand-built DemandStats.
	qty := math.Max(0, targetLevel-position)

	// Constraint order matters, and this ordering is a deliberate choice.
	//
	// Shelf life is a hard ceiling, so it applies to the raw quantity first.
	// Case rounding then runs, and can push us back over that ceiling — so shelf
	// life is re-checked afterwards, at whole-case granularity, and may drop us a
	// full case. Minimum-order-quantity runs last and is itself rounded to cases,
	// because a vendor minimum that is not a case multiple is unshippable.
	qty = applyShelfLife(rec, p, stats, qty)
	qty = applyCaseSize(rec, p, qty)
	qty = enforceShelfLifeAfterRounding(rec, p, stats, qty)
	qty = applyMinimumOrder(rec, p, qty)

	rec.RecommendedQuantity = int(math.Round(qty))
	rec.roundDiagnostics()
	return rec, nil
}

// roundDiagnostics trims the reported float fields to two decimals.
//
// This runs last, after every decision is made, so the ordering logic always
// uses full precision and only the human-facing figures are rounded. Two
// decimals on a unit count is past the point of meaning — nobody stocks 4.4187
// bagels — and it keeps the API payload readable and golden files stable.
func (r *Recommendation) roundDiagnostics() {
	round2 := func(f float64) float64 { return math.Round(f*100) / 100 }
	r.SafetyStock = round2(r.SafetyStock)
	r.ReorderPoint = round2(r.ReorderPoint)
	r.TargetLevel = round2(r.TargetLevel)
	r.AverageDailyDemand = round2(r.AverageDailyDemand)
}

// applyShelfLife caps the order at what can realistically sell before expiry.
// A 3-day-shelf-life item must never receive a 2-week order however strongly the
// safety-stock math argues for one — the excess is guaranteed waste, not cover.
func applyShelfLife(rec *Recommendation, p *Product, stats *DemandStats, qty float64) float64 {
	if p.ShelfLifeDays <= 0 {
		return qty // non-perishable
	}
	sellable := stats.AverageDailyDemand * float64(p.ShelfLifeDays)
	if qty <= sellable {
		return qty
	}
	rec.addConstraint(ConstraintShelfLife, qty, sellable, fmt.Sprintf(
		"capped at %.0f units: %d-day shelf life sells only %.1f units at %.2f units/day",
		math.Floor(sellable), p.ShelfLifeDays, sellable, stats.AverageDailyDemand))
	return sellable
}

// applyCaseSize rounds up to a whole case. Vendors do not break cases, so a
// partial-case recommendation is not an order anyone can place.
func applyCaseSize(rec *Recommendation, p *Product, qty float64) float64 {
	if p.CaseSize <= 1 || qty <= 0 {
		return qty
	}
	rounded := math.Ceil(qty/float64(p.CaseSize)) * float64(p.CaseSize)
	if rounded == qty {
		return qty // already an exact case boundary
	}
	rec.addConstraint(ConstraintCaseSize, qty, rounded, fmt.Sprintf(
		"rounded up %.1f to %.0f units (%.0f cases of %d)",
		qty, rounded, rounded/float64(p.CaseSize), p.CaseSize))
	return rounded
}

// enforceShelfLifeAfterRounding re-applies the expiry ceiling once case rounding
// has run. Rounding 10 units up to a 24-unit case can breach a shelf life that
// the pre-rounding check passed; when it does we step down a case rather than
// knowingly ship product to the bin. Dropping to zero is a legitimate outcome:
// for a short-life item with a large case, no orderable quantity sells in time.
func enforceShelfLifeAfterRounding(rec *Recommendation, p *Product, stats *DemandStats, qty float64) float64 {
	if p.ShelfLifeDays <= 0 || p.CaseSize <= 1 || qty <= 0 {
		return qty
	}
	sellable := stats.AverageDailyDemand * float64(p.ShelfLifeDays)
	if qty <= sellable {
		return qty
	}
	reduced := math.Floor(sellable/float64(p.CaseSize)) * float64(p.CaseSize)
	rec.addConstraint(ConstraintShelfLife, qty, reduced, fmt.Sprintf(
		"reduced %.0f to %.0f units: a full case of %d exceeds the %.1f units sellable within %d-day shelf life",
		qty, reduced, p.CaseSize, sellable, p.ShelfLifeDays))
	return reduced
}

// applyMinimumOrder lifts a small order to the vendor floor, rounded up to a
// whole case so the result stays shippable. A zero quantity is left at zero —
// "order nothing" must not become "order the minimum".
func applyMinimumOrder(rec *Recommendation, p *Product, qty float64) float64 {
	if p.MinimumOrderQuantity <= 0 || qty <= 0 || qty >= float64(p.MinimumOrderQuantity) {
		return qty
	}
	floor := float64(p.MinimumOrderQuantity)
	if p.CaseSize > 1 {
		floor = math.Ceil(floor/float64(p.CaseSize)) * float64(p.CaseSize)
	}
	rec.addConstraint(ConstraintMinimumOrder, qty, floor, fmt.Sprintf(
		"raised %.0f to %.0f units to meet vendor minimum of %d",
		qty, floor, p.MinimumOrderQuantity))
	return floor
}

func (r *Recommendation) addConstraint(name string, from, to float64, reason string) {
	r.Constraints = append(r.Constraints, RecommendationConstraint{
		Name:             name,
		OriginalQuantity: int(math.Round(from)),
		FinalQuantity:    int(math.Round(to)),
		Reason:           reason,
	})
}

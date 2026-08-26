package inventory

import (
	"errors"
	"testing"
	"time"
)

// steadyStats is a hand-built DemandStats with round numbers, so every expected
// value in this file can be verified with arithmetic rather than by trusting the
// demand estimator. Decoupling the two also means a change to the smoothing
// constant cannot break the reorder tests.
//
//	avg = 10/day, sigma = 0
//	safety_stock  = z * 0 * sqrt(...)      = 0
//	reorder_point = 10 * lead                + 0
//	target_level  = 10 * (lead + review)     + 0
func steadyStats() *DemandStats {
	return &DemandStats{
		AverageDailyDemand: 10,
		DemandStdDev:       0,
		DayOfWeekFactors:   map[time.Weekday]float64{},
		DataPoints:         28,
	}
}

// baseProduct: lead 3, review 1 => reorder_point 30, target_level 40.
func baseProduct() *Product {
	return &Product{
		SKU:                "TEST-001",
		Name:               "Test Item",
		LeadTimeDays:       3,
		ReviewPeriodDays:   1,
		CaseSize:           1,
		TargetServiceLevel: 1.65,
	}
}

func TestRecommend_CoreArithmetic(t *testing.T) {
	// sigma = 2 so safety stock is non-zero and the formula is exercised in full:
	//   safety_stock  = 1.65 * 2 * sqrt(3+1) = 1.65 * 2 * 2 = 6.6
	//   reorder_point = 10*3 + 6.6                          = 36.6
	//   target_level  = 10*4 + 6.6                          = 46.6
	//   order         = 46.6 - 10                           = 36.6 -> 37 units
	stats := steadyStats()
	stats.DemandStdDev = 2

	p := baseProduct()
	p.OnHandUnits = 10

	rec, err := Recommend(p, stats)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !floatEq(rec.SafetyStock, 6.6, 1e-9) {
		t.Errorf("SafetyStock = %v, want 6.6", rec.SafetyStock)
	}
	if !floatEq(rec.ReorderPoint, 36.6, 1e-9) {
		t.Errorf("ReorderPoint = %v, want 36.6", rec.ReorderPoint)
	}
	if !floatEq(rec.TargetLevel, 46.6, 1e-9) {
		t.Errorf("TargetLevel = %v, want 46.6", rec.TargetLevel)
	}
	if rec.RecommendedQuantity != 37 {
		t.Errorf("RecommendedQuantity = %d, want 37", rec.RecommendedQuantity)
	}
	if rec.OrderingIsInhibited {
		t.Error("OrderingIsInhibited = true, want false")
	}
}

func TestRecommend_InhibitBoundary(t *testing.T) {
	// The reorder point is 30. The rule is "do not order if position > reorder
	// point", so 30 must still order and 31 must not. Off-by-one here is the
	// difference between ordering a day early on every SKU in the catalogue.
	tests := []struct {
		name          string
		onHand        int
		onOrder       int
		wantInhibited bool
		wantQty       int
	}{
		{name: "far below reorder point", onHand: 0, wantInhibited: false, wantQty: 40},
		{name: "just below reorder point", onHand: 29, wantInhibited: false, wantQty: 11},
		{name: "exactly at reorder point still orders", onHand: 30, wantInhibited: false, wantQty: 10},
		{name: "one unit above reorder point holds", onHand: 31, wantInhibited: true, wantQty: 0},
		{name: "far above reorder point holds", onHand: 200, wantInhibited: true, wantQty: 0},
		{name: "on-order counts toward position", onHand: 20, onOrder: 15, wantInhibited: true, wantQty: 0},
		{name: "on-order included but still below", onHand: 10, onOrder: 15, wantInhibited: false, wantQty: 15},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := baseProduct()
			p.OnHandUnits = tt.onHand
			p.OnOrderUnits = tt.onOrder

			rec, err := Recommend(p, steadyStats())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rec.OrderingIsInhibited != tt.wantInhibited {
				t.Errorf("OrderingIsInhibited = %v, want %v", rec.OrderingIsInhibited, tt.wantInhibited)
			}
			if rec.RecommendedQuantity != tt.wantQty {
				t.Errorf("RecommendedQuantity = %d, want %d", rec.RecommendedQuantity, tt.wantQty)
			}
			if tt.wantInhibited && rec.InhibitReason == "" {
				t.Error("InhibitReason is empty; an operator cannot see why nothing was ordered")
			}
		})
	}
}

func TestRecommend_FractionalReorderPointIsNotTruncated(t *testing.T) {
	// Regression guard. Comparing position against int(reorderPoint) would
	// truncate 36.6 to 36, so a position of 36 would look "not above" and order.
	// It genuinely is below 36.6, so it should order — but a position of 37,
	// which is above 36.6, must hold. The truncating version gets 37 wrong too
	// (37 > 36 is true, so it happens to pass) — the real failure is at 36.6
	// versus 36. Test both sides.
	stats := steadyStats()
	stats.DemandStdDev = 2 // reorder point 36.6

	t.Run("position 36 is below 36.6 and orders", func(t *testing.T) {
		p := baseProduct()
		p.OnHandUnits = 36
		rec, err := Recommend(p, stats)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rec.OrderingIsInhibited {
			t.Error("held at position 36 against a reorder point of 36.6")
		}
		if rec.RecommendedQuantity != 11 { // 46.6 - 36 = 10.6 -> 11
			t.Errorf("RecommendedQuantity = %d, want 11", rec.RecommendedQuantity)
		}
	})

	t.Run("position 37 is above 36.6 and holds", func(t *testing.T) {
		p := baseProduct()
		p.OnHandUnits = 37
		rec, err := Recommend(p, stats)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !rec.OrderingIsInhibited {
			t.Error("ordered at position 37 against a reorder point of 36.6")
		}
	})
}

func TestRecommend_CaseSizeRounding(t *testing.T) {
	// Position 0, target level 40, so the raw quantity is always 40.
	// Case size 10 divides 40 exactly and must record NO constraint — reporting
	// a constraint that did not change the number is noise in the operator's UI.
	tests := []struct {
		name           string
		caseSize       int
		wantQty        int
		wantConstraint bool
	}{
		{name: "case size 1 is a no-op", caseSize: 1, wantQty: 40, wantConstraint: false},
		{name: "case size divides exactly, no constraint", caseSize: 10, wantQty: 40, wantConstraint: false},
		{name: "case size 8 divides exactly, no constraint", caseSize: 8, wantQty: 40, wantConstraint: false},
		{name: "one unit over a boundary rounds to a full extra case", caseSize: 12, wantQty: 48, wantConstraint: true},
		{name: "case larger than the need rounds up to one case", caseSize: 50, wantQty: 50, wantConstraint: true},
		{name: "case size 6 rounds 40 up to 42", caseSize: 6, wantQty: 42, wantConstraint: true},
		{name: "zero case size treated as unconstrained", caseSize: 0, wantQty: 40, wantConstraint: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := baseProduct()
			p.CaseSize = tt.caseSize

			rec, err := Recommend(p, steadyStats())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rec.RecommendedQuantity != tt.wantQty {
				t.Errorf("RecommendedQuantity = %d, want %d", rec.RecommendedQuantity, tt.wantQty)
			}
			if got := hasConstraint(rec, ConstraintCaseSize); got != tt.wantConstraint {
				t.Errorf("case_size constraint present = %v, want %v", got, tt.wantConstraint)
			}
			// Whatever else happened, the result must be shippable.
			if tt.caseSize > 1 && rec.RecommendedQuantity%tt.caseSize != 0 {
				t.Errorf("RecommendedQuantity %d is not a multiple of case size %d",
					rec.RecommendedQuantity, tt.caseSize)
			}
		})
	}
}

func TestRecommend_ShelfLifeConstraint(t *testing.T) {
	// avg 10/day, target level 40, position 0 => raw quantity 40.
	// Sellable within shelf life = 10 * shelf_life_days.
	tests := []struct {
		name           string
		shelfLifeDays  int
		caseSize       int
		wantQty        int
		wantConstraint bool
	}{
		{
			name:          "non-perishable is unconstrained",
			shelfLifeDays: 0, caseSize: 1, wantQty: 40, wantConstraint: false,
		},
		{
			name:          "long shelf life does not bind",
			shelfLifeDays: 30, caseSize: 1, wantQty: 40, wantConstraint: false,
		},
		{
			name:          "shelf life exactly covers the order, no constraint",
			shelfLifeDays: 4, caseSize: 1, wantQty: 40, wantConstraint: false,
		},
		{
			name:          "three day shelf life caps a four day order",
			shelfLifeDays: 3, caseSize: 1, wantQty: 30, wantConstraint: true,
		},
		{
			name:          "one day shelf life caps hard",
			shelfLifeDays: 1, caseSize: 1, wantQty: 10, wantConstraint: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := baseProduct()
			p.ShelfLifeDays = tt.shelfLifeDays
			p.CaseSize = tt.caseSize

			rec, err := Recommend(p, steadyStats())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rec.RecommendedQuantity != tt.wantQty {
				t.Errorf("RecommendedQuantity = %d, want %d", rec.RecommendedQuantity, tt.wantQty)
			}
			if got := hasConstraint(rec, ConstraintShelfLife); got != tt.wantConstraint {
				t.Errorf("shelf_life constraint present = %v, want %v", got, tt.wantConstraint)
			}
		})
	}
}

func TestRecommend_ShelfLifeBeatsCaseRounding(t *testing.T) {
	// The interaction that a naive implementation gets wrong.
	//
	// avg 10/day, 2-day shelf life => 20 units sellable. Raw quantity 40 is
	// capped to 20. But the case size is 24, so rounding up produces 48 — nearly
	// two and a half weeks of stock for a product that expires in two days.
	// Stepping down a case gives 0: there is no orderable quantity that sells in
	// time, and zero is the correct, if unhelpful, answer.
	p := baseProduct()
	p.ShelfLifeDays = 2
	p.CaseSize = 24

	rec, err := Recommend(p, steadyStats())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rec.RecommendedQuantity != 0 {
		t.Errorf("RecommendedQuantity = %d, want 0; a full 24-unit case cannot "+
			"sell within a 2-day shelf life at 10 units/day", rec.RecommendedQuantity)
	}
	if !hasConstraint(rec, ConstraintShelfLife) {
		t.Error("no shelf_life constraint recorded; the operator cannot see why this is zero")
	}
}

func TestRecommend_ShelfLifeAllowsOneCaseWhenItFits(t *testing.T) {
	// Counterpart to the case above: a 3-day shelf life sells 30 units, so a
	// single 24-unit case fits and must survive the post-rounding re-check.
	p := baseProduct()
	p.ShelfLifeDays = 3
	p.CaseSize = 24

	rec, err := Recommend(p, steadyStats())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.RecommendedQuantity != 24 {
		t.Errorf("RecommendedQuantity = %d, want 24", rec.RecommendedQuantity)
	}
}

func TestRecommend_MinimumOrderQuantity(t *testing.T) {
	// Position 30, target 40 => raw quantity 10.
	tests := []struct {
		name           string
		moq            int
		caseSize       int
		wantQty        int
		wantConstraint bool
	}{
		{name: "no minimum", moq: 0, caseSize: 1, wantQty: 10, wantConstraint: false},
		{name: "minimum below the order does not bind", moq: 5, caseSize: 1, wantQty: 10, wantConstraint: false},
		{name: "minimum equal to the order does not bind", moq: 10, caseSize: 1, wantQty: 10, wantConstraint: false},
		{name: "minimum above the order lifts it", moq: 24, caseSize: 1, wantQty: 24, wantConstraint: true},
		{name: "minimum is itself rounded to a whole case", moq: 25, caseSize: 12, wantQty: 36, wantConstraint: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := baseProduct()
			p.OnHandUnits = 30
			p.MinimumOrderQuantity = tt.moq
			p.CaseSize = tt.caseSize

			rec, err := Recommend(p, steadyStats())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rec.RecommendedQuantity != tt.wantQty {
				t.Errorf("RecommendedQuantity = %d, want %d", rec.RecommendedQuantity, tt.wantQty)
			}
			if got := hasConstraint(rec, ConstraintMinimumOrder); got != tt.wantConstraint {
				t.Errorf("minimum_order_qty constraint present = %v, want %v", got, tt.wantConstraint)
			}
		})
	}
}

func TestRecommend_MinimumOrderDoesNotResurrectAZeroOrder(t *testing.T) {
	// "Order nothing" must never become "order the vendor minimum". A held SKU
	// with a 48-unit minimum should stay at zero, not receive 48 units.
	p := baseProduct()
	p.OnHandUnits = 500 // far above the reorder point
	p.MinimumOrderQuantity = 48

	rec, err := Recommend(p, steadyStats())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.RecommendedQuantity != 0 {
		t.Errorf("RecommendedQuantity = %d, want 0", rec.RecommendedQuantity)
	}
}

func TestRecommend_NeverNegative(t *testing.T) {
	// The quantity is clamped at zero. Position can only exceed the target level
	// when it also exceeds the reorder point (target >= reorder always), so the
	// inhibit branch normally catches it — but a caller can hand us a
	// hand-assembled DemandStats, so the clamp is load-bearing.
	tests := []struct {
		name   string
		stats  *DemandStats
		onHand int
	}{
		{name: "zero demand with stock on hand", stats: &DemandStats{}, onHand: 100},
		{name: "zero demand with nothing on hand", stats: &DemandStats{}, onHand: 0},
		{name: "steady demand with enormous stock", stats: steadyStats(), onHand: 100000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := baseProduct()
			p.OnHandUnits = tt.onHand

			rec, err := Recommend(p, tt.stats)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rec.RecommendedQuantity < 0 {
				t.Errorf("RecommendedQuantity = %d, want >= 0", rec.RecommendedQuantity)
			}
		})
	}
}

func TestRecommend_ZeroDemandOrdersNothing(t *testing.T) {
	// A listed-but-never-selling SKU with empty shelves. Every term is zero, so
	// the reorder point is zero, position is zero, and 0 > 0 is false — we fall
	// through to the arithmetic and must still come out at zero rather than
	// ordering a vendor minimum for a product nobody buys.
	p := baseProduct()
	p.MinimumOrderQuantity = 24

	rec, err := Recommend(p, &DemandStats{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.RecommendedQuantity != 0 {
		t.Errorf("RecommendedQuantity = %d, want 0 for a zero-demand SKU", rec.RecommendedQuantity)
	}
}

func TestRecommend_InputErrors(t *testing.T) {
	tests := []struct {
		name    string
		product *Product
		stats   *DemandStats
		want    error
	}{
		{name: "nil product", product: nil, stats: steadyStats(), want: ErrProductNotFound},
		{name: "nil stats", product: baseProduct(), stats: nil, want: ErrInvalidDemandStats},
		{
			name:    "invalid product is rejected before any arithmetic",
			product: &Product{SKU: "", ReviewPeriodDays: 1},
			stats:   steadyStats(),
			want:    ErrInvalidProduct,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Recommend(tt.product, tt.stats)
			if !errors.Is(err, tt.want) {
				t.Fatalf("got error %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRecommend_ConstraintsAreAlwaysNonNil(t *testing.T) {
	// An empty slice rather than nil, so the JSON encoder emits [] instead of
	// null. Clients should not have to special-case a missing array.
	p := baseProduct()
	rec, err := Recommend(p, steadyStats())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Constraints == nil {
		t.Error("Constraints is nil, want an empty slice so JSON renders []")
	}
}

// hasConstraint reports whether a constraint of the given name was recorded.
func hasConstraint(rec *Recommendation, name string) bool {
	for _, c := range rec.Constraints {
		if c.Name == name {
			return true
		}
	}
	return false
}

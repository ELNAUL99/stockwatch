package inventory

import (
	"errors"
	"testing"
	"time"
)

func TestProductValidate(t *testing.T) {
	// valid returns a product that passes, so each case can mutate exactly one
	// field and prove that field is what the assertion is about.
	valid := func() *Product {
		return &Product{
			SKU:                  "OK-1",
			LeadTimeDays:         3,
			ReviewPeriodDays:     1,
			CaseSize:             12,
			MinimumOrderQuantity: 12,
			ShelfLifeDays:        7,
			TargetServiceLevel:   1.65,
			OnHandUnits:          10,
			OnOrderUnits:         0,
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Product)
		wantErr bool
	}{
		{name: "fully populated product is valid", mutate: func(*Product) {}, wantErr: false},

		// Accepted edge values. These look unusual but are all real
		// configurations, and rejecting them would be the bug.
		{name: "zero lead time is a same-day vendor", mutate: func(p *Product) { p.LeadTimeDays = 0 }},
		{name: "zero shelf life means non-perishable", mutate: func(p *Product) { p.ShelfLifeDays = 0 }},
		{name: "zero case size means no case constraint", mutate: func(p *Product) { p.CaseSize = 0 }},
		{name: "zero minimum order quantity is allowed", mutate: func(p *Product) { p.MinimumOrderQuantity = 0 }},
		{name: "zero service level means no safety stock", mutate: func(p *Product) { p.TargetServiceLevel = 0 }},
		{name: "empty shelves are a valid position", mutate: func(p *Product) { p.OnHandUnits = 0 }},

		// Rejected.
		{name: "empty sku", mutate: func(p *Product) { p.SKU = "" }, wantErr: true},
		{name: "negative lead time", mutate: func(p *Product) { p.LeadTimeDays = -1 }, wantErr: true},
		{name: "zero review period", mutate: func(p *Product) { p.ReviewPeriodDays = 0 }, wantErr: true},
		{name: "negative review period", mutate: func(p *Product) { p.ReviewPeriodDays = -2 }, wantErr: true},
		{name: "negative case size", mutate: func(p *Product) { p.CaseSize = -6 }, wantErr: true},
		{name: "negative minimum order quantity", mutate: func(p *Product) { p.MinimumOrderQuantity = -1 }, wantErr: true},
		{name: "negative shelf life", mutate: func(p *Product) { p.ShelfLifeDays = -3 }, wantErr: true},
		{name: "negative service level", mutate: func(p *Product) { p.TargetServiceLevel = -1.5 }, wantErr: true},
		{name: "negative on hand", mutate: func(p *Product) { p.OnHandUnits = -5 }, wantErr: true},
		{name: "negative on order", mutate: func(p *Product) { p.OnOrderUnits = -5 }, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := valid()
			tt.mutate(p)

			err := p.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Validate() = nil, want an error")
				}
				// Every rejection must carry the sentinel so callers can branch
				// on it, and a message so the operator learns which field.
				if !errors.Is(err, ErrInvalidProduct) {
					t.Errorf("error %v does not wrap ErrInvalidProduct", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestExpectedDemandOn(t *testing.T) {
	tests := []struct {
		name    string
		stats   *DemandStats
		weekday time.Weekday
		want    float64
	}{
		{
			name: "applies the weekday factor",
			stats: &DemandStats{
				AverageDailyDemand: 10,
				DayOfWeekFactors:   map[time.Weekday]float64{time.Saturday: 1.5},
			},
			weekday: time.Saturday,
			want:    15,
		},
		{
			name: "missing weekday falls back to the typical day",
			stats: &DemandStats{
				AverageDailyDemand: 10,
				DayOfWeekFactors:   map[time.Weekday]float64{},
			},
			weekday: time.Tuesday,
			want:    10,
		},
		{
			name:    "nil factor map does not panic",
			stats:   &DemandStats{AverageDailyDemand: 10},
			weekday: time.Tuesday,
			want:    10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.stats.ExpectedDemandOn(tt.weekday); !floatEq(got, tt.want, 1e-9) {
				t.Errorf("ExpectedDemandOn(%v) = %v, want %v", tt.weekday, got, tt.want)
			}
		})
	}
}

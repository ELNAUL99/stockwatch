package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ELNAUL99/stockwatch/internal/httpapi"
	"github.com/ELNAUL99/stockwatch/internal/inventory"
)

// The brief says not to chase coverage in the HTTP layer, so these tests target
// the things that are genuinely this layer's job and that a domain test cannot
// reach: status-code mapping, the error envelope, request-ID propagation, panic
// containment, and body parsing. The arithmetic is already covered upstream.

// fakeService implements httpapi.Service with scripted responses.
type fakeService struct {
	product   *inventory.Product
	rec       *inventory.Recommendation
	batch     []inventory.BatchResult
	err       error
	panicWith any

	mu         sync.Mutex
	recordedTo string
	recorded   *inventory.SalesDay
	blockFor   time.Duration
}

func (f *fakeService) GetProduct(_ context.Context, _ string) (*inventory.Product, error) {
	if f.panicWith != nil {
		panic(f.panicWith)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.product, nil
}

func (f *fakeService) Recommend(ctx context.Context, _ string) (*inventory.Recommendation, error) {
	if f.panicWith != nil {
		panic(f.panicWith)
	}
	if f.blockFor > 0 {
		// Block until either the delay elapses or the request context is
		// cancelled — which is exactly what a context-aware database call does,
		// and what makes the timeout test meaningful rather than a sleep race.
		select {
		case <-time.After(f.blockFor):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.rec, nil
}

func (f *fakeService) RecommendBatch(_ context.Context, skus []string) ([]inventory.BatchResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.batch != nil {
		return f.batch, nil
	}
	out := make([]inventory.BatchResult, 0, len(skus))
	for _, sku := range skus {
		out = append(out, inventory.BatchResult{
			SKU:            sku,
			Recommendation: &inventory.Recommendation{SKU: sku, RecommendedQuantity: 12},
		})
	}
	return out, nil
}

func (f *fakeService) RecordSale(_ context.Context, sku string, day inventory.SalesDay) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordedTo = sku
	f.recorded = &day
	return nil
}

type fakePinger struct{ err error }

func (f *fakePinger) Ping(context.Context) error { return f.err }

// quietLogger discards output. Without it every test run prints the access log
// and the recovered stack traces, drowning the actual failures.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func newTestServer(t *testing.T, svc httpapi.Service, pinger httpapi.Pinger) http.Handler {
	t.Helper()
	if pinger == nil {
		pinger = &fakePinger{}
	}
	return httpapi.NewRouter(svc, pinger, httpapi.Options{Logger: quietLogger()})
}

// do issues a request against the handler and returns the recorder.
//
// httptest.NewRecorder plus handler.ServeHTTP exercises the full middleware
// chain without binding a port, which keeps these tests fast and parallel-safe.
func do(t *testing.T, h http.Handler, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()

	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// decodeError pulls the error envelope out of a response.
func decodeError(t *testing.T, w *httptest.ResponseRecorder) struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
} {
	t.Helper()
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v\nbody: %s", err, w.Body.String())
	}
	return body
}

func testProduct() *inventory.Product {
	return &inventory.Product{
		SKU: "MILK-1L", Name: "Whole Milk 1L",
		LeadTimeDays: 3, MinimumOrderQuantity: 12, CaseSize: 12,
		ReviewPeriodDays: 1, ShelfLifeDays: 14, TargetServiceLevel: 1.65,
		OnHandUnits: 40, OnOrderUnits: 12,
	}
}

func TestErrorMapping(t *testing.T) {
	// The table this whole layer exists to get right: one domain error, one
	// status, one stable code. Centralising it in toAPIError is what stops a
	// missing SKU being a 404 on one route and a 500 on another.
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "unknown sku is 404",
			err:        inventory.ErrProductNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   httpapi.CodeProductNotFound,
		},
		{
			name: "no sales history is 422, not 404",
			// The SKU exists and the request was well-formed; we simply cannot
			// compute. The fix is "record sales", not "fix your request".
			err:        inventory.ErrEmptySalesHistory,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   httpapi.CodeNoHistory,
		},
		{
			name:       "insufficient history is 422",
			err:        inventory.ErrInsufficientHistory,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   httpapi.CodeNoHistory,
		},
		{
			name:       "invalid product is 400",
			err:        inventory.ErrInvalidProduct,
			wantStatus: http.StatusBadRequest,
			wantCode:   httpapi.CodeInvalidProduct,
		},
		{
			name:       "deadline exceeded is 504",
			err:        context.DeadlineExceeded,
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   httpapi.CodeTimeout,
		},
		{
			name:       "an unrecognised error is a generic 500",
			err:        errors.New("something we did not anticipate"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   httpapi.CodeInternal,
		},
		{
			name: "a wrapped domain error still maps correctly",
			// errors.Is walks the chain, so the service adding context with %w
			// must not break the mapping.
			err:        errors.New("load product: " + inventory.ErrProductNotFound.Error()),
			wantStatus: http.StatusInternalServerError, // plain string, not wrapped
			wantCode:   httpapi.CodeInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestServer(t, &fakeService{err: tt.err}, nil)
			w := do(t, h, http.MethodGet, "/products/MILK-1L/recommendation", "")

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body: %s", w.Code, tt.wantStatus, w.Body)
			}
			body := decodeError(t, w)
			if body.Error.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", body.Error.Code, tt.wantCode)
			}
		})
	}
}

func TestWrappedErrorPreservesMapping(t *testing.T) {
	// The genuine %w case, separated out because it is the one that matters:
	// Service.Recommend wraps store errors, and errors.Is must still see through.
	wrapped := errors.Join(errors.New("load sales history"), inventory.ErrProductNotFound)

	h := newTestServer(t, &fakeService{err: wrapped}, nil)
	w := do(t, h, http.MethodGet, "/products/MILK-1L/recommendation", "")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; a wrapped sentinel lost its identity", w.Code)
	}
}

func TestErrorEnvelopeIsAlwaysTheSameShape(t *testing.T) {
	// Every failure, whatever its origin, must produce {"error": {...}} with a
	// content type of JSON. A client that special-cases one route's error format
	// is a client that breaks on the next route.
	tests := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{name: "unmatched route", method: http.MethodGet, target: "/no/such/thing"},
		{name: "wrong method on a known path", method: http.MethodDelete, target: "/products/X"},
		{name: "malformed json body", method: http.MethodPost, target: "/recommendations/batch", body: "{oh no"},
		{name: "empty batch", method: http.MethodPost, target: "/recommendations/batch", body: `{"skus":[]}`},
		{name: "unknown field", method: http.MethodPost, target: "/recommendations/batch", body: `{"skus":["A"],"nope":1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestServer(t, &fakeService{}, nil)
			w := do(t, h, tt.method, tt.target, tt.body)

			if w.Code < 400 {
				t.Fatalf("status = %d, want an error status", w.Code)
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			body := decodeError(t, w)
			if body.Error.Code == "" {
				t.Error("error.code is empty")
			}
			if body.Error.Message == "" {
				t.Error("error.message is empty")
			}
			if body.Error.RequestID == "" {
				t.Error("error.request_id is empty; a user cannot correlate this with a log line")
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	// ServeMux gives 405 with an Allow header for free once patterns carry a
	// method — worth asserting, because it is a behaviour we get by using the
	// Go 1.22 routing rather than something we wrote.
	h := newTestServer(t, &fakeService{product: testProduct()}, nil)
	w := do(t, h, http.MethodPost, "/products/MILK-1L", `{}`)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Errorf("Allow = %q, want it to mention GET", allow)
	}
}

func TestGetProduct(t *testing.T) {
	h := newTestServer(t, &fakeService{product: testProduct()}, nil)
	w := do(t, h, http.MethodGet, "/products/MILK-1L", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body)
	}

	var got struct {
		SKU    string `json:"sku"`
		Vendor struct {
			CaseSize int `json:"case_size"`
		} `json:"vendor"`
		Position struct {
			OnHandUnits  int `json:"on_hand_units"`
			OnOrderUnits int `json:"on_order_units"`
			TotalUnits   int `json:"total_units"`
		} `json:"position"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, w.Body)
	}

	if got.SKU != "MILK-1L" {
		t.Errorf("sku = %q", got.SKU)
	}
	if got.Vendor.CaseSize != 12 {
		t.Errorf("vendor.case_size = %d, want 12", got.Vendor.CaseSize)
	}
	// The precomputed total is the field that stops callers forgetting on-order,
	// which is the most common replenishment mistake there is.
	if got.Position.TotalUnits != 52 {
		t.Errorf("position.total_units = %d, want 52 (40 on hand + 12 on order)",
			got.Position.TotalUnits)
	}
}

func TestRecordSale(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		check      func(*testing.T, *fakeService)
	}{
		{
			name:       "valid sale",
			body:       `{"date":"2026-06-01","units_sold":25}`,
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f *fakeService) {
				if f.recorded == nil {
					t.Fatal("nothing was recorded")
				}
				if f.recorded.UnitsSold != 25 {
					t.Errorf("UnitsSold = %d, want 25", f.recorded.UnitsSold)
				}
				want := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
				if !f.recorded.Date.Equal(want) {
					t.Errorf("Date = %v, want %v", f.recorded.Date, want)
				}
			},
		},
		{
			name:       "stockout day",
			body:       `{"date":"2026-06-01","units_sold":12,"stockout_occurred":true}`,
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f *fakeService) {
				if !f.recorded.StockOutOccurred {
					t.Error("StockOutOccurred = false, want true")
				}
			},
		},
		{
			name: "explicit zero sales is recorded, not treated as missing",
			// The reason units_sold is a *int. A genuine zero-demand day is a
			// real observation the estimator needs; conflating it with an absent
			// field would silently drop data.
			body:       `{"date":"2026-06-01","units_sold":0}`,
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f *fakeService) {
				if f.recorded == nil || f.recorded.UnitsSold != 0 {
					t.Error("a zero-unit day was not recorded")
				}
			},
		},
		{
			name:       "missing date rejected",
			body:       `{"units_sold":25}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing units_sold rejected rather than defaulting to zero",
			body:       `{"date":"2026-06-01"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "bad date format rejected",
			body:       `{"date":"01/06/2026","units_sold":25}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "a fully censored day is accepted",
			// Zero sales WITH the stockout flag means the shelf was empty at
			// opening: the whole day's demand went unrecorded. It is the most
			// censored observation the API accepts, and rejecting it would force
			// callers to report it as an ordinary zero-demand day — feeding a
			// fabricated zero into the demand estimate.
			body:       `{"date":"2026-06-01","units_sold":0,"stockout_occurred":true}`,
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f *fakeService) {
				if f.recorded == nil {
					t.Fatal("nothing was recorded")
				}
				if !f.recorded.StockOutOccurred || f.recorded.UnitsSold != 0 {
					t.Errorf("recorded %+v, want a zero-unit censored day", *f.recorded)
				}
			},
		},
		{
			name:       "wrong type rejected",
			body:       `{"date":"2026-06-01","units_sold":"lots"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unknown field rejected",
			body:       `{"date":"2026-06-01","units_sold":25,"typo":true}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "trailing content rejected",
			body:       `{"date":"2026-06-01","units_sold":25}{"date":"2026-06-02","units_sold":9}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{}
			h := newTestServer(t, svc, nil)
			w := do(t, h, http.MethodPost, "/products/MILK-1L/sales", tt.body)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tt.wantStatus, w.Body)
			}
			if tt.check != nil {
				tt.check(t, svc)
			}
		})
	}
}

func TestBatch(t *testing.T) {
	t.Run("duplicates are collapsed, order preserved", func(t *testing.T) {
		svc := &fakeService{}
		h := newTestServer(t, svc, nil)
		w := do(t, h, http.MethodPost, "/recommendations/batch",
			`{"skus":["B","A","B","C","A"]}`)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body)
		}

		var body struct {
			Results []struct {
				SKU string `json:"sku"`
			} `json:"results"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}

		want := []string{"B", "A", "C"}
		if len(body.Results) != len(want) {
			t.Fatalf("got %d results, want %d", len(body.Results), len(want))
		}
		for i, sku := range want {
			if body.Results[i].SKU != sku {
				t.Errorf("results[%d] = %q, want %q", i, body.Results[i].SKU, sku)
			}
		}
	})

	t.Run("partial failures still return 200", func(t *testing.T) {
		// The batch call itself succeeded. One dead SKU out of a thousand must
		// not colour the whole response as a failure.
		svc := &fakeService{batch: []inventory.BatchResult{
			{SKU: "GOOD", Recommendation: &inventory.Recommendation{SKU: "GOOD"}},
			{SKU: "BAD", Error: "product not found"},
		}}
		h := newTestServer(t, svc, nil)
		w := do(t, h, http.MethodPost, "/recommendations/batch", `{"skus":["GOOD","BAD"]}`)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("oversized batch rejected", func(t *testing.T) {
		skus := make([]string, 501)
		for i := range skus {
			skus[i] = "SKU-" + strings.Repeat("x", i%5) + string(rune('A'+i%26)) + string(rune('0'+i%10))
		}
		payload, _ := json.Marshal(map[string]any{"skus": skus})

		h := newTestServer(t, &fakeService{}, nil)
		w := do(t, h, http.MethodPost, "/recommendations/batch", string(payload))

		// Either 400 for too many entries, or 413 if it also blew the body cap.
		if w.Code != http.StatusBadRequest && w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 400 or 413", w.Code)
		}
	})

	t.Run("empty sku string rejected", func(t *testing.T) {
		h := newTestServer(t, &fakeService{}, nil)
		w := do(t, h, http.MethodPost, "/recommendations/batch", `{"skus":["A","  "]}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})
}

func TestHealthzIgnoresDependencies(t *testing.T) {
	// Liveness must not touch the database. If it did, a brief Postgres outage
	// would make Kubernetes restart every replica, turning a recoverable blip
	// into a full outage with a thundering-herd reconnect afterwards.
	h := newTestServer(t, &fakeService{}, &fakePinger{err: errors.New("database is on fire")})
	w := do(t, h, http.MethodGet, "/healthz", "")

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 even with a failed database; "+
			"liveness must not depend on dependencies", w.Code)
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name       string
		pingErr    error
		wantStatus int
	}{
		{name: "healthy database", pingErr: nil, wantStatus: http.StatusOK},
		{name: "unreachable database", pingErr: errors.New("connection refused"),
			wantStatus: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestServer(t, &fakeService{}, &fakePinger{err: tt.pingErr})
			w := do(t, h, http.MethodGet, "/readyz", "")

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}

func TestPanicIsContained(t *testing.T) {
	// net/http runs each request in its own goroutine, and an unrecovered panic
	// in any goroutine takes down the whole process — not just that request.
	// Without Recover, one nil-map write drops every in-flight request on the
	// instance. This test failing means the process would have died.
	h := newTestServer(t, &fakeService{panicWith: "boom"}, nil)
	w := do(t, h, http.MethodGet, "/products/MILK-1L", "")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}

	body := decodeError(t, w)
	if body.Error.Code != httpapi.CodeInternal {
		t.Errorf("code = %q, want %q", body.Error.Code, httpapi.CodeInternal)
	}
	// The panic value must not reach the client — it can carry internal detail.
	if strings.Contains(w.Body.String(), "boom") {
		t.Error("the panic value leaked into the response body")
	}
}

func TestRequestID(t *testing.T) {
	t.Run("generated when absent", func(t *testing.T) {
		h := newTestServer(t, &fakeService{product: testProduct()}, nil)
		w := do(t, h, http.MethodGet, "/products/MILK-1L", "")

		if got := w.Header().Get(httpapi.RequestIDHeader); got == "" {
			t.Error("no request ID on the response")
		}
	})

	t.Run("inbound id is reused so traces survive across hops", func(t *testing.T) {
		h := newTestServer(t, &fakeService{product: testProduct()}, nil)

		r := httptest.NewRequest(http.MethodGet, "/products/MILK-1L", nil)
		r.Header.Set(httpapi.RequestIDHeader, "trace-from-upstream")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		if got := w.Header().Get(httpapi.RequestIDHeader); got != "trace-from-upstream" {
			t.Errorf("request id = %q, want it preserved from the inbound header", got)
		}
	})

	t.Run("ids differ between requests", func(t *testing.T) {
		h := newTestServer(t, &fakeService{product: testProduct()}, nil)

		first := do(t, h, http.MethodGet, "/products/MILK-1L", "").
			Header().Get(httpapi.RequestIDHeader)
		second := do(t, h, http.MethodGet, "/products/MILK-1L", "").
			Header().Get(httpapi.RequestIDHeader)

		if first == second {
			t.Errorf("both requests got id %q", first)
		}
	})
}

func TestTimeoutCancelsHandlerWork(t *testing.T) {
	// The point of using a context deadline rather than http.TimeoutHandler:
	// the handler must actually observe cancellation and stop, so a pooled
	// database connection is returned instead of being held by work nobody is
	// waiting for. The fake selects on ctx.Done(), mirroring what pgx does.
	svc := &fakeService{blockFor: 5 * time.Second}
	h := httpapi.NewRouter(svc, &fakePinger{}, httpapi.Options{
		Logger:         quietLogger(),
		RequestTimeout: 50 * time.Millisecond,
	})

	start := time.Now()
	w := do(t, h, http.MethodGet, "/products/MILK-1L/recommendation", "")
	elapsed := time.Since(start)

	if w.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504; body: %s", w.Code, w.Body)
	}
	if elapsed > time.Second {
		t.Errorf("took %s; the handler did not observe the cancelled context", elapsed)
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	h := httpapi.NewRouter(&fakeService{}, &fakePinger{}, httpapi.Options{
		Logger:       quietLogger(),
		MaxBodyBytes: 100,
	})

	w := do(t, h, http.MethodPost, "/recommendations/batch",
		`{"skus":["`+strings.Repeat("A", 500)+`"]}`)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

func TestUnsupportedMediaType(t *testing.T) {
	h := newTestServer(t, &fakeService{}, nil)

	r := httptest.NewRequest(http.MethodPost, "/recommendations/batch",
		strings.NewReader(`{"skus":["A"]}`))
	r.Header.Set("Content-Type", "text/xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", w.Code)
	}
}

func TestContentTypeWithCharsetAccepted(t *testing.T) {
	// A charset parameter is legitimate and must not be rejected — comparing
	// the whole header rather than the media type is a common bug.
	h := newTestServer(t, &fakeService{}, nil)

	r := httptest.NewRequest(http.MethodPost, "/recommendations/batch",
		strings.NewReader(`{"skus":["A"]}`))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body: %s", w.Code, w.Body)
	}
}

func TestRecommendationPayload(t *testing.T) {
	// The recommendation is serialised straight from the domain type, so this
	// asserts the wire contract those json tags create.
	rec := &inventory.Recommendation{
		SKU:                 "BAGEL-6PK",
		RecommendedQuantity: 60,
		SafetyStock:         4.42,
		ReorderPoint:        48.89,
		TargetLevel:         71.12,
		AverageDailyDemand:  22.23,
		CurrentOnHand:       14,
		Constraints: []inventory.RecommendationConstraint{
			{Name: "case_size", OriginalQuantity: 57, FinalQuantity: 60, Reason: "rounded up"},
		},
	}

	h := newTestServer(t, &fakeService{rec: rec}, nil)
	w := do(t, h, http.MethodGet, "/products/BAGEL-6PK/recommendation", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Field names are the API contract; renaming one breaks every client.
	for _, key := range []string{
		"sku", "recommended_quantity", "safety_stock", "reorder_point",
		"target_level", "average_daily_demand", "current_on_hand",
		"current_on_order", "ordering_is_inhibited", "constraints",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("response is missing field %q", key)
		}
	}
}

func TestConstraintsSerialiseAsArrayNotNull(t *testing.T) {
	// An empty slice must render as [] so clients can iterate unconditionally.
	// A nil slice would render as null and break a naive for-loop.
	rec := &inventory.Recommendation{SKU: "X", Constraints: []inventory.RecommendationConstraint{}}

	h := newTestServer(t, &fakeService{rec: rec}, nil)
	w := do(t, h, http.MethodGet, "/products/X/recommendation", "")

	if !strings.Contains(w.Body.String(), `"constraints":[]`) {
		t.Errorf("constraints did not render as an empty array; body: %s", w.Body)
	}
}

func TestSKUValidation(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		wantStatus int
	}{
		{name: "normal sku", target: "/products/MILK-1L", wantStatus: http.StatusOK},
		{name: "sku with url encoding", target: "/products/MILK%2F1L", wantStatus: http.StatusOK},
		{name: "overlong sku rejected", target: "/products/" + strings.Repeat("X", 100),
			wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestServer(t, &fakeService{product: testProduct()}, nil)
			w := do(t, h, http.MethodGet, tt.target, "")
			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body: %s", w.Code, tt.wantStatus, w.Body)
			}
		})
	}
}

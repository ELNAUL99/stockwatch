package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ELNAUL99/stockwatch/internal/inventory"
)

// Service is the behaviour the HTTP layer needs.
//
// Declared here rather than importing *inventory.Service concretely, for the
// same reason the domain declares its storage ports: the consumer owns the
// interface. It also means these handlers can be tested against a fake with no
// database anywhere in sight.
type Service interface {
	ListProducts(ctx context.Context) ([]*inventory.Product, error)
	GetProduct(ctx context.Context, sku string) (*inventory.Product, error)
	Recommend(ctx context.Context, sku string) (*inventory.Recommendation, error)
	RecommendBatch(ctx context.Context, skus []string) ([]inventory.BatchResult, error)
	RecordSale(ctx context.Context, sku string, day inventory.SalesDay) error
}

// Pinger is the readiness check's dependency, kept separate from Service so a
// probe cannot accidentally be wired to something that queries business data.
type Pinger interface {
	Ping(ctx context.Context) error
}

// maxBatchSKUs bounds a batch request.
//
// The batch path is two queries regardless of size, but the response grows
// linearly and an unbounded request is a cheap way for one caller to make the
// service allocate without limit. 500 covers a full DashMart replenishment run
// with room to spare.
const maxBatchSKUs = 500

// productResponse is the wire shape for a product.
//
// This one is a hand-written DTO rather than json tags on inventory.Product,
// which is the opposite of the choice made for Recommendation. The reason is
// that Product mixes two things a client should see differently: vendor terms
// that rarely change, and a stock position that changes constantly. Splitting
// them here documents that in the payload. Recommendation has no such internal
// structure, so tagging it directly cost nothing.
type productResponse struct {
	SKU  string `json:"sku"`
	Name string `json:"name"`

	Vendor struct {
		LeadTimeDays         int `json:"lead_time_days"`
		MinimumOrderQuantity int `json:"minimum_order_quantity"`
		CaseSize             int `json:"case_size"`
	} `json:"vendor"`

	Policy struct {
		ReviewPeriodDays   int     `json:"review_period_days"`
		ShelfLifeDays      int     `json:"shelf_life_days"`
		TargetServiceLevel float64 `json:"target_service_level"`
	} `json:"policy"`

	Position struct {
		OnHandUnits  int `json:"on_hand_units"`
		OnOrderUnits int `json:"on_order_units"`
		TotalUnits   int `json:"total_units"`
	} `json:"position"`
}

func toProductResponse(p *inventory.Product) productResponse {
	var out productResponse
	out.SKU = p.SKU
	out.Name = p.Name

	out.Vendor.LeadTimeDays = p.LeadTimeDays
	out.Vendor.MinimumOrderQuantity = p.MinimumOrderQuantity
	out.Vendor.CaseSize = p.CaseSize

	out.Policy.ReviewPeriodDays = p.ReviewPeriodDays
	out.Policy.ShelfLifeDays = p.ShelfLifeDays
	out.Policy.TargetServiceLevel = p.TargetServiceLevel

	out.Position.OnHandUnits = p.OnHandUnits
	out.Position.OnOrderUnits = p.OnOrderUnits
	// Precomputed because "do I have enough?" is the question every caller
	// actually asks, and making each one sum two fields invites them to forget
	// on-order — the single most common replenishment mistake.
	out.Position.TotalUnits = p.OnHandUnits + p.OnOrderUnits

	return out
}

// handleListProducts serves GET /products.
func (s *server) handleListProducts(w http.ResponseWriter, r *http.Request) {
	products, err := s.svc.ListProducts(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}

	out := make([]productResponse, len(products))
	for i, p := range products {
		out[i] = toProductResponse(p)
	}

	writeJSON(w, r, http.StatusOK, out)
}

// handleGetProduct serves GET /products/{sku}.
func (s *server) handleGetProduct(w http.ResponseWriter, r *http.Request) {
	sku, err := pathSKU(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	product, err := s.svc.GetProduct(r.Context(), sku)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, toProductResponse(product))
}

// salesRequest is the body of POST /products/{sku}/sales.
//
// Pointer fields for the two required values so the decoder can distinguish
// "absent" from "present and zero". Without that, a body of {} would silently
// record a zero-unit sales day — and zero sales is a meaningful observation we
// must not fabricate. This is the standard Go answer to the missing-vs-zero
// problem that languages with null-by-default do not have.
type salesRequest struct {
	Date             *string `json:"date"`
	UnitsSold        *int    `json:"units_sold"`
	StockOutOccurred bool    `json:"stockout_occurred"`
}

// handleRecordSale serves POST /products/{sku}/sales.
func (s *server) handleRecordSale(w http.ResponseWriter, r *http.Request) {
	sku, err := pathSKU(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	var req salesRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}

	if req.Date == nil {
		writeError(w, r, badRequest("field \"date\" is required"))
		return
	}
	if req.UnitsSold == nil {
		writeError(w, r, badRequest("field \"units_sold\" is required"))
		return
	}

	// time.DateOnly is the "2006-01-02" layout constant added in Go 1.20.
	// Parsing in UTC matches how the column is stored, so a sale is attributed
	// to the same calendar day regardless of where the caller is.
	date, err := time.ParseInLocation(time.DateOnly, *req.Date, time.UTC)
	if err != nil {
		writeError(w, r, badRequest(fmt.Sprintf(
			"field \"date\" must be YYYY-MM-DD, got %q", *req.Date)))
		return
	}

	// Note what is NOT rejected here: units_sold = 0 with stockout_occurred =
	// true. That combination is not a contradiction, it is the strongest signal
	// this endpoint accepts — the shelf was empty at opening, so the entire
	// day's demand went unrecorded. Rejecting it would force the caller to
	// report a fully censored day as an ordinary zero-demand day, which is
	// precisely the data corruption the demand estimator exists to avoid.

	day := inventory.SalesDay{
		Date:             date,
		UnitsSold:        *req.UnitsSold,
		StockOutOccurred: req.StockOutOccurred,
	}

	if err := s.svc.RecordSale(r.Context(), sku, day); err != nil {
		writeError(w, r, err)
		return
	}

	// 200, not 201. The write is an idempotent upsert of a day that either
	// exists or does not, so there is no new resource with its own URL to point
	// a Location header at. Returning the stored value lets the caller confirm
	// what was persisted after date normalisation.
	writeJSON(w, r, http.StatusOK, map[string]any{
		"sku":               sku,
		"date":              date.Format(time.DateOnly),
		"units_sold":        day.UnitsSold,
		"stockout_occurred": day.StockOutOccurred,
	})
}

// handleRecommendation serves GET /products/{sku}/recommendation.
func (s *server) handleRecommendation(w http.ResponseWriter, r *http.Request) {
	sku, err := pathSKU(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	rec, err := s.svc.Recommend(r.Context(), sku)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, rec)
}

type batchRequest struct {
	SKUs []string `json:"skus"`
}

type batchResponse struct {
	Results []inventory.BatchResult `json:"results"`
}

// handleBatch serves POST /recommendations/batch.
//
// 200 even when individual SKUs fail. The batch itself succeeded; per-SKU
// outcomes live in the body. Returning 207 Multi-Status would be more literal
// but it is a WebDAV code that most HTTP clients treat as an unexpected
// success, and a partial failure is the normal case here rather than an
// exception — one dead SKU in a thousand should not colour the whole response.
func (s *server) handleBatch(w http.ResponseWriter, r *http.Request) {
	var req batchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}

	if len(req.SKUs) == 0 {
		writeError(w, r, badRequest("field \"skus\" must contain at least one sku"))
		return
	}
	if len(req.SKUs) > maxBatchSKUs {
		writeError(w, r, badRequest(fmt.Sprintf(
			"field \"skus\" has %d entries, limit is %d", len(req.SKUs), maxBatchSKUs)))
		return
	}

	// Deduplicate while preserving the caller's order. A repeated SKU would
	// otherwise be computed and returned twice, which is wasted work and
	// confusing output.
	seen := make(map[string]struct{}, len(req.SKUs))
	skus := make([]string, 0, len(req.SKUs))
	for _, sku := range req.SKUs {
		sku = strings.TrimSpace(sku)
		if sku == "" {
			writeError(w, r, badRequest("field \"skus\" contains an empty sku"))
			return
		}
		if _, dup := seen[sku]; dup {
			continue
		}
		seen[sku] = struct{}{}
		skus = append(skus, sku)
	}

	results, err := s.svc.RecommendBatch(r.Context(), skus)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, batchResponse{Results: results})
}

// handleHealthz serves GET /healthz: is this process alive?
//
// It checks nothing. That is the point, and it is the most commonly botched
// probe in production: if liveness touched the database, a brief database
// outage would make Kubernetes kill and restart every replica, turning a
// recoverable dependency blip into a full outage with a thundering-herd
// reconnect on the far side.
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz serves GET /readyz: can this process serve traffic right now?
//
// This one does check the database, because a replica that cannot reach
// Postgres should be taken out of the load balancer — but left running, so it
// can rejoin when the database returns.
func (s *server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// A tight bound of its own: a readiness probe that blocks for the full
	// request timeout keeps a connection busy and delays the orchestrator's
	// verdict. Failing fast is the useful behaviour here.
	ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
	defer cancel()

	if err := s.pinger.Ping(ctx); err != nil {
		LoggerFrom(r.Context()).Warn("readiness check failed",
			slog.String("error", err.Error()))
		writeJSON(w, r, http.StatusServiceUnavailable, errorResponse{Error: errorBody{
			Code:      CodeUnavailable,
			Message:   "database unreachable",
			RequestID: RequestIDFrom(r.Context()),
		}})
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]string{
		"status":   "ok",
		"database": "ok",
	})
}

const readyzTimeout = 2 * time.Second

// pathSKU extracts and validates {sku} from the route.
//
// r.PathValue is the Go 1.22 addition that makes stdlib routing viable — before
// it, extracting a path segment meant either a third-party router or manual
// strings.Split on r.URL.Path.
func pathSKU(r *http.Request) (string, error) {
	sku := strings.TrimSpace(r.PathValue("sku"))
	if sku == "" {
		return "", badRequest("sku is required")
	}
	if len(sku) > maxSKULength {
		return "", badRequest(fmt.Sprintf(
			"sku is %d characters, limit is %d", len(sku), maxSKULength))
	}
	return sku, nil
}

const maxSKULength = 64

// decodeJSON reads a JSON body strictly.
//
// DisallowUnknownFields makes a typo in a field name a 400 rather than a silent
// no-op. That is a deliberate strictness choice with a real cost: it means
// adding a field to a client before the server knows about it breaks the call,
// so a rolling deploy has to update the server first. For an internal service
// the early feedback is worth more than the flexibility.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		// Compare only the media type, since a charset parameter is legitimate.
		if mediaType, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(mediaType) != "application/json" {
			return &apiError{
				status:  http.StatusUnsupportedMediaType,
				code:    CodeBadRequest,
				message: fmt.Sprintf("Content-Type must be application/json, got %q", mediaType),
			}
		}
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		// MaxBytesReader signals its limit with this specific error type, which
		// deserves 413 rather than a generic 400.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return &apiError{
				status:  http.StatusRequestEntityTooLarge,
				code:    CodePayloadTooLarge,
				message: fmt.Sprintf("request body exceeds %d bytes", maxErr.Limit),
			}
		}

		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return badRequest(fmt.Sprintf(
				"malformed JSON at byte %d", syntaxErr.Offset))
		}

		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return badRequest(fmt.Sprintf(
				"field %q must be of type %s", typeErr.Field, typeErr.Type))
		}

		return badRequest(fmt.Sprintf("invalid request body: %v", err))
	}

	// A second Decode must report EOF. Anything else means the body held
	// trailing content — two concatenated objects, say — and silently ignoring
	// the remainder would mean accepting a request we did not fully understand.
	//
	// Decoding into json.RawMessage rather than an empty struct matters:
	// DisallowUnknownFields is set on this decoder, so a second real object
	// decoded into struct{}{} fails with "unknown field" rather than succeeding,
	// and a check for a nil error would let the trailing content through. Only
	// io.EOF proves the body is exhausted.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return badRequest("request body must contain exactly one JSON object")
	}

	return nil
}

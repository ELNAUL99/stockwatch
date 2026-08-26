package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/ELNAUL99/stockwatch/internal/inventory"
)

// Every error response has this shape, always, whatever went wrong:
//
//	{"error": {"code": "product_not_found",
//	           "message": "no product with sku MILK-1L",
//	           "request_id": "3f8a1c..."}}
//
// The nesting under "error" is what makes it stable: a client can test for the
// presence of that one key to decide success or failure, without needing to know
// the status code or the shape of the success payload. Flat error objects tend
// to collide with success fields as an API grows.
//
// code is a machine-readable enum a client may branch on, and it is part of the
// API contract — renaming one is a breaking change. message is for humans and
// may change freely. request_id lets a user paste one string into a support
// ticket and have it match a log line.
type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

// Error codes. Exported so tests reference the same constants the handlers do
// rather than restating string literals that can silently drift.
const (
	CodeBadRequest       = "bad_request"
	CodeProductNotFound  = "product_not_found"
	CodeNoHistory        = "insufficient_history"
	CodeInvalidProduct   = "invalid_product"
	CodeTimeout          = "timeout"
	CodeInternal         = "internal_error"
	CodeUnavailable      = "service_unavailable"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeNotFound         = "not_found"
	CodePayloadTooLarge  = "payload_too_large"
)

// apiError couples an HTTP status with a stable code. Handlers construct these
// rather than calling http.Error directly, so no handler can invent a one-off
// error format.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func badRequest(msg string) *apiError {
	return &apiError{status: http.StatusBadRequest, code: CodeBadRequest, message: msg}
}

// writeError renders an apiError, or maps a domain error onto one.
//
// This function is the single place where a domain error becomes an HTTP status.
// Keeping that mapping here rather than in each handler is what stops the API
// from returning 500 for a missing SKU in one endpoint and 404 in another.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	apiErr := toAPIError(err)

	logger := LoggerFrom(r.Context())

	// 5xx means we broke; 4xx means the caller did. Logging both at the same
	// level makes an error dashboard useless, because a client typo would look
	// identical to a database outage.
	if apiErr.status >= http.StatusInternalServerError {
		logger.Error("request failed",
			slog.String("code", apiErr.code),
			slog.Int("status", apiErr.status),
			// The full error chain goes to the log, never to the client. A
			// wrapped pgx error can name tables, columns and the connection
			// string, so the response carries only the sanitised message.
			slog.String("error", err.Error()))
	} else {
		logger.Info("request rejected",
			slog.String("code", apiErr.code),
			slog.Int("status", apiErr.status),
			slog.String("error", err.Error()))
	}

	writeJSON(w, r, apiErr.status, errorResponse{Error: errorBody{
		Code:      apiErr.code,
		Message:   apiErr.message,
		RequestID: RequestIDFrom(r.Context()),
	}})
}

// toAPIError maps any error onto a status and a code.
//
// The default is deliberately 500 with a generic message: an error we did not
// anticipate is by definition one we cannot describe safely to a client. Adding
// a case here is how a new failure mode gets a proper status.
func toAPIError(err error) *apiError {
	// An explicitly constructed apiError wins over any mapping below.
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr
	}

	switch {
	case errors.Is(err, inventory.ErrProductNotFound):
		return &apiError{
			status:  http.StatusNotFound,
			code:    CodeProductNotFound,
			message: err.Error(),
		}

	case errors.Is(err, inventory.ErrEmptySalesHistory),
		errors.Is(err, inventory.ErrInsufficientHistory):
		// 422, not 404 and not 400. The request was well-formed and the SKU
		// exists — we simply cannot compute a recommendation from the data we
		// hold. That is a distinct situation an operator needs to see, because
		// the fix is "record more sales", not "correct the request".
		return &apiError{
			status:  http.StatusUnprocessableEntity,
			code:    CodeNoHistory,
			message: err.Error(),
		}

	case errors.Is(err, inventory.ErrInvalidProduct),
		errors.Is(err, inventory.ErrNoProducts):
		return &apiError{
			status:  http.StatusBadRequest,
			code:    CodeInvalidProduct,
			message: err.Error(),
		}

	case errors.Is(err, context.DeadlineExceeded):
		// 504: we gave up waiting on something downstream. Distinguishing this
		// from a plain 500 tells an operator to look at database latency rather
		// than at application logs.
		return &apiError{
			status:  http.StatusGatewayTimeout,
			code:    CodeTimeout,
			message: "request timed out",
		}

	case errors.Is(err, context.Canceled):
		// The client hung up. 499 is nginx's non-standard code for this; it is
		// never actually delivered because there is no longer a connection to
		// write to, but it keeps these out of the 5xx error rate where they
		// would look like our failures.
		return &apiError{
			status:  499,
			code:    CodeBadRequest,
			message: "client closed request",
		}

	default:
		return &apiError{
			status:  http.StatusInternalServerError,
			code:    CodeInternal,
			message: "internal error",
		}
	}
}

// writeJSON renders a value with the right headers and status.
//
// The header order matters and is a genuine gotcha: Content-Type must be set
// before WriteHeader, because WriteHeader flushes the header block. Setting it
// afterwards silently does nothing and Go will not warn you.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Responses are per-request and never cacheable — stock positions change
	// minute to minute and a stale recommendation is worse than none.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already sent, so we cannot change it to a 500 —
		// the client will receive a truncated body and see a JSON parse error.
		// All we can do is make sure the failure is visible on our side.
		LoggerFrom(r.Context()).Error("encode response body",
			slog.String("error", err.Error()))
	}
}

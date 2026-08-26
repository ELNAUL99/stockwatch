package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// Middleware is a function that wraps a handler in another handler.
//
// This signature is the whole of Go's middleware convention — there is no
// framework and no registration step. Anything matching it composes with
// anything else matching it, which is why stdlib-only routing costs so little
// here compared to Express or FastAPI.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so that the first argument is the outermost layer.
//
// Chain(A, B, C)(h) produces A(B(C(h))): a request passes through A, then B,
// then C, then the handler, and the response unwinds back out the same way.
// Applying them in reverse order here is what makes the call site read in the
// order requests actually travel — without it, the list would be inside-out and
// every reader would have to mentally reverse it.
func Chain(mw ...Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		for i := len(mw) - 1; i >= 0; i-- {
			next = mw[i](next)
		}
		return next
	}
}

// contextKey is an unexported named type used for context keys.
//
// Using a bare string like "request_id" would risk another package writing to
// the same key and silently clobbering ours. An unexported type makes the key
// unforgeable outside this package: nobody else can even construct one. This is
// the documented convention and the reason context.WithValue takes an `any`.
type contextKey string

const (
	requestIDKey contextKey = "request_id"
	loggerKey    contextKey = "logger"
)

// RequestIDHeader is the header read on the way in and echoed on the way out.
const RequestIDHeader = "X-Request-Id"

// RequestIDFrom returns the request ID, or "" outside a request.
func RequestIDFrom(ctx context.Context) string {
	// The comma-ok form on a type assertion. Without it, a missing or
	// wrong-typed value panics; with it, we get the zero value instead. In
	// middleware that runs on every request, panicking on a missing key would
	// be a poor trade.
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// LoggerFrom returns the request-scoped logger, already carrying the request ID
// and method and path, so no call site has to remember to attach them.
//
// It falls back to the default logger rather than returning nil, so calling it
// from a context that never passed through the middleware — a background job, a
// test — still logs instead of panicking.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// RequestID assigns each request an ID and attaches a logger carrying it.
//
// An inbound X-Request-Id is trusted and reused so a trace survives across
// service hops. That is safe here because this service sits behind an ingress
// that controls the header; on a directly internet-facing service you would
// validate or discard it, since it lands in log fields.
func RequestID(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if id == "" {
				id = newRequestID()
			}

			// Attaching the fields once here is what satisfies "request ID in
			// every line": every later log call goes through LoggerFrom and
			// inherits them, so no handler can forget.
			reqLogger := logger.With(
				slog.String("request_id", id),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)

			ctx := context.WithValue(r.Context(), requestIDKey, id)
			ctx = context.WithValue(ctx, loggerKey, reqLogger)

			// Echo it back so a client can correlate without parsing the body,
			// and so it is present even on responses with no body at all.
			w.Header().Set(RequestIDHeader, id)

			// r.WithContext returns a shallow copy; the original request is
			// never mutated. Requests are effectively immutable in Go, which is
			// why every middleware ends with this line rather than assigning
			// to r.ctx.
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func newRequestID() string {
	b := make([]byte, 8)
	// crypto/rand.Read is documented never to return an error on any supported
	// platform since Go 1.24, and the older API errored only on catastrophic
	// entropy failure. Discarding it is safe and avoids an unreachable branch.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// statusRecorder captures the status code and byte count for the access log.
//
// net/http gives no way to read back what a handler wrote, so logging
// middleware has to intercept. This is the standard workaround.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (rec *statusRecorder) WriteHeader(status int) {
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	// A handler that calls Write without WriteHeader implicitly sends 200.
	// Recording that here keeps the log honest for those handlers.
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += n
	return n, err
}

// Logging writes one structured line per completed request.
func Logging() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}

			// Deferred so the line is still written when an inner handler
			// panics: the deferred call runs during stack unwinding, before
			// Recover further out converts the panic into a 500.
			defer func() {
				status := rec.status
				if status == 0 {
					status = http.StatusOK
				}

				LoggerFrom(r.Context()).Info("request",
					slog.Int("status", status),
					slog.Int("bytes", rec.bytes),
					// Milliseconds as a float: integer milliseconds round a
					// 400-microsecond health check to 0, which makes latency
					// percentiles useless at the fast end.
					slog.Float64("duration_ms",
						float64(time.Since(start).Microseconds())/1000.0),
				)
			}()

			next.ServeHTTP(rec, r)
		})
	}
}

// Recover turns a panic into a 500 instead of killing the process.
//
// This matters more in Go than in most languages: net/http runs every request in
// its own goroutine, and an unrecovered panic in any goroutine tears down the
// whole process, not just that request. Without this, one nil map write in one
// handler drops every in-flight request on the instance.
func Recover() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				// recover() returns nil unless we are actually unwinding a
				// panic, and only works when called directly inside a deferred
				// function — not from a helper it calls.
				rv := recover()
				if rv == nil {
					return
				}

				// http.ErrAbortHandler is the documented way for a handler to
				// abandon a response deliberately. Swallowing it here would
				// convert an intentional abort into a logged 500.
				if rv == http.ErrAbortHandler {
					panic(rv)
				}

				LoggerFrom(r.Context()).Error("panic recovered",
					slog.Any("panic", rv),
					slog.String("stack", string(debug.Stack())))

				// The handler may already have written a status, in which case
				// the header block is flushed and this write does nothing
				// useful — but attempting it is still right for the common case
				// where the panic happened before anything was sent.
				writeError(w, r, &apiError{
					status:  http.StatusInternalServerError,
					code:    CodeInternal,
					message: "internal error",
				})
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds how long a request may run.
//
// This sets a deadline on the request context rather than using
// http.TimeoutHandler, and the difference is the whole point: a context deadline
// propagates into pgx, so an abandoned request actually stops querying and
// returns its pooled connection. http.TimeoutHandler only stops *waiting* — it
// writes its own 503 and moves on while the handler goroutine and its database
// query carry on running, which under load is how a pool gets exhausted by work
// nobody is listening for.
//
// The trade-off: a handler that ignores its context will not be interrupted by
// this. Every path here reaches the database through a context-aware call, so
// that is not a live risk, but it is the reason to keep domain code honest about
// threading ctx through.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			// Always cancel, including on the success path. Skipping it leaks
			// the timer until it fires — go vet's lostcancel check exists
			// precisely because this is such a common omission.
			defer cancel()

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// MaxBodyBytes caps request body size.
//
// Without this an unauthenticated POST can stream gigabytes into memory before
// the JSON decoder notices. MaxBytesReader makes the read itself fail at the
// limit, so the cap costs nothing on well-behaved requests.
func MaxBodyBytes(n int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

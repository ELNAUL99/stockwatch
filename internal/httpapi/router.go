package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// server holds the handlers' dependencies. Unexported because nothing outside
// this package should construct one directly — NewRouter is the entry point,
// and keeping it that way means the middleware chain can never be skipped.
type server struct {
	svc    Service
	pinger Pinger
	// mux is held so handleNotFound can ask it whether the path would have
	// matched under a different method. See the comment there.
	mux *http.ServeMux
}

// Options configures the router.
type Options struct {
	Logger         *slog.Logger
	RequestTimeout time.Duration
	MaxBodyBytes   int64
	// CORSAllowedOrigins is the browser origins permitted to call the API. Empty
	// disables CORS entirely (same-origin only), which is the right default for a
	// service with no browser client.
	CORSAllowedOrigins []string
}

// Defaults for anything the caller leaves unset.
const (
	DefaultRequestTimeout = 10 * time.Second
	DefaultMaxBodyBytes   = 1 << 20 // 1 MiB
)

// NewRouter wires routes and middleware and returns the root handler.
//
// Returning http.Handler rather than *http.ServeMux is deliberate: the concrete
// type would let a caller register extra routes after the fact, bypassing the
// middleware chain applied below. Handing back the wrapped handler makes that
// impossible.
func NewRouter(svc Service, pinger Pinger, opts Options) http.Handler {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}

	mux := http.NewServeMux()
	s := &server{svc: svc, pinger: pinger, mux: mux}

	// Go 1.22 method-and-wildcard patterns. Before this release, ServeMux
	// matched prefixes only, so every handler began with a method switch and a
	// manual path split — which is the single biggest reason Go services
	// reached for chi or gorilla/mux. With these patterns a third-party router
	// buys very little.
	//
	// Two things worth knowing about the matching rules:
	//   - A pattern with a method matches only that method. ServeMux answers 405
	//     with an Allow header automatically — but ONLY while no other pattern
	//     matches the path, and the "/" catch-all registered below matches
	//     everything. Registering it therefore disables the built-in 405, which
	//     handleNotFound has to reconstruct. That is the price of keeping every
	//     error response inside the JSON contract.
	//   - The more specific pattern wins regardless of registration order, so
	//     "/products/{sku}/sales" is not shadowed by "/products/{sku}".
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	mux.HandleFunc("GET /products", s.handleListProducts)
	mux.HandleFunc("GET /products/{sku}", s.handleGetProduct)
	mux.HandleFunc("POST /products/{sku}/sales", s.handleRecordSale)
	mux.HandleFunc("GET /products/{sku}/recommendation", s.handleRecommendation)

	mux.HandleFunc("POST /recommendations/batch", s.handleBatch)

	// "/" catches everything unmatched. Without it, ServeMux's built-in 404 is
	// plain text, which would break the promise that every error response has
	// the same JSON shape.
	mux.HandleFunc("/", s.handleNotFound)

	// Outermost first. Order is load-bearing:
	//
	//   RequestID  must be first so every later layer can log with the ID.
	//   Logging    wraps Recover so the access line records the 500 that
	//              Recover produced, rather than being skipped by the panic.
	//   Recover    wraps Timeout and the handler, so a panic anywhere inside
	//              becomes a response instead of a dead process.
	//   Timeout    is innermost of the four so its deadline covers only handler
	//              work, not the logging that reports on it.
	// CORS sits just inside Logging so a preflight still gets an access-log line
	// and a request ID, but outside Recover/Timeout/handler so an OPTIONS
	// short-circuits before touching the mux. Only added when origins are
	// configured, so a same-origin deployment carries no CORS headers at all.
	mws := []Middleware{
		RequestID(opts.Logger),
		Logging(),
	}
	if len(opts.CORSAllowedOrigins) > 0 {
		mws = append(mws, CORS(opts.CORSAllowedOrigins))
	}
	mws = append(mws,
		Recover(),
		Timeout(opts.RequestTimeout),
		MaxBodyBytes(opts.MaxBodyBytes),
	)

	return Chain(mws...)(mux)
}

// handleNotFound keeps unmatched routes inside the JSON error contract, and
// distinguishes "no such path" from "wrong method for this path".
//
// Registering a "/" catch-all suppresses ServeMux's automatic 405, because the
// catch-all matches before the mux concludes that nothing did. To recover the
// distinction we re-ask the mux the same request under each other method: if one
// of them resolves to a real pattern, the path exists and only the method was
// wrong. That is a 405 with an Allow header, not a 404 — the difference between
// telling a client "you have the wrong URL" and "you have the wrong verb".
func (s *server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if allowed := s.allowedMethods(r); len(allowed) > 0 {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		writeError(w, r, &apiError{
			status: http.StatusMethodNotAllowed,
			code:   CodeMethodNotAllowed,
			message: fmt.Sprintf("%s is not allowed on %s; allowed: %s",
				r.Method, r.URL.Path, strings.Join(allowed, ", ")),
		})
		return
	}

	writeError(w, r, &apiError{
		status:  http.StatusNotFound,
		code:    CodeNotFound,
		message: fmt.Sprintf("no such endpoint: %s %s", r.Method, r.URL.Path),
	})
}

// probeMethods are the verbs handleNotFound tries. Only those this API actually
// registers need listing — a method absent from every pattern can never produce
// a match, so including it would just cost a lookup.
var probeMethods = []string{http.MethodGet, http.MethodPost}

// allowedMethods returns the methods that would match this path.
func (s *server) allowedMethods(r *http.Request) []string {
	var allowed []string
	for _, method := range probeMethods {
		if method == r.Method {
			continue
		}

		// Clone rather than mutate: the original request is still in flight and
		// shares its body and context with the caller.
		probe := r.Clone(r.Context())
		probe.Method = method

		// ServeMux.Handler reports the pattern it matched. A match on "/" is
		// the catch-all matching again, which tells us nothing.
		if _, pattern := s.mux.Handler(probe); pattern != "" && pattern != "/" {
			allowed = append(allowed, method)
		}
	}
	return allowed
}

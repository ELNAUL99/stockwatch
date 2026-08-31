# stockwatch

Decides what a grocery store should reorder, when, and how much. Given what a
product sold, what is on the shelf, and how long the vendor takes to deliver, it
returns a concrete order quantity — and tells the buyer which real-world limit
changed that number when the arithmetic alone would have said otherwise.

## Live demo

| | URL |
|---|---|
| **Dashboard** | https://stockwatch-web-g40n.onrender.com |
| **API** | https://stockwatch-api-v2ok.onrender.com |

Deployed on Render: a Go web service, a React/Vite static site, and a private
Postgres reachable only over Render's internal network. The API seeds itself on
boot (`./seed && ./app`) with history ending yesterday, so the demo data never
falls outside the estimation window. Both run on the free tier and spin down
after ~15 minutes idle — the first request then takes 30–60s to wake the service.

Try the API directly:

```bash
curl -s https://stockwatch-api-v2ok.onrender.com/products/BAGEL-PLAIN-6PK/recommendation
```

---

## Quickstart

Everything below runs from a clean clone. Docker is the only requirement.

```bash
git clone <this-repo> && cd stockwatch
docker compose up --build --wait
docker compose exec stockwatch /usr/local/bin/seed
```

The service migrates itself on boot, so there is no separate migrate step. If you
have Go installed, `make seed` does the same thing from the host.

`make seed` loads 21 grocery SKUs with 60 days of synthetic sales, including
deliberate stockouts, weekend spikes, and a long-tail item that mostly sells
nothing. Then:

```bash
curl -s localhost:8080/products/BAGEL-PLAIN-6PK/recommendation | jq
```

```json
{
  "sku": "BAGEL-PLAIN-6PK",
  "recommended_quantity": 72,
  "safety_stock": 12.52,
  "reorder_point": 63.42,
  "target_level": 88.88,
  "average_daily_demand": 25.45,
  "current_on_hand": 14,
  "current_on_order": 0,
  "ordering_is_inhibited": false,
  "constraints": [
    {
      "name": "case_size",
      "original_quantity": 75,
      "final_quantity": 84,
      "reason": "rounded up 74.9 to 84 units (7 cases of 12)"
    },
    {
      "name": "shelf_life",
      "original_quantity": 84,
      "final_quantity": 72,
      "reason": "reduced 84 to 72 units: a full case of 12 exceeds the 76.4 units sellable within 3-day shelf life"
    }
  ]
}
```

Two constraints fired, and the response says so. The formula wanted 74.9 units;
cases pushed that to 84; the three-day shelf life pulled it back to 72. A buyer
looking at "72" can see exactly why it is not 75.

> **On the exact figures.** The seed is deterministic given a PRNG seed, but the
> history always ends *yesterday* — so the day-of-week alignment of the window
> shifts with the date you run it, and your decimals will differ from those above
> by a fraction. The structure does not move: the same constraints fire, on the
> same SKUs, to the same order quantity.
>
> The seeder does take `-end-date` to pin the window, but do not use it to match
> this README: history that ends more than 28 days ago falls outside the
> estimation window entirely, and every SKU then answers 422. The seeder warns
> loudly if you do it anyway. (It warns because I did it, and shipped a README
> whose headline `curl` returned an error.)
>
> For figures that never move, the tests use a fixed window internally:
> `go test ./cmd/seed/ -run TestCensoringChangesTheAnswer -v`

### More interesting SKUs

Each of these is in the catalogue to make one specific behaviour visible:

| SKU | Order | What it shows |
| --- | --- | --- |
| `AVOCADO-HASS-EA` | 144 | A five-day supply gap inside the estimation window |
| `CROISSANT-BUTTER-4PK` | 30 | Two-day shelf life fighting a six-unit case — three constraints fire |
| `PASTA-PENNE-500G` | 48 | A 48-unit vendor minimum lifting a 24-unit order |
| `WATER-SPARKLING-12PK` | **0** | Badly overstocked: holds, and says why |
| `TRUFFLE-OIL-100ML` | 6 | Long tail at 0.37 units/day — one case or nothing |
| `KIMCHI-JAR-400G` | — | **422**: listed yesterday, one day of history |

```bash
curl -s localhost:8080/products/CROISSANT-BUTTER-4PK/recommendation | jq .constraints
```

```json
[
  { "name": "shelf_life", "reason": "capped at 32 units: 2-day shelf life sells only 32.2 units at 16.08 units/day" },
  { "name": "case_size",  "reason": "rounded up 32.2 to 36 units (6 cases of 6)" },
  { "name": "shelf_life", "reason": "reduced 36 to 30 units: a full case of 6 exceeds the 32.2 units sellable within 2-day shelf life" }
]
```

The shelf life appears twice on purpose — once before case rounding and once
after, because rounding *up* to a whole case can push an order back over the
limit the first check just enforced.

```bash
curl -s -X POST localhost:8080/recommendations/batch \
  -H 'Content-Type: application/json' \
  -d '{"skus":["BAGEL-PLAIN-6PK","AVOCADO-HASS-EA","KIMCHI-JAR-400G"]}' | jq
```

A batch is two queries regardless of size, and one bad SKU does not sink the
rest — failures travel alongside successes in the response body.

---

## The replenishment model

Four numbers, computed in order.

```
safety_stock  = z × σ × √(lead_time + review_period)
reorder_point = avg_daily_demand × lead_time + safety_stock
target_level  = avg_daily_demand × (lead_time + review_period) + safety_stock
order_units   = target_level − (on_hand + on_order)
```

Then: hold entirely if `on_hand + on_order > reorder_point`; otherwise apply the
shelf life cap, round up to a whole case, re-check the shelf life, and lift to
the vendor minimum.

**Why `√(lead_time + review_period)`.** Demand variance accumulates over the
window you are exposed to, and standard deviations add in quadrature, not
linearly. Doubling your exposure multiplies the risk by √2, not 2 — which is why
a long lead time hurts less than intuition suggests, and why a lead time that
becomes *unpredictable* hurts far more than one that is merely long.

**Why `lead_time` for the reorder point but `lead_time + review_period` for the
target.** The reorder point answers "will I run out before the delivery lands?"
— that exposure is the lead time. The target answers "how much should I bring in
so I last until the delivery after that?" — which also spans the gap until the
next review.

### Worked example: BAGEL-PLAIN-6PK

The last 28 days of sales for plain bagels, straight out of the seeded database:

```
2026-07-28 Tue   25      2026-08-11 Tue   20
2026-07-29 Wed   29      2026-08-12 Wed   20
2026-07-30 Thu   24      2026-08-13 Thu   16
2026-07-31 Fri   27      2026-08-14 Fri   23
2026-08-01 Sat   15  ←   2026-08-15 Sat   44
2026-08-02 Sun    0  ←   2026-08-16 Sun   41
2026-08-03 Mon    0  ←   2026-08-17 Mon   25
2026-08-04 Tue   20      2026-08-18 Tue   21
2026-08-05 Wed   27      2026-08-19 Wed   23
2026-08-06 Thu   25      2026-08-20 Thu   29
2026-08-07 Fri   25      2026-08-21 Fri   22
2026-08-08 Sat   39      2026-08-22 Sat   30
2026-08-09 Sun   33      2026-08-23 Sun   30
2026-08-10 Mon   21      2026-08-24 Mon   15

← = stockout: the recorded figure is a lower bound on demand
```

```sql
SELECT sales_date, units_sold, stockout_occurred FROM sales_days
WHERE sku = 'BAGEL-PLAIN-6PK' ORDER BY sales_date DESC LIMIT 28;
```

**Step 1 — drop the censored days.** Three days are marked. Saturday the 1st
sold 15 and then ran out; the Sunday and Monday after it opened with an empty
shelf and sold nothing at all. None of the three measured demand, so all three
are excluded. That leaves **25 usable observations**.

Note the two zeros. They are not quiet days — they are days when nobody *could*
buy a bagel. Feeding them into the average as though they were genuine zero
demand is the single most damaging thing a replenishment system can do, and it
is what the section below is about.

**Step 2 — remove day-of-week structure.** Sundays run about 1.43× a typical
day, Mondays about 0.75×. Each observation is divided by its weekday factor
before smoothing, so a busy Sunday is not mistaken for rising demand.

**Step 3 — smooth, weighting recent days more.** Exponential smoothing with
α = 0.10, giving an effective half-life of about a week. Fast enough to follow a
real shift within a fortnight, slow enough to ignore one loud Saturday.

```
avg_daily_demand = 25.45 units/day
σ                =  4.38
```

**Step 4 — the four numbers.** z = 1.65 (≈95% cycle service level), lead time
2 days, review period 1 day:

```
safety_stock  = 1.65 × 4.38 × √3            = 12.52
reorder_point = 25.45 × 2 + 12.52           = 63.42
target_level  = 25.45 × 3 + 12.52           = 88.88
position      = 14 on hand + 0 on order     = 14
```

14 is below the reorder point of 63.42, so we order.

```
order_units = 88.88 − 14 = 74.88
```

**Step 5 — apply the real world.**

| Step | Units | Why |
| --- | --- | --- |
| Formula | 74.88 | |
| Shelf life cap | 74.88 | 3 days × 25.45/day = 76.4 sellable — does not bind |
| Round to cases | **84** | 7 × 12; a partial case cannot be ordered |
| Re-check shelf life | **72** | 84 > 76.4 sellable, so step down one case |

**Order 72 units — six cases.** That is the `recommended_quantity` in the
response at the top of this README.

Note step 5's last row. Rounding *up* to a case can push an order past what the
shelf life allows, so the cap has to be re-checked after rounding, not before.
Checking once, before rounding, would have shipped 84 units of bagels into a
store that can sell 77 before they go stale. Seven units of waste, every order,
invisibly.

---

## Why censored demand matters

**A day that sold 12 units and then hit zero stock is not evidence that demand
was 12. It is evidence that demand was *at least* 12.**

This is the difference between a sales log and a demand estimate, and it is
where naive replenishment systems fail in a way that is very hard to see from
the inside.

The failure is self-reinforcing. A SKU sells out. The stockout days record
artificially low sales. Those low numbers drag the average down. The lower
average produces a smaller order. The smaller order sells out sooner. Each cycle
the system becomes more confident that a product nobody can buy is a product
nobody wants — and the evidence it uses to justify that is evidence it created
itself.

**What this service does:** every sales day carries a `stockout_occurred` flag,
and flagged days are excluded from the demand estimate entirely.

### Before and after

Hass avocados, from the seeded data. A five-day supply gap falls inside the
28-day window.

| | Demand estimate | σ | Days used |
| --- | --- | --- | --- |
| **Stockout days excluded** | **41.79 /day** | 6.97 | 24 |
| Stockout days averaged in | 37.81 /day | 13.93 | 28 |

Ignoring availability **understates demand by 9.5%** and **doubles σ**.

The second number is the one people miss. The artificial drop to zero and back
does not just lower the average — it looks like enormous volatility. But that is
not volatility in what customers wanted. It is the variance of *our own supply
failures*, being fed into a safety-stock formula as though it were uncertainty
about customers.

### The bias does not have a consistent sign

Here is the part that makes this genuinely dangerous rather than merely wrong.
The two errors push in opposite directions:

- the depressed mean **lowers** the target level
- the inflated σ **raises** safety stock

Which one wins depends on the SKU's lead time and service level. For these
avocados the inflated σ wins outright, and the naive estimate orders **more**,
not less:

| | Reorder point | Order |
| --- | --- | --- |
| **Stockout days excluded** | 153.9 | **144 units** |
| Stockout days averaged in | 170.5 | 168 units |

The blind version over-orders by 24 units *and* triggers 17 units earlier — it is
carrying safety stock against volatility that does not exist, then calling that
prudence.

That is worse than a consistent bias, not better. A buyer can learn to correct
for "the system always orders 10% light". Nobody can learn to correct for an
error whose sign changes depending on the item, which is what happens once mean
and variance are distorted in opposite directions.

Reproduce both tables:

```bash
go test ./cmd/seed/ -run TestCensoringChangesTheAnswer -v
```

### Excluding, not uplifting

Two standard approaches exist. This service **excludes** censored days rather
than **uplifting** them to an estimated true value.

**Why.** Uplifting means inventing a number — you know demand was *at least* 12,
so you scale to 12/0.6 = 20 using an assumed fill rate. That assumed rate is
itself unknowable, and the invented figure then flows into both the mean and the
variance as though it had been measured. Excluding throws away information but
never fabricates any. The failure mode is honest: with too few real observations
the service returns **422** and says so, rather than confidently ordering against
a number it made up.

**The trade-off, stated plainly.** A SKU that is *always* out of stock has no
uncensored days, so this approach yields nothing and returns an error — exactly
when a recommendation would be most valuable. Uplifting would produce a number.
I would rather the system say "I cannot tell you" than quietly guess, but this is
a real cost and a reasonable person could choose the other way. At scale I would
fit a censored-demand model (Tobit, or an EM loop over the truncation points),
which uses the censored days as the inequalities they genuinely are instead of
either discarding or inventing.

### A bug this caught

The schema originally carried this constraint:

```sql
CHECK (NOT stockout_occurred OR units_sold > 0)
```

justified as *"if the shelf was empty all day, there were no sales to truncate."*

That reasoning is wrong, and it inverts the entire feature. Two rows look
identical in `units_sold` and mean opposite things:

| Row | Meaning |
| --- | --- |
| `units_sold = 0`, `stockout = false` | In stock all day, nobody bought it. **Genuine zero demand** — real evidence, must be kept. |
| `units_sold = 0`, `stockout = true` | Shelf empty at opening. Sold nothing because we *had* nothing. **The most censored observation possible.** |

The constraint forced the second to be stored as the first, feeding a fabricated
zero straight into the mean — precisely the failure this service exists to
prevent. It survived three phases of development behind a confident comment, and
was caught only when the seed generator produced real empty-shelf days and the
loader rejected them.

Look at the bagel history above: `2026-06-07`, `08` and `09` are exactly those
rows. Under the old constraint they would have been recorded as three ordinary
zero-demand days.

The constraint is gone. `migrations/0001_init.up.sql` now carries a comment
explaining why no such constraint exists, and
`TestFullyCensoredDayIsStorable` asserts the row round-trips.

---

## Design decisions and trade-offs

**Domain logic imports nothing but the standard library.** `internal/inventory`
defines the interfaces it needs (`ProductStore`, `SalesStore`); `internal/storage`
implements them. The dependency arrow points inward and never outward. This is
enforced by a test, not a comment — `architecture_test.go` parses the package's
imports and fails on any non-stdlib one. Go rejects import *cycles* at compile
time but would happily allow `inventory` to import `storage` one-directionally,
which is the mistake this catches.

**`Service` lives in the domain package**, despite taking a `context.Context` and
calling out to stores — so it is not "pure" the way the calculation files are.
It is there because it imports nothing beyond stdlib, which is the property that
actually matters. The orthodox alternative is a separate `internal/app` package.
I would take it the moment a second domain lands; for one domain it is a package
boundary that buys nothing. **This is the layering choice most open to challenge.**

**Standard library HTTP routing.** Go 1.22's `ServeMux` handles
`GET /products/{sku}` patterns and method matching, so chi and gorilla/mux buy
very little now. One consequence is documented in `router.go`: registering a `/`
catch-all to keep 404s inside the JSON error contract *disables* the built-in
405 handling, which `handleNotFound` then has to reconstruct by re-probing the
mux. A third-party router would have given both for free.

**A hand-rolled migrator, ~90 lines.** The brief allowed one dependency and pgx
spent it. Ordering files, recording what ran, wrapping each in a transaction, and
taking an advisory lock so concurrent replicas do not race is genuinely small.
What was given up: down-migration sequencing, dirty-state recovery, and a CLI
that works without the app binary. With a second datastore, I would take
golang-migrate.

**Per-request context deadlines, not `http.TimeoutHandler`.** `TimeoutHandler`
stops *waiting* but leaves the handler goroutine and its database query running.
Under load that exhausts the pool with work nobody is listening for. A context
deadline propagates into pgx, so an abandoned request actually stops querying and
returns its connection.

**Reported floats are rounded to two decimals.** `safety_stock: 4.418722351095998`
is unreadable and makes golden files brittle. Rounding happens last, after every
decision — the arithmetic uses full precision, only the reported figures are
trimmed.

**Stockouts in the seed data are simulated, never written.** The generator tracks
on-hand stock and sells against it; a day is censored only when the shelf
genuinely empties. Faking the flag would put stockouts wherever I chose;
simulating puts them wherever the supply chain failed. It also means the seed
holds ground truth — the demand we would have seen with infinite stock — which is
what makes the before/after above measured rather than illustrative.

**`go.mod` requires Go 1.25, not 1.22.** testcontainers-go pulls the Docker
client tree, which requires ≥1.23 throughout. A working `go 1.22` build exists
with pgx v5.6.0, but it collapses the moment testcontainers goes in.

### What I would do differently at scale

**The recommendation is stateless and recomputed per request.** Fine for
thousands of SKUs; wrong for millions. Demand statistics change slowly — a
nightly batch writing `demand_stats` would turn the read path into a single
indexed lookup, at the cost of staleness that would need its own monitoring.

**`review_period_days` is per-product but should be per-vendor-schedule.** Real
replenishment is driven by delivery calendars — "this vendor delivers Tuesday and
Friday" — not a scalar day count. The current model cannot express "order Monday
to cover through Thursday."

**Lead time is a constant, and in reality it is a distribution.** The formula
assumes deterministic lead time, so all variability lands in demand. A vendor
whose delivery is 2 days ± 3 is far more dangerous than one at a reliable 5, and
this model cannot see the difference. The fix is
`σ_combined = √(lead × σ_demand² + demand² × σ_lead²)` — mechanically easy, but
it needs lead time variance actually measured from receipts.

**Batch endpoint capped at 500 SKUs and computed synchronously.** A full chain
replenishment run is hundreds of thousands. That wants a job queue and a results
table, not an HTTP request.

**No authentication.** Deliberately out of scope; this would sit behind an
internal gateway. Worth saying rather than leaving a reviewer to wonder.

---

## What I would build next

**A Kafka consumer for real-time sales events.** Sales arrive today by `POST`
per day per SKU, which is a nightly batch wearing an API. A consumer reading a
POS topic would let the stock position track intraday and, more importantly,
detect a stockout *as it happens* rather than inferring it after the fact. That
turns `stockout_occurred` from a field somebody remembers to set into something
observed. The upsert is already idempotent, so at-least-once delivery is safe;
the work is consumer-group management, ordering per SKU, and a dead-letter path.

**Vendor-level order batching.** Right now every SKU is decided in isolation, but
purchase orders are placed per vendor and vendors have terms that only make sense
across a whole order — free delivery over €500, a full-pallet discount, a minimum
drop size. A SKU that says "order nothing today" should sometimes be topped up
anyway because a truck is going to that vendor regardless, and the marginal cost
of adding a case is near zero. That is a per-vendor knapsack over the SKUs whose
positions are near their reorder points, and it is where the money actually is.

**Cost optimisation across substitutable products.** Six brands of oat milk are
not six independent demand streams. When one runs out most of its demand moves to
the others rather than leaving the store, which means the true cost of a stockout
is much lower for a substitutable SKU than for a unique one — and a service level
of 95% on every variant is over-buying. Modelling substitution matrices would let
safety stock be allocated across a category by marginal value rather than
uniformly per SKU.

---

## API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/healthz` | Liveness. Checks nothing — see below. |
| `GET` | `/readyz` | Readiness. Pings the database. |
| `GET` | `/products/{sku}` | Product terms and current stock position |
| `POST` | `/products/{sku}/sales` | Record one day of sales |
| `GET` | `/products/{sku}/recommendation` | The replenishment decision |
| `POST` | `/recommendations/batch` | Many SKUs, two queries |

**`/healthz` deliberately checks nothing.** If liveness touched the database, a
brief Postgres outage would make Kubernetes kill and restart every replica —
turning a recoverable dependency blip into a full outage with a thundering-herd
reconnect on the far side. Readiness pulls a replica out of the load balancer;
liveness kills it. Only the first is the right response to a database problem.

### Recording a sale

```bash
curl -s -X POST localhost:8080/products/BAGEL-PLAIN-6PK/sales \
  -H 'Content-Type: application/json' \
  -d '{"date":"2026-07-01","units_sold":24,"stockout_occurred":false}'
```

`units_sold: 0` with `stockout_occurred: true` is valid and meaningful — it
records a shelf that was empty at opening. See above for why that row matters.

### Errors

Every error, from any route, has the same shape:

```json
{
  "error": {
    "code": "insufficient_history",
    "message": "estimate demand for KIMCHI-JAR-400G: fewer than two uncensored observations",
    "request_id": "8a3f1c9d2e5b7041"
  }
}
```

`code` is a stable enum clients may branch on; `message` is for humans;
`request_id` matches a log line and is echoed in the `X-Request-Id` header.

| Status | When |
| --- | --- |
| 400 | Malformed body, unknown field, bad date |
| 404 | Unknown SKU, or unknown route |
| 405 | Wrong method, with an `Allow` header |
| 415 | `Content-Type` is not `application/json` |
| 422 | SKU exists, request was fine, **not enough data to compute** |
| 500 | Anything unanticipated — generic message, full detail to logs |
| 503 | `/readyz` only: database unreachable |
| 504 | Request exceeded its deadline |

422 rather than 404 or 400 for insufficient history is a deliberate choice: the
request was well-formed and the SKU exists, so the fix is "record more sales",
not "correct your request".

---

## Layout

```
cmd/server/      wiring, config, graceful shutdown
cmd/migrate/     apply and roll back migrations
cmd/seed/        demo catalogue and sales simulator
internal/inventory/   domain logic — stdlib only, enforced by test
internal/storage/     Postgres adapter
internal/httpapi/     handlers, middleware, JSON errors
migrations/           numbered SQL, embedded in the binary
```

---

## Development

```bash
make test         # unit tests only, no Docker needed
make test-all     # everything, including Postgres integration tests
make lint         # go vet + staticcheck
make cover        # coverage on the domain package
make seed         # load the demo catalogue
make migrate      # apply migrations
make              # list every target
```

Domain coverage is **96.3%**, enforced at 80% in CI. Coverage in the HTTP layer
is deliberately not chased — those tests target status mapping, the error
envelope, request-ID propagation and panic containment, not line count.

Integration tests run against a real Postgres and are skipped by `-short`.
Gating on `testing.Short()` rather than a build tag is deliberate: with a build
tag, a plain `go test ./...` silently never runs them. This way, forgetting a
flag runs *more* tests, not fewer.

By default they start a throwaway container via testcontainers. Point them at a
Postgres you already have instead — useful on any machine without a working
Docker daemon, and considerably faster:

```bash
createdb stockwatch_test
TEST_DATABASE_URL='postgres://localhost:5432/stockwatch_test?sslmode=disable' \
  go test -race ./internal/storage/
```

The tests are identical either way: they migrate on entry and truncate between
cases, so they neither assume an empty database nor leave one behind.

### Configuration

Everything is environment-driven with working defaults; only `DATABASE_URL` is
required. Startup fails fast and reports **every** problem at once rather than
one per restart:

```
ERROR fatal error="invalid configuration:
  - DATABASE_URL is required (e.g. postgres://user:pass@host:5432/stockwatch?sslmode=disable)
  - LOG_FORMAT must be \"json\" or \"text\", got \"xml\"
  - DEMAND_ALPHA must be in (0, 1], got 9"
```

Cross-field rules are checked too — `WRITE_TIMEOUT` must exceed
`REQUEST_TIMEOUT`, or the server severs the connection before a slow handler can
report its own 504. See `.env.example` for the full list.

## Continuous integration

The same checks run in two systems, on purpose:

| | Runs | Purpose |
|---|---|---|
| **GitHub Actions** (`.github/workflows/ci.yml`) | tidy · gofmt · vet · staticcheck · **full** `go test -race` (incl. Postgres integration) · coverage gate · Docker build + smoke test | The gate on every push and PR |
| **Jenkins** (`Jenkinsfile`) | the same, with `go test -short -race`; the Docker image stage self-skips when no daemon is present | Portable pipeline that runs without Docker |

The deliberate difference is test scope. GitHub's runners have a Docker daemon,
so Actions runs the testcontainers-backed Postgres tests. The Jenkins pipeline is
written to need no Docker at all, so it runs `-short` (unit and adapter tests)
and leaves the integration suite to Actions. To run the full suite in Jenkins
too, give the node a Postgres, set `TEST_DATABASE_URL`, and drop `-short`.

Both express one idea two ways. Where Actions adds Go with a `setup-go` step, the
`Jenkinsfile` uses Jenkins' own **Go tool installer** — the toolchain is
downloaded by Jenkins, not carried in a container.

### Running the Jenkins pipeline locally — no Docker required

Run Jenkins natively (no `docker run`). On macOS:

```bash
brew install jenkins-lts
brew services start jenkins-lts   # serves http://localhost:8080
```

Or, with any Java 17+, run the war directly: `java -jar jenkins.war`.

Then, in the UI:

1. **Manage Jenkins → Plugins** — install **Go** (and the Pipeline plugins, which
   ship with a standard install).
2. **Manage Jenkins → Tools → Go installations** — add one named exactly
   `go-1.25`, tick *Install automatically*, choose 1.25.x. Jenkins downloads the
   toolchain; nothing to install by hand.
3. **New Item → Pipeline** → *Pipeline script from SCM* → point at this repo.
   Jenkins reads the `Jenkinsfile` from the root.

The `-race` stage needs a C compiler on the node (clang via Xcode command-line
tools on macOS, gcc on Linux). The final Docker-image stage runs only if a daemon
is reachable and skips cleanly otherwise, so a Docker-free box still goes green.

Validate the syntax before a run with the declarative linter (needs `JENKINS_URL`
set and the Jenkins CLI jar):

```bash
java -jar jenkins-cli.jar -s "$JENKINS_URL" declarative-linter < Jenkinsfile
```

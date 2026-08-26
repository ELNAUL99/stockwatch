// Command seed loads a demo catalogue with synthetic sales history.
//
// The point is not to fill tables — it is to make the decision logic visible.
// Every SKU in the catalogue is chosen to exercise a specific branch: censored
// demand, a binding shelf life, a vendor minimum, an overstocked position, a
// long-tail item that mostly sells nothing, and one listed yesterday with too
// little history to compute anything at all.
//
// Deterministic by default. The same -seed produces byte-identical data, so the
// worked example in the README stays true and a demo cannot surprise you.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ELNAUL99/stockwatch/internal/inventory"
	"github.com/ELNAUL99/stockwatch/internal/storage"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "seed failed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		days    = flag.Int("days", 60, "days of sales history per SKU")
		seed    = flag.Uint64("seed", 20260601, "PRNG seed; the same value reproduces the same data")
		reset   = flag.Bool("reset", true, "delete existing history for these SKUs first")
		quiet   = flag.Bool("quiet", false, "suppress the per-SKU summary table")
		timeout = flag.Duration("timeout", 2*time.Minute, "overall timeout")
		endFlag = flag.String("end-date", "",
			"last day of history as YYYY-MM-DD (default: yesterday)")
	)
	flag.Parse()

	if *days < 14 {
		return fmt.Errorf("-days is %d; fewer than 14 leaves too little history "+
			"for the estimator to say anything interesting", *days)
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	// Seeding an unmigrated database produces a wall of "relation does not
	// exist". Migrating first makes `make seed` work from a clean clone.
	if err := storage.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	store := storage.New(pool)

	// math/rand/v2's explicit source. The older math/rand had a package-level
	// global that any library could reseed from under you; v2 makes the
	// generator a value you own, which is what makes this reproducible.
	//
	// PCG rather than the default: it is seeded from two explicit uint64s, so
	// the seed is genuinely part of the output rather than mixed with process
	// state.
	rng := rand.New(rand.NewPCG(*seed, *seed^0x9e3779b97f4a7c15))

	// History ends yesterday. Ending today would mean seeding a partial day that
	// looks like a demand collapse to the estimator, since only part of the
	// day's sales exist.
	//
	// -end-date pins it instead. That matters more than it looks: the day-of-week
	// alignment of the whole window shifts with the end date, so the same -seed
	// run on a different day produces different numbers. Pinning both is what
	// makes the figures quoted in the README reproducible rather than
	// approximately true.
	endDate := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	if *endFlag != "" {
		parsed, err := time.ParseInLocation(time.DateOnly, *endFlag, time.UTC)
		if err != nil {
			return fmt.Errorf("-end-date must be YYYY-MM-DD, got %q", *endFlag)
		}
		endDate = parsed
	}

	results := make([]generated, 0, len(catalog))

	for _, s := range catalog {
		g := generate(s, *days, endDate, rng)

		if err := store.UpsertProduct(ctx, g.Product); err != nil {
			return fmt.Errorf("upsert %s: %w", s.SKU, err)
		}
		if *reset {
			if err := store.DeleteSalesHistory(ctx, s.SKU); err != nil {
				return err
			}
		}
		if err := store.RecordSalesDays(ctx, s.SKU, g.Days); err != nil {
			return err
		}

		results = append(results, g)
	}

	fmt.Printf("seeded %d SKUs with %d days of history ending %s (seed=%d)\n",
		len(results), *days, endDate.Format(time.DateOnly), *seed)

	// Loud warning when the data just written is already too old to be read.
	//
	// The estimator only looks at the most recent HISTORY_WINDOW_DAYS, so history
	// ending well in the past loads perfectly and is then invisible: every SKU
	// answers 422 and nothing explains why. Pinning -end-date to an old date for
	// reproducibility is exactly how you get there, and it cost me a README whose
	// headline curl returned an error.
	if staleness := int(time.Since(endDate).Hours() / 24); staleness > inventory.DefaultHistoryWindowDays {
		fmt.Fprintf(os.Stderr, "\nWARNING: this history ends %d days ago, but the "+
			"estimator only reads the most recent %d days.\n"+
			"Every SKU will return 422 (insufficient_history) until you reseed "+
			"without -end-date.\n",
			staleness, inventory.DefaultHistoryWindowDays)
	}

	if !*quiet {
		printSummary(results)
	}
	return nil
}

// printSummary renders what was generated and why it is interesting.
//
// text/tabwriter aligns columns without anyone counting spaces; it buffers
// output and pads on Flush, which is why the deferred-looking Flush at the end
// is mandatory rather than tidy.
func printSummary(results []generated) {
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SKU\tDAYS\tSOLD\tTRUE DEMAND\tLOST\tCENSORED\tON HAND\tNOTE")

	var totalLost, totalCensored int

	for _, g := range results {
		lost := g.TrueDemandTotal - g.SoldTotal
		totalLost += lost
		totalCensored += g.CensoredDays

		note := noteFor(g.Product.SKU)

		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n",
			g.Product.SKU,
			len(g.Days),
			g.SoldTotal,
			g.TrueDemandTotal,
			lost,
			g.CensoredDays,
			g.Product.OnHandUnits,
			note,
		)
	}
	_ = w.Flush()

	// The headline number. "Lost" is demand that walked out of the store, and it
	// is precisely the quantity a sales log cannot show you — every unit of it is
	// invisible in the units_sold column. This is the censored-demand problem
	// stated as an integer.
	fmt.Printf("\n%d units of demand were lost to stockouts across %d censored days.\n",
		totalLost, totalCensored)
	fmt.Println("Those units appear nowhere in the sales data. Excluding censored days")
	fmt.Println("from the mean is how the estimator avoids being fooled by their absence.")

	fmt.Println("\nTry:")
	fmt.Println("  curl -s localhost:8080/products/AVOCADO-HASS-EA/recommendation | jq")
	fmt.Println("  curl -s localhost:8080/products/CROISSANT-BUTTER-4PK/recommendation | jq")
	fmt.Println("  curl -s localhost:8080/products/WATER-SPARKLING-12PK/recommendation | jq")
	fmt.Println("  curl -s localhost:8080/products/KIMCHI-JAR-400G/recommendation | jq   # 422")
}

func noteFor(sku string) string {
	for _, s := range catalog {
		if s.SKU == sku {
			return s.Note
		}
	}
	return ""
}

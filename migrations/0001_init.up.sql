-- Product master data: the vendor terms and physical limits for an SKU.
-- These change rarely — a buyer renegotiates a case size a few times a year.
CREATE TABLE products (
    sku                     TEXT PRIMARY KEY,
    name                    TEXT        NOT NULL,

    -- Vendor terms.
    lead_time_days          INTEGER     NOT NULL,
    minimum_order_quantity  INTEGER     NOT NULL DEFAULT 0,
    case_size               INTEGER     NOT NULL DEFAULT 1,

    -- Operating parameters.
    review_period_days      INTEGER     NOT NULL DEFAULT 1,
    shelf_life_days         INTEGER     NOT NULL DEFAULT 0, -- 0 means non-perishable

    -- The z in safety_stock = z * sigma * sqrt(lead + review). NOT a percentage:
    -- 1.65 is roughly a 95% cycle service level, 2.33 roughly 99%.
    -- NUMERIC rather than DOUBLE PRECISION because this is an operator-tuned
    -- business parameter that people type in and read back; exact decimal
    -- round-tripping matters more than arithmetic speed on a single scalar.
    target_service_level    NUMERIC(5,3) NOT NULL DEFAULT 1.65,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Mirror inventory.Product.Validate at the storage boundary. Application
    -- validation catches bad input early with a good message; these catch bad
    -- data arriving by any other route — a migration, a manual fix, a future
    -- service. Cheap insurance for a table this small.
    CONSTRAINT products_lead_time_non_negative  CHECK (lead_time_days >= 0),
    CONSTRAINT products_moq_non_negative        CHECK (minimum_order_quantity >= 0),
    CONSTRAINT products_case_size_non_negative  CHECK (case_size >= 0),
    CONSTRAINT products_review_period_positive  CHECK (review_period_days >= 1),
    CONSTRAINT products_shelf_life_non_negative CHECK (shelf_life_days >= 0),
    CONSTRAINT products_service_level_non_negative CHECK (target_service_level >= 0)
);

-- Current stock position, split from products because the two have completely
-- different write patterns: master data is edited by a buyer a few times a year,
-- position is rewritten by every receipt, sale and count — many times a day.
-- Keeping them apart means position churn does not bloat the products table and
-- the two can be audited, cached and eventually sharded independently.
--
-- One row per SKU, enforced by making sku the primary key.
CREATE TABLE stock_positions (
    sku             TEXT PRIMARY KEY REFERENCES products(sku) ON DELETE CASCADE,
    on_hand_units   INTEGER     NOT NULL DEFAULT 0,
    on_order_units  INTEGER     NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT stock_on_hand_non_negative  CHECK (on_hand_units >= 0),
    CONSTRAINT stock_on_order_non_negative CHECK (on_order_units >= 0)
);

-- One row per SKU per day of sales.
--
-- stockout_occurred is the column that makes this a replenishment system rather
-- than a sales log. When true, units_sold is a LOWER BOUND on demand: we sold
-- everything we had and turned away an unknown number of customers. The demand
-- estimator excludes these rows rather than averaging them in, because including
-- a truncated observation biases demand downward and quietly under-orders the
-- exact SKUs that are already selling out.
CREATE TABLE sales_days (
    sku                 TEXT        NOT NULL REFERENCES products(sku) ON DELETE CASCADE,
    sales_date          DATE        NOT NULL,
    units_sold          INTEGER     NOT NULL,
    stockout_occurred   BOOLEAN     NOT NULL DEFAULT FALSE,
    recorded_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Composite primary key: one row per SKU per day. This also gives us the
    -- index the history-window query needs, because Postgres can range-scan a
    -- btree on its leading column (sku) plus a bound on the second (sales_date).
    -- A separate index on (sku, sales_date) would be redundant.
    PRIMARY KEY (sku, sales_date),

    CONSTRAINT sales_units_non_negative CHECK (units_sold >= 0)

    -- There is deliberately NO constraint forbidding units_sold = 0 alongside
    -- stockout_occurred = true, and the absence is the point.
    --
    -- A zero with the flag set and a zero without it are opposites:
    --
    --   units_sold = 0, stockout_occurred = false
    --       In stock all day, nobody bought it. Genuine zero demand, and real
    --       evidence the estimator must keep.
    --
    --   units_sold = 0, stockout_occurred = true
    --       The shelf was empty when the doors opened. We sold nothing because
    --       we had nothing. This is the MOST censored observation possible —
    --       every unit of that day's demand walked out unseen.
    --
    -- An earlier version of this schema required units_sold > 0 whenever the
    -- flag was set, on the reasoning that "no sales means nothing to truncate".
    -- That reasoning conflates the two rows above and forces the second to be
    -- stored as the first, which feeds a fabricated zero straight into the mean
    -- — the exact failure this service exists to avoid.
);

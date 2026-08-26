-- Reverse dependency order: sales_days and stock_positions both reference
-- products, so they must go first.
DROP TABLE IF EXISTS sales_days;
DROP TABLE IF EXISTS stock_positions;
DROP TABLE IF EXISTS products;

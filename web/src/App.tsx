import { useEffect, useState } from 'react';
import './App.css';

// These interfaces mirror the API's JSON exactly — snake_case, because the Go
// service tags its fields that way and the browser does no translation. A
// mismatch here does not fail loudly; it yields `undefined` and a blank card, so
// the names are load-bearing.
interface Constraint {
  name: string;
  original_quantity: number;
  final_quantity: number;
  reason: string;
}

interface Recommendation {
  sku: string;
  recommended_quantity: number;
  safety_stock: number;
  reorder_point: number;
  target_level: number;
  average_daily_demand: number;
  current_on_hand: number;
  current_on_order: number;
  ordering_is_inhibited: boolean;
  inhibit_reason?: string;
  constraints: Constraint[];
}

interface Product {
  sku: string;
  name: string;
  position: {
    on_hand_units: number;
    on_order_units: number;
    total_units: number;
  };
}

interface SKURow {
  sku: string;
  name: string;
  onHand: number;
  onOrder: number;
  total: number;
  recommendation?: Recommendation;
  error?: string;
  loading: boolean;
}

// Empty in dev — the Vite proxy (vite.config.ts) forwards /products to the local
// backend. In a static production build there is no proxy, so VITE_API_URL must
// point at the deployed API's origin.
const API_BASE = import.meta.env.VITE_API_URL || '';

// fmt guards every numeric render: a recommendation that failed to parse must
// degrade to "0.0" rather than throwing on undefined.toFixed and blanking the
// whole page, since there is no error boundary above the grid.
const fmt = (n: number | undefined, digits = 1) => (n ?? 0).toFixed(digits);

async function getProducts(): Promise<Product[]> {
  const response = await fetch(`${API_BASE}/products`);
  if (!response.ok) throw new Error(`products request failed (HTTP ${response.status})`);
  return response.json();
}

async function getRecommendation(sku: string): Promise<Recommendation> {
  const response = await fetch(`${API_BASE}/products/${sku}/recommendation`);
  const data = await response.json();
  if (!response.ok) {
    // The API's error envelope is {error:{code,message}}. Turn the two cases a
    // demo actually hits into short human labels; fall back to the raw message.
    const code = data?.error?.code;
    if (code === 'insufficient_history') throw new Error('Insufficient sales history to recommend');
    throw new Error(data?.error?.message || `HTTP ${response.status}`);
  }
  return data;
}

export default function App() {
  const [skus, setSkus] = useState<SKURow[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    loadData();
  }, []);

  async function loadData() {
    try {
      setLoading(true);
      setError(null);

      const products = await getProducts();
      setSkus(
        products.map((p) => ({
          sku: p.sku,
          name: p.name,
          onHand: p.position?.on_hand_units ?? 0,
          onOrder: p.position?.on_order_units ?? 0,
          total: p.position?.total_units ?? 0,
          loading: true,
        }))
      );

      const recs = await Promise.allSettled(
        products.map((p) => getRecommendation(p.sku).then((rec) => ({ sku: p.sku, rec })))
      );

      const bySku = new Map<string, PromiseSettledResult<{ sku: string; rec: Recommendation }>>();
      products.forEach((p, i) => bySku.set(p.sku, recs[i]));

      setSkus((prev) =>
        prev.map((row) => {
          const r = bySku.get(row.sku);
          if (r && r.status === 'fulfilled') {
            return { ...row, recommendation: r.value.rec, loading: false };
          }
          const reason = r && r.status === 'rejected' ? String(r.reason?.message ?? r.reason) : 'Failed to load';
          return { ...row, error: reason, loading: false };
        })
      );
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load products');
      setSkus([]);
    } finally {
      setLoading(false);
    }
  }

  if (error) {
    return (
      <div className="container">
        <div className="error-banner">
          <h2>⚠️ Connection Error</h2>
          <p>{error}</p>
          <p className="hint">The API may be waking from sleep — give it a moment and retry.</p>
          <button onClick={loadData}>Retry</button>
        </div>
      </div>
    );
  }

  const orderable = skus.filter((s) => s.recommendation && !s.recommendation.ordering_is_inhibited).length;

  return (
    <div className="container">
      <header className="header">
        <h1>📦 Stockwatch</h1>
        <p className="subtitle">Inventory Replenishment Dashboard</p>
        {!loading && skus.length > 0 && (
          <p className="summary">
            {skus.length} SKUs · {orderable} need reordering
          </p>
        )}
      </header>

      {loading && skus.length === 0 && (
        <div className="loading">
          <div className="spinner"></div>
          <p>Loading SKUs…</p>
        </div>
      )}

      <div className="skus-grid">
        {skus.map((sku) => {
          const rec = sku.recommendation;
          const inhibited = rec?.ordering_is_inhibited ?? false;
          const cls = sku.loading ? 'loading' : sku.error ? 'error' : inhibited ? 'inhibited' : 'active';
          return (
            <div key={sku.sku} className={`sku-card ${cls}`}>
              <div className="sku-header">
                <h3>{sku.name}</h3>
                <code className="sku-code">{sku.sku}</code>
              </div>

              <div className="position-section">
                <div className="metric">
                  <div className="label">On Hand</div>
                  <div className="value">{sku.onHand}</div>
                </div>
                <div className="metric">
                  <div className="label">On Order</div>
                  <div className="value">{sku.onOrder}</div>
                </div>
                <div className="metric">
                  <div className="label">Position</div>
                  <div className="value">{sku.total}</div>
                </div>
              </div>

              {sku.loading && <div className="spinner-small"></div>}

              {sku.error && (
                <div className="error-message">
                  <p>⚠️ {sku.error}</p>
                </div>
              )}

              {rec && (
                <>
                  <div className="reorder-section">
                    <div className="metric">
                      <div className="label">Avg Demand/day</div>
                      <div className="value">{fmt(rec.average_daily_demand)}</div>
                    </div>
                    <div className="metric">
                      <div className="label">Reorder Point</div>
                      <div className="value">{fmt(rec.reorder_point)}</div>
                    </div>
                    <div className="metric">
                      <div className="label">Target Level</div>
                      <div className="value">{fmt(rec.target_level)}</div>
                    </div>
                  </div>

                  <div className={`recommendation ${inhibited ? 'inhibited' : 'active'}`}>
                    {inhibited ? (
                      <>
                        <div className="status">🛑 Hold — no order</div>
                        {rec.inhibit_reason && <p className="reason">{rec.inhibit_reason}</p>}
                      </>
                    ) : (
                      <>
                        <div className="status">📦 Order</div>
                        <div className="quantity">{rec.recommended_quantity} units</div>
                        <div className="safety-stock">Safety stock: {fmt(rec.safety_stock, 2)}</div>
                      </>
                    )}
                  </div>

                  {rec.constraints && rec.constraints.length > 0 && (
                    <div className="constraints">
                      <div className="constraints-label">Constraints applied</div>
                      {rec.constraints.map((c, i) => (
                        <div key={i} className="constraint" title={c.reason}>
                          <span className="name">{c.name.replace(/_/g, ' ')}</span>
                          <span className="change">
                            {c.original_quantity} → {c.final_quantity}
                          </span>
                        </div>
                      ))}
                    </div>
                  )}
                </>
              )}
            </div>
          );
        })}
      </div>

      {skus.length === 0 && !loading && (
        <div className="empty-state">
          <p>
            No SKUs found. Seed the database first: <code>make seed</code>
          </p>
        </div>
      )}

      <footer className="footer">
        <p>
          Inventory replenishment with censored-demand estimation ·{' '}
          <a href="https://github.com/ELNAUL99/stockwatch" target="_blank" rel="noopener noreferrer">
            View on GitHub
          </a>
        </p>
      </footer>
    </div>
  );
}

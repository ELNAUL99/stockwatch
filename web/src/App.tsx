import { useEffect, useState } from 'react';
import './App.css';

interface Recommendation {
  sku: string;
  recommendedQuantity: number;
  reorderPoint: number;
  targetLevel: number;
  safetyStock: number;
  orderingIsInhibited: boolean;
  inhibitReason?: string;
  constraints: Array<{
    constraint: string;
    originalQuantity: number;
    constrainedQuantity: number;
  }>;
}

interface Product {
  sku: string;
  name: string;
  onHandUnits: number;
  onOrderUnits: number;
  leadTimeDays: number;
  reviewPeriodDays: number;
  caseSize: number;
  shelfLifeDays: number;
  targetServiceLevel: number;
}

interface SKUWithRec {
  sku: string;
  name: string;
  onHandUnits: number;
  onOrderUnits: number;
  recommendation?: Recommendation;
  error?: string;
  loading: boolean;
}

// Empty in dev — the Vite proxy (vite.config.ts) forwards /products to the
// local backend. In a static production build there is no proxy, so VITE_API_URL
// must point at the deployed API's origin.
const API_BASE = import.meta.env.VITE_API_URL || '';

async function getProducts(): Promise<Product[]> {
  const response = await fetch(`${API_BASE}/products`);
  if (!response.ok) throw new Error('Failed to fetch products');
  return response.json();
}

async function getRecommendation(sku: string): Promise<Recommendation> {
  const response = await fetch(`${API_BASE}/products/${sku}/recommendation`);
  if (!response.ok) throw new Error('Failed to fetch recommendation');
  return response.json();
}

export default function App() {
  const [skus, setSkus] = useState<SKUWithRec[]>([]);
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
      const skusWithRec: SKUWithRec[] = products.map((p) => ({
        sku: p.sku,
        name: p.name,
        onHandUnits: p.onHandUnits,
        onOrderUnits: p.onOrderUnits,
        loading: true,
      }));

      setSkus(skusWithRec);

      const recs = await Promise.allSettled(
        products.map((p) =>
          getRecommendation(p.sku).then((rec) => ({ sku: p.sku, rec }))
        )
      );

      setSkus((prev) =>
        prev.map((sku) => {
          const result = recs.find((r) => r.status === 'fulfilled' && (r.value as any).sku === sku.sku);
          if (result?.status === 'fulfilled') {
            return { ...sku, recommendation: (result.value as any).rec, loading: false };
          }
          return { ...sku, error: (result as any)?.reason?.message || 'Failed to load', loading: false };
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
          <p className="hint">Make sure the stockwatch server is running</p>
          <button onClick={loadData}>Retry</button>
        </div>
      </div>
    );
  }

  return (
    <div className="container">
      <header className="header">
        <h1>📦 Stockwatch</h1>
        <p className="subtitle">Inventory Replenishment Dashboard</p>
      </header>

      {loading && skus.length === 0 && (
        <div className="loading">
          <div className="spinner"></div>
          <p>Loading SKUs...</p>
        </div>
      )}

      <div className="skus-grid">
        {skus.map((sku) => (
          <div key={sku.sku} className={`sku-card ${sku.loading ? 'loading' : sku.error ? 'error' : sku.recommendation?.orderingIsInhibited ? 'inhibited' : 'active'}`}>
            <div className="sku-header">
              <h3>{sku.name}</h3>
              <code className="sku-code">{sku.sku}</code>
            </div>

            {sku.loading && <div className="spinner-small"></div>}

            {sku.error && <div className="error-message"><p>⚠️ {sku.error}</p></div>}

            {!sku.loading && !sku.error && sku.recommendation && (
              <>
                <div className="position-section">
                  <div className="metric">
                    <div className="label">On Hand</div>
                    <div className="value">{sku.onHandUnits}</div>
                  </div>
                  <div className="metric">
                    <div className="label">On Order</div>
                    <div className="value">{sku.onOrderUnits}</div>
                  </div>
                  <div className="metric">
                    <div className="label">Total Position</div>
                    <div className="value">{sku.onHandUnits + sku.onOrderUnits}</div>
                  </div>
                </div>

                <div className="reorder-section">
                  <div className="metric">
                    <div className="label">Reorder Point</div>
                    <div className="value">{sku.recommendation.reorderPoint.toFixed(1)}</div>
                  </div>
                  <div className="metric">
                    <div className="label">Target Level</div>
                    <div className="value">{sku.recommendation.targetLevel.toFixed(1)}</div>
                  </div>
                </div>

                <div className={`recommendation ${sku.recommendation.orderingIsInhibited ? 'inhibited' : 'active'}`}>
                  <div className="status">{sku.recommendation.orderingIsInhibited ? '🛑 Ordering Inhibited' : '📦 Order'}</div>
                  {!sku.recommendation.orderingIsInhibited && <div className="quantity">{sku.recommendation.recommendedQuantity} units</div>}
                  {!sku.recommendation.orderingIsInhibited && <div className="safety-stock">Safety stock: {sku.recommendation.safetyStock.toFixed(2)}</div>}
                  {sku.recommendation.orderingIsInhibited && sku.recommendation.inhibitReason && <p className="reason">{sku.recommendation.inhibitReason}</p>}
                </div>

                {sku.recommendation.constraints && sku.recommendation.constraints.length > 0 && (
                  <div className="constraints">
                    <div className="constraints-label">Constraints Applied:</div>
                    {sku.recommendation.constraints.map((c, i) => (
                      <div key={i} className="constraint">
                        <span className="name">{c.constraint}</span>
                        <span className="change">{c.originalQuantity} → {c.constrainedQuantity}</span>
                      </div>
                    ))}
                  </div>
                )}
              </>
            )}
          </div>
        ))}
      </div>

      {skus.length === 0 && !loading && (
        <div className="empty-state">
          <p>No SKUs found. Seed the database first: <code>make seed</code></p>
        </div>
      )}

      <footer className="footer">
        <p>
          Replenishment service for inventory management •
          <a href="https://github.com/ELNAUL99/stockwatch" target="_blank" rel="noopener noreferrer">
            View on GitHub
          </a>
        </p>
      </footer>
    </div>
  );
}

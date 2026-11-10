import { Reports } from '../lib/api';
import { useAsync } from '../lib/hooks';
import { cop, secs } from '../lib/format';
import { Loading, ErrorState, EmptyState } from '../components/ui';
import type { ClosingReport } from '../lib/types';

export function Closing() {
  const { data, loading, error, reload } = useAsync<ClosingReport>(() => Reports.closing(), []);

  if (loading) return <Loading />;
  if (error) return <ErrorState message={error} onRetry={reload} />;
  if (!data) return <EmptyState title="Sin datos de cierre" />;

  const top = data.top_products?.[0];
  const bestSeller = [...(data.operators ?? [])].sort((a, b) => b.total - a.total)[0];
  const fastest = data.timing?.fastest_operator;
  const margin = Math.round((data.totals?.margin ?? 0) * 100);

  const kpis = [
    { label: 'Ventas', value: cop(data.totals.revenue), sub: `${data.totals.order_count} pedidos` },
    { label: 'Utilidad', value: cop(data.totals.profit), sub: 'ingresos − costo', good: true },
    { label: 'Margen', value: `${margin}%`, sub: 'sobre ventas', good: true },
    { label: 'Costo', value: cop(data.totals.cost), sub: 'valor al por mayor' },
    { label: 'Producto más vendido', value: top ? top.name : '—', sub: top ? `${top.qty_sold} uds · ${cop(top.revenue)}` : 'sin ventas' },
    { label: 'Vendedor que más vendió', value: bestSeller ? bestSeller.username : '—', sub: bestSeller ? cop(bestSeller.total) : 'sin ventas' },
    { label: 'Vendedor más rápido', value: fastest ? fastest.username : '—', sub: fastest ? `${secs(fastest.avg_seconds)} · ${fastest.order_count} pedidos` : '—' },
    { label: 'Tiempo promedio', value: secs(data.timing.avg_seconds_per_order), sub: 'por pedido' },
    { label: 'Clientes registrados', value: String(data.attendees.registered), sub: `${data.attendees.active} con consumo` },
    { label: 'Consumo promedio', value: cop(data.attendees.avg_consumption), sub: 'por cliente activo' },
    { label: 'Excedente (efectivo)', value: cop(data.totals.overage_cash), sub: 'cobrado por sobre saldo' },
    { label: 'Dinero pendiente', value: cop(data.attendees.pending_balance), sub: 'saldo sin consumir' },
  ];

  return (
    <div>
      <div class="page-head spread">
        <div>
          <h1>Cierre</h1>
          <p>Reporte de negocio del evento {data.event?.name ? `· ${data.event.name}` : ''}.</p>
        </div>
        <div class="row no-print">
          <a class="btn primary" href={Reports.closingXlsxUrl}>⬇ Descargar Excel</a>
          <a class="btn" href={Reports.closingCsvUrl}>⬇ Descargar CSV</a>
        </div>
      </div>

      <div class="kpi-grid" style="margin-bottom:18px">
        {kpis.map((k) => (
          <div class="kpi" key={k.label}>
            <div class="label">{k.label}</div>
            <div class="value" style={k.good ? 'color:var(--green)' : ''}>{k.value}</div>
            <div class="sub">{k.sub}</div>
          </div>
        ))}
      </div>

      <div style="display:grid;grid-template-columns:1fr 1fr;gap:18px;align-items:start" class="dash-grid">
        <div class="card">
          <h3 style="margin:0 0 14px">Ventas por vendedor</h3>
          {(data.operators ?? []).length === 0 ? (
            <EmptyState icon="◦" title="Sin ventas aún" />
          ) : (
            <div class="table-wrap">
              <table class="tbl">
                <thead><tr><th>Vendedor</th><th class="num">Pedidos</th><th class="num">Total</th><th class="num">Tiempo prom.</th></tr></thead>
                <tbody>
                  {[...data.operators].sort((a, b) => b.total - a.total).map((o) => (
                    <tr key={o.username}>
                      <td style="font-weight:600">{o.username}</td>
                      <td class="num mono">{o.count}</td>
                      <td class="num mono">{cop(o.total)}</td>
                      <td class="num mono faint">{secs(o.avg_seconds)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>

        <div class="card">
          <h3 style="margin:0 0 14px">Productos más vendidos</h3>
          {(data.top_products ?? []).length === 0 ? (
            <EmptyState icon="◦" title="Sin ventas aún" />
          ) : (
            <div class="table-wrap">
              <table class="tbl">
                <thead><tr><th>Producto</th><th class="num">Uds.</th><th class="num">Ingreso</th><th class="num">Ganancia</th></tr></thead>
                <tbody>
                  {data.top_products.slice(0, 8).map((p) => (
                    <tr key={p.name}>
                      <td style="font-weight:600">{p.name}</td>
                      <td class="num mono">{p.qty_sold}</td>
                      <td class="num mono">{cop(p.revenue)}</td>
                      <td class="num mono" style="color:var(--green)">{cop(p.profit)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>

        <div class="card">
          <h3 style="margin:0 0 14px">Inventario agotado</h3>
          {(data.inventory?.out_of_stock ?? []).length === 0 ? (
            <EmptyState icon="✓" title="Nada agotado" />
          ) : (
            <div class="col" style="gap:8px">
              {data.inventory.out_of_stock.map((p) => (
                <div class="spread" key={p.name} style="padding:8px 0;border-bottom:1px solid var(--line)">
                  <span>{p.name}</span>
                  <span class="pill red">Agotado</span>
                </div>
              ))}
            </div>
          )}
        </div>

        <div class="card">
          <h3 style="margin:0 0 14px">Inventario restante (bajo)</h3>
          {(data.inventory?.low_stock ?? []).length === 0 ? (
            <EmptyState icon="✓" title="Inventario en orden" />
          ) : (
            <div class="col" style="gap:8px">
              {data.inventory.low_stock.map((p) => (
                <div class="spread" key={p.name} style="padding:8px 0;border-bottom:1px solid var(--line)">
                  <span>{p.name}</span>
                  <span class="pill amber">{p.stock_qty} restantes</span>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>

      <style>{`@media (max-width:900px){.dash-grid{grid-template-columns:1fr !important}}`}</style>
    </div>
  );
}

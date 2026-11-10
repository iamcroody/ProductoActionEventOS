import { Reports as ReportsApi } from '../lib/api';
import { useAsync } from '../lib/hooks';
import { cop, dateTime } from '../lib/format';
import { Loading, ErrorState, EmptyState } from '../components/ui';
import type { Order } from '../lib/types';

const EXPORTS = [
  { key: 'sales', label: 'Ventas' },
  { key: 'products', label: 'Productos' },
  { key: 'attendees', label: 'Asistentes' },
  { key: 'ledger', label: 'Movimientos' },
];

export function Reports() {
  const { data, loading, error, reload } = useAsync<Order[]>(() => ReportsApi.sales(150), []);

  return (
    <div>
      <div class="page-head spread">
        <div><h1>Reportes</h1><p>Historial de ventas y exportaciones.</p></div>
        <button class="btn ghost sm no-print" onClick={() => window.print()}>⎙ Imprimir / PDF</button>
      </div>

      <div class="card no-print" style="margin-bottom:18px">
        <strong>Exportar CSV</strong>
        <div class="row wrap" style="margin-top:12px">
          {EXPORTS.map((e) => (
            <a key={e.key} class="btn sm" href={ReportsApi.exportCsvUrl(e.key)} download>↓ {e.label}</a>
          ))}
        </div>
      </div>

      <h3 style="margin:0 0 12px">Ventas recientes</h3>
      {loading && <Loading />}
      {error && <ErrorState message={error} onRetry={reload} />}
      {!loading && !error && data && data.length === 0 && <EmptyState icon="◦" title="Sin ventas registradas" />}
      {!loading && !error && data && data.length > 0 && (
        <div class="table-wrap">
          <table class="tbl">
            <thead>
              <tr>
                <th>Fecha</th><th>Operador</th><th class="num">Ítems</th>
                <th class="num">Saldo</th><th class="num">Efectivo</th><th class="num">Total</th>
              </tr>
            </thead>
            <tbody>
              {data.map((o) => (
                <tr key={o.id}>
                  <td class="faint">{dateTime(o.created_at)}</td>
                  <td>{o.operator_name || '—'}</td>
                  <td class="num mono">{o.item_count ?? 0}</td>
                  <td class="num mono">{cop(o.paid_from_balance ?? 0)}</td>
                  <td class="num mono">{cop(o.paid_cash ?? 0)}</td>
                  <td class="num mono" style="font-weight:700">{cop(o.total)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

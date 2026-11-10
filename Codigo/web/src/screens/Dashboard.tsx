import { useLocation } from 'preact-iso';
import { Reports, Events, Pendientes } from '../lib/api';
import { useAsync } from '../lib/hooks';
import { cop, secs } from '../lib/format';
import { Loading, ErrorState, EmptyState } from '../components/ui';
import type { ClosingReport, DashboardReport, EventItem, PendienteOrder } from '../lib/types';

function FirstStepsWizard() {
  const loc = useLocation();
  const go = (to: string) => (e: Event) => { e.preventDefault(); loc.route(to); };
  const step = (to: string, label: string) => (
    <a href={to} class="wizard-link" onClick={go(to)}>{label}</a>
  );
  return (
    <div class="card wizard" style="margin-bottom:24px">
      <strong style="color:var(--amber)">Primeros pasos</strong>
      <p class="muted" style="margin:6px 0 14px;font-size:13px">
        No hay un evento activo. Sigue estos pasos para comenzar a operar:
      </p>
      <ol style="margin:0;padding-left:20px;display:flex;flex-direction:column;gap:10px;font-size:14px">
        <li>{step('/admin/eventos', 'Crear evento')} — define nombre, recarga por defecto y política de división.</li>
        <li>{step('/admin/menu', 'Configurar menú')} — crea categorías y productos con precios.</li>
        <li>{step('/admin/asistentes', 'Registrar asistentes')} — importa o crea asistentes con cédula, celular y recarga inicial.</li>
        <li>{step('/admin/eventos', 'Iniciar evento')} — activa el evento para bloquear la configuración e iniciar ventas.</li>
      </ol>
    </div>
  );
}

export function Dashboard() {
  const eventState = useAsync<EventItem | null>(
    () => Events.active().catch(() => null),
    [],
  );
  const hasActiveEvent = !!(eventState.data?.status === 'active' || eventState.data?.active === 1);

  // Only query the dashboard once we know an event is active. Without one, the
  // endpoint legitimately returns a 4xx ("sin evento activo") — fetching anyway
  // would log a red error in devtools and surface an error screen instead of
  // the first-steps wizard. No active event → guide the admin, don't alarm them.
  const { data, loading, error, reload } = useAsync<DashboardReport | null>(
    () => (hasActiveEvent ? Reports.dashboard() : Promise.resolve(null)),
    [hasActiveEvent],
  );

  // Hero KPIs pull from two extra sources: live pending orders and the closing
  // report (for attendee count + avg time). Both degrade gracefully to null so a
  // missing endpoint never breaks the dashboard.
  const pend = useAsync<PendienteOrder[] | null>(
    () => (hasActiveEvent ? Pendientes.list().catch(() => null) : Promise.resolve(null)),
    [hasActiveEvent],
  );
  const closing = useAsync<ClosingReport | null>(
    () => (hasActiveEvent ? Reports.closing().catch(() => null) : Promise.resolve(null)),
    [hasActiveEvent],
  );

  if (eventState.loading) return <Loading />;

  if (!hasActiveEvent) {
    return (
      <div>
        <div class="page-head spread">
          <div><h1>Dashboard</h1><p>Resumen operativo del evento en curso.</p></div>
        </div>
        <FirstStepsWizard />
      </div>
    );
  }

  if (loading) return <Loading />;
  if (error) return <ErrorState message={error} onRetry={reload} />;
  if (!data) return <EmptyState title="Sin datos" />;

  const activeOrders = (pend.data ?? []).length;
  const registered = closing.data?.attendees.registered;
  const avgTime = closing.data?.timing.avg_seconds_per_order;
  const criticalStock = (data.low_stock ?? []).length;

  const hero = [
    { label: 'Evento', value: eventState.data?.name || '—', accent: true },
    { label: 'Asistentes', value: registered != null ? String(registered) : String(data.active_attendees ?? 0) },
    { label: 'Ventas', value: cop(data.total_sold), good: true },
    { label: 'Pedidos activos', value: String(activeOrders) },
    { label: 'Tiempo promedio', value: avgTime != null ? secs(avgTime) : '—' },
    { label: 'Inventario crítico', value: String(criticalStock), warn: criticalStock > 0 },
  ];

  const kpis = [
    { label: 'Ingresos', value: cop(data.total_sold), sub: `${data.order_count} órdenes` },
    { label: 'Costo', value: cop(data.total_cost), sub: 'valor al por mayor' },
    { label: 'Ganancia', value: cop(data.total_profit), sub: 'ingresos − costo', good: true },
    { label: 'Margen', value: `${Math.round((data.profit_margin ?? 0) * 100)}%`, sub: 'sobre ingresos', good: true },
    { label: 'Consumo promedio', value: cop(data.avg_consumption), sub: 'por orden' },
    { label: 'Asistentes activos', value: String(data.active_attendees ?? 0), sub: 'con consumo' },
    { label: 'Efectivo (sobrepago)', value: cop(data.overage_cash), sub: 'excedente cobrado' },
    { label: 'Pagado con saldo', value: cop(data.balance_from_sales), sub: 'desde recargas' },
  ];

  return (
    <div>
      <div class="page-head spread">
        <div><h1>Dashboard</h1><p>Resumen operativo del evento en curso.</p></div>
        <div class="row no-print">
          <a class="btn sm" href={Reports.exportCsvUrl('sales')}>↓ Ventas CSV</a>
          <button class="btn ghost sm" onClick={() => window.print()}>⎙ Imprimir</button>
        </div>
      </div>

      <div class="hero-band">
        {hero.map((h) => (
          <div class={`hero-kpi${h.accent ? ' accent' : ''}`} key={h.label}>
            <div class="h-label">{h.label}</div>
            <div
              class={`h-value${h.accent ? ' name' : ' mono'}`}
              style={h.good ? 'color:var(--green)' : h.warn ? 'color:var(--amber)' : ''}
              title={String(h.value)}
            >
              {h.value}
            </div>
          </div>
        ))}
      </div>

      <div class="kpi-grid" style="margin-bottom:18px">
        {kpis.map((k) => (
          <div class="kpi" key={k.label}>
            <div class="label">{k.label}</div>
            <div class="value mono" style={k.good ? 'color:var(--green)' : ''}>{k.value}</div>
            <div class="sub">{k.sub}</div>
          </div>
        ))}
      </div>

      <div style="display:grid;grid-template-columns:1fr 1fr;gap:18px;align-items:start" class="dash-grid">
        <div class="card">
          <h3 style="margin:0 0 4px">Ventas por hora</h3>
          <p class="muted" style="margin:0 0 8px;font-size:13px">Total facturado por franja horaria.</p>
          <BarChart data={(data.sales_by_hour ?? []).map((s) => ({ label: formatHour(s.hour), value: s.total }))} />
        </div>

        <div class="card">
          <h3 style="margin:0 0 14px">Productos más vendidos</h3>
          {(data.top_products ?? []).length === 0 ? (
            <EmptyState icon="◦" title="Sin ventas aún" />
          ) : (
            <div class="table-wrap">
              <table class="tbl">
                <thead><tr><th>Producto</th><th class="num">Uds.</th><th class="num">Ingreso</th><th class="num">Costo</th><th class="num">Ganancia</th></tr></thead>
                <tbody>
                  {data.top_products.slice(0, 8).map((p) => (
                    <tr key={p.name}>
                      <td style="font-weight:600">{p.name}</td>
                      <td class="num mono">{p.qty_sold}</td>
                      <td class="num mono">{cop(p.revenue ?? 0)}</td>
                      <td class="num mono faint">{cop(p.cost ?? 0)}</td>
                      <td class="num mono" style="color:var(--green)">{cop(p.profit ?? 0)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>

        <div class="card">
          <h3 style="margin:0 0 14px">Ventas por operador</h3>
          <HBars data={(data.sales_by_operator ?? []).map((o) => ({ label: o.username, value: o.total }))} money />
        </div>

        <div class="card">
          <h3 style="margin:0 0 14px">Bajo inventario</h3>
          {(data.low_stock ?? []).length === 0 ? (
            <EmptyState icon="✓" title="Inventario en orden" />
          ) : (
            <div class="col" style="gap:8px">
              {data.low_stock.map((p) => (
                <div class="spread" key={p.id} style="padding:8px 0;border-bottom:1px solid var(--line)">
                  <span>{p.name}</span>
                  <span class={`pill ${p.stock_qty <= 0 ? 'red' : 'amber'}`}>{p.stock_qty} restantes</span>
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

function formatHour(h: number | string): string {
  const n = typeof h === 'string' ? parseInt(h, 10) : h;
  if (Number.isNaN(n)) return String(h);
  return `${String(n % 24).padStart(2, '0')}h`;
}

function BarChart({ data }: { data: { label: string; value: number }[] }) {
  if (!data.length) return <EmptyState icon="◦" title="Sin ventas aún" />;
  const max = Math.max(...data.map((d) => d.value), 1);
  return (
    <div class="chart">
      {data.map((d) => (
        <div class="bar-col" key={d.label} title={cop(d.value)}>
          <div class="bar" style={`height:${Math.max(2, (d.value / max) * 150)}px`} />
          <div class="bar-lbl">{d.label}</div>
        </div>
      ))}
    </div>
  );
}

function HBars({ data, money, unit }: { data: { label: string; value: number }[]; money?: boolean; unit?: string }) {
  if (!data.length) return <EmptyState icon="◦" title="Sin datos" />;
  const max = Math.max(...data.map((d) => d.value), 1);
  return (
    <div>
      {data.map((d) => (
        <div class="hbar-row" key={d.label}>
          <div style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title={d.label}>{d.label}</div>
          <div class="hbar-track"><div class="hbar-fill" style={`width:${(d.value / max) * 100}%`} /></div>
          <div class="mono faint" style="font-size:13px">{money ? cop(d.value) : `${d.value}${unit ? ' ' + unit : ''}`}</div>
        </div>
      ))}
    </div>
  );
}

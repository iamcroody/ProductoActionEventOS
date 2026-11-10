import { useState } from 'preact/hooks';
import { ConfigApi, Reports, ApiError } from '../../lib/api';
import { useAsync } from '../../lib/hooks';
import { cop, copInput } from '../../lib/format';
import { toast } from '../../lib/toast';
import { Field, Spinner, Loading, ErrorState, EmptyState } from '../../components/ui';
import type { Config, TipsReport } from '../../lib/types';

function parseAseo(cfg: Config | null): string[] {
  const raw = cfg?.aseo_staff;
  if (typeof raw !== 'string' || !raw.trim()) return [];
  try {
    const arr = JSON.parse(raw);
    return Array.isArray(arr) ? arr.map((x) => String(x)).filter(Boolean) : [];
  } catch { return []; }
}

export function TipsTab() {
  const cfg = useAsync<Config>(() => ConfigApi.get(), []);

  if (cfg.loading) return <Loading />;
  if (cfg.error) return <ErrorState message={cfg.error} onRetry={cfg.reload} />;

  return (
    <div class="dash-grid" style="display:grid;grid-template-columns:340px 1fr;gap:18px;align-items:start">
      <AseoStaff initial={parseAseo(cfg.data)} onSaved={cfg.reload} />
      <TipsCalculator />
      <style>{`@media (max-width:900px){.dash-grid{grid-template-columns:1fr !important}}`}</style>
    </div>
  );
}

function AseoStaff({ initial, onSaved }: { initial: string[]; onSaved: () => void }) {
  const [names, setNames] = useState<string[]>(initial);
  const [draft, setDraft] = useState('');
  const [busy, setBusy] = useState(false);

  const add = () => {
    const n = draft.trim();
    if (!n) return;
    if (names.some((x) => x.toLowerCase() === n.toLowerCase())) { toast.err('Ya está en la lista'); return; }
    setNames([...names, n]);
    setDraft('');
  };
  const remove = (i: number) => setNames(names.filter((_, idx) => idx !== i));

  const save = async () => {
    setBusy(true);
    try {
      await ConfigApi.update({ aseo_staff: JSON.stringify(names) });
      toast.ok('Personal de aseo guardado');
      onSaved();
    } catch (e) { toast.err(e instanceof ApiError ? e.message : 'Error'); }
    finally { setBusy(false); }
  };

  return (
    <div class="card">
      <strong>Personal de aseo</strong>
      <p class="muted" style="margin:6px 0 0;font-size:13px">Reciben un 5% adicional en el reparto de propinas.</p>
      <div class="divider" />
      <div class="row" style="gap:8px">
        <input class="input" placeholder="Nombre" value={draft}
          onInput={(e) => setDraft((e.target as HTMLInputElement).value)}
          onKeyDown={(e) => { if (e.key === 'Enter') add(); }} />
        <button class="btn sm" onClick={add}>+ Agregar</button>
      </div>
      <div class="col" style="gap:8px;margin-top:14px">
        {names.length === 0 && <span class="faint">Sin personal de aseo</span>}
        {names.map((n, i) => (
          <div class="spread" key={n + i} style="padding:8px 0;border-bottom:1px solid var(--line)">
            <span>{n}</span>
            <button class="btn ghost sm danger" onClick={() => remove(i)}>Quitar</button>
          </div>
        ))}
      </div>
      <button class="btn primary" style="margin-top:16px;align-self:flex-start" disabled={busy} onClick={save}>
        {busy ? <Spinner /> : 'Guardar lista'}
      </button>
    </div>
  );
}

function TipsCalculator() {
  const [pool, setPool] = useState(0);
  const [busy, setBusy] = useState(false);
  const [report, setReport] = useState<TipsReport | null>(null);
  const [error, setError] = useState<string | null>(null);

  const calc = async () => {
    if (pool <= 0) { toast.err('Ingresa el monto de propinas'); return; }
    setBusy(true); setError(null);
    try {
      const r = await Reports.tips(pool);
      setReport(r);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : 'Error');
      setReport(null);
    } finally { setBusy(false); }
  };

  return (
    <div>
      <div class="card">
        <strong>Calcular reparto de propinas</strong>
        <p class="muted" style="margin:6px 0 0;font-size:13px">
          Los 3 meseros con más ventas y todo el personal de aseo reciben un porcentaje extra; el resto se reparte por igual.
        </p>
        <div class="divider" />
        <div class="row" style="gap:12px;align-items:flex-end">
          <Field label="Bolsa de propinas">
            <input class="input mono" inputMode="numeric" value={pool ? cop(pool) : ''} placeholder="$0"
              onInput={(e) => setPool(copInput((e.target as HTMLInputElement).value))}
              onKeyDown={(e) => { if (e.key === 'Enter') calc(); }} />
          </Field>
          <button class="btn primary" disabled={busy} onClick={calc} style="height:46px">
            {busy ? <Spinner /> : 'Calcular'}
          </button>
        </div>
      </div>

      {error && <div style="margin-top:18px"><ErrorState message={error} onRetry={calc} /></div>}

      {report && (
        <div style="margin-top:18px">
          <div class="kpi-grid" style="margin-bottom:18px">
            <div class="kpi">
              <div class="label">Bolsa</div>
              <div class="value mono">{cop(report.pool)}</div>
              <div class="sub">total a repartir</div>
            </div>
            <div class="kpi">
              <div class="label">Distribuido</div>
              <div class="value mono" style="color:var(--green)">{cop(report.distributed)}</div>
              <div class="sub">{report.distributed === report.pool ? 'cuadra exacto' : 'verificar'}</div>
            </div>
            <div class="kpi">
              <div class="label">Bono extra</div>
              <div class="value mono">{Math.round((report.bonus_pct ?? 0) * 100)}%</div>
              <div class="sub">top-3 y aseo</div>
            </div>
          </div>

          <div class="card" style="margin-bottom:18px">
            <h3 style="margin:0 0 14px">Meseros</h3>
            {report.meseros.length === 0 ? (
              <EmptyState icon="◦" title="Sin meseros con ventas" />
            ) : (
              <div class="table-wrap">
                <table class="tbl">
                  <thead><tr><th>Nombre</th><th class="num">Ventas</th><th></th><th class="num">Propina</th></tr></thead>
                  <tbody>
                    {report.meseros.map((m) => (
                      <tr key={m.name} style={m.top3 ? 'background:var(--amber-soft)' : ''}>
                        <td style="font-weight:600">{m.name}</td>
                        <td class="num mono">{cop(m.sales)}</td>
                        <td>{m.top3 ? <span class="pill amber">Top 3</span> : (m.bonus ? <span class="pill blue">Bono</span> : '')}</td>
                        <td class="num mono" style="font-weight:700;color:var(--green)">{cop(m.share)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>

          <div class="card">
            <h3 style="margin:0 0 14px">Personal de aseo</h3>
            {report.aseo.length === 0 ? (
              <EmptyState icon="◦" title="Sin personal de aseo" hint="Agrégalo en la lista de la izquierda." />
            ) : (
              <div class="table-wrap">
                <table class="tbl">
                  <thead><tr><th>Nombre</th><th></th><th class="num">Propina</th></tr></thead>
                  <tbody>
                    {report.aseo.map((a) => (
                      <tr key={a.name}>
                        <td style="font-weight:600">{a.name}</td>
                        <td><span class="pill green">+{Math.round((report.bonus_pct ?? 0) * 100)}%</span></td>
                        <td class="num mono" style="font-weight:700;color:var(--green)">{cop(a.share)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

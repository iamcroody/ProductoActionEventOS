import { useState } from 'preact/hooks';
import { ConfigApi, Health, ApiError } from '../../lib/api';
import { useAsync } from '../../lib/hooks';
import { cop, copInput } from '../../lib/format';
import { toast } from '../../lib/toast';
import { Field, Spinner, Loading, ErrorState } from '../../components/ui';
import type { Config } from '../../lib/types';

export function SettingsTab() {
  const cfg = useAsync<Config>(() => ConfigApi.get(), []);
  const health = useAsync(() => Health.get(), []);

  if (cfg.loading) return <Loading />;
  if (cfg.error) return <ErrorState message={cfg.error} onRetry={cfg.reload} />;

  return (
    <div style="max-width:560px">
      <SettingsForm initial={cfg.data ?? {}} onSaved={cfg.reload} />
      <div class="card" style="margin-top:18px">
        <strong>Sistema</strong>
        <div class="divider" />
        <div class="spread"><span class="muted">Producto</span><span>{health.data?.product ?? 'Action EventOS'}</span></div>
        <div class="spread" style="margin-top:8px"><span class="muted">Estado</span><span class="pill green">{health.data?.ok ? 'En línea' : '—'}</span></div>
        <div class="spread" style="margin-top:8px"><span class="muted">Firma</span><span class="faint mono" style="font-size:12px">{health.data?.signature ?? 'CROODY'}</span></div>
      </div>
    </div>
  );
}

function SettingsForm({ initial, onSaved }: { initial: Config; onSaved: () => void }) {
  const [org, setOrg] = useState(String(initial.org_name ?? ''));
  const [currency, setCurrency] = useState(String(initial.currency ?? 'COP'));
  const [topup, setTopup] = useState(Number(initial.default_topup ?? 0));
  const [liteMode, setLiteMode] = useState(initial.lite_mode === '1' || initial.lite_mode === true);
  const [busy, setBusy] = useState(false);

  const save = async () => {
    setBusy(true);
    try {
      await ConfigApi.update({ org_name: org, currency, default_topup: topup, lite_mode: liteMode ? '1' : '0' });
      toast.ok('Configuración guardada');
      onSaved();
    } catch (e) { toast.err(e instanceof ApiError ? e.message : 'Error'); }
    finally { setBusy(false); }
  };

  return (
    <div class="card">
      <strong>Configuración general</strong>
      <div class="divider" />
      <div class="col">
        <Field label="Organización"><input class="input" value={org} onInput={(e) => setOrg((e.target as HTMLInputElement).value)} /></Field>
        <div class="field-row">
          <Field label="Moneda"><input class="input" value={currency} onInput={(e) => setCurrency((e.target as HTMLInputElement).value)} /></Field>
          <Field label="Recarga por defecto (mínimo requerido)">
            <input class="input mono" inputMode="numeric" value={topup ? cop(topup) : ''} placeholder="$0"
              onInput={(e) => setTopup(copInput((e.target as HTMLInputElement).value))} />
          </Field>
        </div>
        <label class="toggle">
          <input type="checkbox" checked={liteMode} onChange={(e) => setLiteMode((e.target as HTMLInputElement).checked)} />
          Modo lite — ocultar Inventario y Dashboard en eventos que no los necesiten
        </label>
        <button class="btn primary" style="align-self:flex-start" disabled={busy} onClick={save}>{busy ? <Spinner /> : 'Guardar cambios'}</button>
      </div>
    </div>
  );
}

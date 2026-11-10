import { useState } from 'preact/hooks';
import { Events, ApiError } from '../../lib/api';
import { useAsync } from '../../lib/hooks';
import { cop, copInput, dateTime } from '../../lib/format';
import { toast } from '../../lib/toast';
import { Modal, Field, Spinner, Loading, EmptyState, ErrorState } from '../../components/ui';
import type { EventItem } from '../../lib/types';

export function EventsTab() {
  const { data, loading, error, reload } = useAsync<EventItem[]>(() => Events.list(), []);
  const [showNew, setShowNew] = useState(false);

  const act = async (fn: () => Promise<unknown>, msg: string) => {
    try { await fn(); toast.ok(msg); reload(); }
    catch (e) { toast.err(e instanceof ApiError ? e.message : 'Error'); }
  };

  return (
    <div>
      <div class="spread" style="margin-bottom:16px">
        <strong>Eventos</strong>
        <button class="btn primary sm" onClick={() => setShowNew(true)}>+ Nuevo evento</button>
      </div>

      {loading && <Loading />}
      {error && <ErrorState message={error} onRetry={reload} />}
      {!loading && !error && (data ?? []).length === 0 && <EmptyState icon="◦" title="Sin eventos" hint="Crea el primer evento." />}
      {!loading && !error && (data ?? []).length > 0 && (
        <div class="table-wrap">
          <table class="tbl">
            <thead><tr><th>Nombre</th><th>Estado</th><th>Inicia</th><th class="num">Recarga def.</th><th></th></tr></thead>
            <tbody>
              {data!.map((ev) => {
                const active = ev.status === 'active' || ev.active === 1;
                return (
                  <tr key={ev.id}>
                    <td style="font-weight:600">{ev.name}</td>
                    <td><span class={`pill ${active ? 'green' : ev.status === 'closed' ? 'red' : 'blue'}`}>{ev.status || (active ? 'active' : 'draft')}</span></td>
                    <td class="faint">{ev.starts_at ? dateTime(ev.starts_at) : '—'}</td>
                    <td class="num mono">{cop(ev.default_topup ?? 0)}</td>
                    <td class="num">
                      <div class="row" style="justify-content:flex-end;gap:6px">
                        {!active && ev.status !== 'closed' && (
                          <button class="btn sm" onClick={() => act(() => Events.activate(ev.id), 'Evento activado')}>Activar</button>
                        )}
                        {ev.status !== 'closed' && (
                          <button class="btn danger sm" onClick={() => act(() => Events.close(ev.id), 'Evento cerrado')}>Cerrar</button>
                        )}
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {showNew && <NewEvent onClose={() => setShowNew(false)} onSaved={() => { setShowNew(false); reload(); }} />}
    </div>
  );
}

function NewEvent({ onClose, onSaved }: { onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState('');
  const [topup, setTopup] = useState(0);
  const [policy, setPolicy] = useState('equal');
  const [busy, setBusy] = useState(false);

  const save = async () => {
    if (!name.trim()) { toast.err('Ingresa el nombre'); return; }
    setBusy(true);
    try {
      await Events.create({ name: name.trim(), default_topup: topup, split_policy: policy });
      toast.ok('Evento creado');
      onSaved();
    } catch (e) { toast.err(e instanceof ApiError ? e.message : 'No se pudo crear'); }
    finally { setBusy(false); }
  };

  return (
    <Modal title="Nuevo evento" onClose={onClose}
      footer={<><button class="btn ghost" onClick={onClose}>Cancelar</button>
        <button class="btn primary" disabled={busy} onClick={save}>{busy ? <Spinner /> : 'Crear'}</button></>}>
      <Field label="Nombre del evento"><input class="input" autoFocus value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} /></Field>
      <div class="field-row">
        <Field label="Recarga por defecto">
          <input class="input mono" inputMode="numeric" value={topup ? cop(topup) : ''} placeholder="$0"
            onInput={(e) => setTopup(copInput((e.target as HTMLInputElement).value))} />
        </Field>
        <Field label="Política de división">
          <select class="select" value={policy} onChange={(e) => setPolicy((e.target as HTMLSelectElement).value)}>
            <option value="equal">Igual</option>
            <option value="proportional">Proporcional</option>
            <option value="none">Sin división</option>
          </select>
        </Field>
      </div>
    </Modal>
  );
}

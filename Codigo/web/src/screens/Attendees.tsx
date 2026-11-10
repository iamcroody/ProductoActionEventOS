import { useEffect, useState } from 'preact/hooks';
import { Attendees as AttApi, Client, ApiError } from '../lib/api';
import { useAsync, useDebounced } from '../lib/hooks';
import { consumeNewAttendee } from '../lib/hotkeys';
import { cop, copInput, dateTime } from '../lib/format';
import { printRegion } from '../lib/print';
import { toast } from '../lib/toast';
import { Modal, Field, Spinner, Loading, EmptyState, ErrorState } from '../components/ui';
import { qrSvg } from '../lib/qr';
import type { Attendee, LedgerEntry } from '../lib/types';

export function Attendees({ embedded, locked = false }: { embedded?: boolean; locked?: boolean } = {}) {
  const [q, setQ] = useState('');
  const dq = useDebounced(q.trim(), 220);
  const [showCreate, setShowCreate] = useState(false);
  const [showImport, setShowImport] = useState(false);
  const [selected, setSelected] = useState<Attendee | null>(null);
  const [qrFor, setQrFor] = useState<Attendee | null>(null);

  const { data, loading, error, reload } = useAsync<Attendee[]>(
    () => AttApi.search(dq || undefined), [dq],
  );

  // F1 (global) routes here and requests the create form; open it on arrival and
  // also live while already on this screen.
  useEffect(() => {
    if (locked) return;
    if (consumeNewAttendee()) setShowCreate(true);
    const onNew = () => setShowCreate(true);
    window.addEventListener('aeos:new-attendee', onNew);
    return () => window.removeEventListener('aeos:new-attendee', onNew);
  }, [locked]);

  return (
    <div>
      {!embedded && <div class="page-head"><h1>Asistentes</h1><p>Clientes del evento, recargas y saldos.</p></div>}

      <div class="row wrap" style="margin-bottom:16px;gap:10px">
        <div class="search-xl grow" style="min-width:240px">
          <span class="ico">⌕</span>
          <input class="input" data-hotkey-search placeholder="Buscar por nombre o documento…" value={q}
            onInput={(e) => setQ((e.target as HTMLInputElement).value)} />
        </div>
        <button class="btn" disabled={locked} onClick={() => setShowImport(true)}>⇪ Importar</button>
        <button class="btn primary" disabled={locked} onClick={() => setShowCreate(true)}>+ Nuevo asistente</button>
      </div>

      {loading && <Loading />}
      {error && <ErrorState message={error} onRetry={reload} />}
      {!loading && !error && data && data.length === 0 && (
        <div class="state">
          <div class="big">☷</div>
          <div style="font-weight:600;color:var(--text)">Sin asistentes</div>
          <div style="margin-top:6px;font-size:13.5px">
            {dq ? `Nada para "${dq}"` : 'Crea o importa asistentes para empezar.'}
          </div>
          {!locked && (
            <div class="row" style="justify-content:center;margin-top:16px;gap:10px">
              <button class="btn primary" onClick={() => setShowCreate(true)}>+ Nuevo asistente</button>
              <button class="btn" onClick={() => setShowImport(true)}>⇪ Importar</button>
            </div>
          )}
        </div>
      )}
      {!loading && !error && data && data.length > 0 && (
        <div class="table-wrap">
          <table class="tbl">
            <thead>
              <tr><th>Nombre</th><th>Documento</th><th>Teléfono</th><th class="num">Saldo</th><th></th></tr>
            </thead>
            <tbody>
              {data.map((a) => (
                <tr key={a.id} style="cursor:pointer" onClick={() => setSelected(a)}>
                  <td style="font-weight:600">{a.display_name} {a.blocked ? <span class="pill red">Bloqueado</span> : null}</td>
                  <td class="faint">{a.doc_id || '—'}</td>
                  <td class="faint">{a.phone || '—'}</td>
                  <td class="num mono" style={a.balance > 0 ? 'color:var(--green);font-weight:600' : ''}>{cop(a.balance)}</td>
                  <td class="num">
                    <div class="row" style="justify-content:flex-end;gap:6px">
                      <button class="btn ghost sm" title="QR personal del asistente" onClick={(e) => { e.stopPropagation(); setQrFor(a); }}>⛶ QR</button>
                      <button class="btn ghost sm" onClick={(e) => { e.stopPropagation(); setSelected(a); }}>Ver</button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {showCreate && <CreateAttendee onClose={() => setShowCreate(false)} onCreated={() => { setShowCreate(false); reload(); }} />}
      {showImport && <ImportAttendees onClose={() => setShowImport(false)} onDone={() => { setShowImport(false); reload(); }} />}
      {selected && <AttendeeDetail attendee={selected} onClose={() => setSelected(null)} onChanged={reload} />}
      {qrFor && <AttendeeQrModal attendee={qrFor} onClose={() => setQrFor(null)} />}
    </div>
  );
}

// Personal QR (server PNG) that opens the customer web prefilled with this
// attendee's token. Printable on its own (only the QR + name reach the printer).
function AttendeeQrModal({ attendee, onClose }: { attendee: Attendee; onClose: () => void }) {
  return (
    <Modal
      title={`QR · ${attendee.display_name}`}
      onClose={onClose}
      footer={
        <>
          <button class="btn ghost no-print" onClick={onClose}>Cerrar</button>
          <button class="btn primary no-print" onClick={printRegion}>⎙ Imprimir</button>
        </>
      }
    >
      <div class="print-region" style="text-align:center">
        <div class="qr-box" style="margin:0 auto">
          <img
            src={Client.attendeeQrUrl(attendee.id)}
            alt={`QR de ${attendee.display_name}`}
            width={240}
            height={240}
            style="display:block;width:240px;height:240px"
          />
        </div>
        <div style="font-weight:700;margin-top:14px;font-size:16px">{attendee.display_name}</div>
        <div class="faint" style="font-size:12.5px;margin-top:4px">
          Escanéalo para pedir desde el celular con el saldo ya cargado.
        </div>
      </div>
    </Modal>
  );
}

function CreateAttendee({ onClose, onCreated }: { onClose: () => void; onCreated: (a: Attendee) => void }) {
  const [name, setName] = useState('');
  const [email, setEmail] = useState('');
  const [doc, setDoc] = useState('');
  const [phone, setPhone] = useState('');
  const [topup, setTopup] = useState(0);
  const [noRedeem, setNoRedeem] = useState(false);
  const [busy, setBusy] = useState(false);

  const save = async () => {
    if (!name.trim()) { toast.err('Ingresa el nombre'); return; }
    setBusy(true);
    try {
      const a = await AttApi.create({
        display_name: name.trim(),
        email: email.trim() || undefined,
        doc_id: doc.trim() || undefined,
        phone: phone.trim() || undefined,
        initial_topup: noRedeem ? undefined : (topup || undefined),
        no_redeem: noRedeem || undefined,
      });
      toast.ok('Asistente creado');
      onCreated(a);
    } catch (e) {
      toast.err(e instanceof ApiError ? e.message : 'No se pudo crear');
    } finally { setBusy(false); }
  };

  return (
    <Modal title="Nuevo asistente" onClose={onClose}
      footer={<><button class="btn ghost" onClick={onClose}>Cancelar</button>
        <button class="btn primary" disabled={busy} onClick={save}>{busy ? <Spinner /> : 'Crear'}</button></>}>
      <Field label="Nombre *">
        <input class="input" autoFocus value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} />
      </Field>
      <Field label="Correo electrónico">
        <input class="input" type="email" inputMode="email" value={email} placeholder="correo@ejemplo.com"
          onInput={(e) => setEmail((e.target as HTMLInputElement).value)} />
      </Field>
      <div class="field-row">
        <Field label="Cédula">
          <input class="input" value={doc} placeholder="Número de documento"
            onInput={(e) => setDoc((e.target as HTMLInputElement).value)} />
        </Field>
        <Field label="Celular">
          <input class="input" inputMode="tel" value={phone} placeholder="3001234567"
            onInput={(e) => setPhone((e.target as HTMLInputElement).value)} />
        </Field>
      </div>
      <label class="toggle" style="margin-bottom:12px">
        <input type="checkbox" checked={noRedeem} onChange={(e) => setNoRedeem((e.target as HTMLInputElement).checked)} />
        Sin dinero redimible (no asignar saldo inicial)
      </label>
      {!noRedeem && (
        <Field label="Recarga inicial">
          <input class="input mono" inputMode="numeric" value={topup ? cop(topup) : ''} placeholder="$0"
            onInput={(e) => setTopup(copInput((e.target as HTMLInputElement).value))} />
        </Field>
      )}
    </Modal>
  );
}

function ImportAttendees({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);

  const parse = () => {
    const rows = text.split('\n').map((l) => l.trim()).filter(Boolean);
    return rows.map((line) => {
      const [display_name, doc_id, phone, topup] = line.split(/[,\t;]/).map((s) => s?.trim());
      const initial_topup = topup ? copInput(topup) : undefined;
      return { display_name, doc_id: doc_id || undefined, phone: phone || undefined, initial_topup };
    }).filter((r) => r.display_name);
  };

  const run = async () => {
    const rows = parse();
    if (rows.length === 0) { toast.err('No hay filas válidas'); return; }
    setBusy(true);
    try {
      const r = await AttApi.import(rows);
      toast.ok(`${r.imported ?? rows.length} asistentes importados`);
      onDone();
    } catch (e) {
      toast.err(e instanceof ApiError ? e.message : 'No se pudo importar');
    } finally { setBusy(false); }
  };

  const count = parse().length;

  return (
    <Modal title="Importar asistentes" onClose={onClose} wide
      footer={<><button class="btn ghost" onClick={onClose}>Cancelar</button>
        <button class="btn primary" disabled={busy || count === 0} onClick={run}>{busy ? <Spinner /> : `Importar ${count || ''}`}</button></>}>
      <p class="muted" style="margin:0">Pega una fila por asistente: <code>nombre, documento, teléfono, recarga</code> (separadores coma, tab o punto y coma).</p>
      <textarea class="input" style="min-height:200px;font-family:ui-monospace,monospace;font-size:13px" placeholder={'Juan Pérez, 1020304050, 3001234567, 50000\nMaría López, 1122334455'}
        value={text} onInput={(e) => setText((e.target as HTMLTextAreaElement).value)} />
      <div class="faint">{count} fila{count === 1 ? '' : 's'} detectada{count === 1 ? '' : 's'}</div>
    </Modal>
  );
}

function AttendeeDetail({ attendee, onClose, onChanged }: { attendee: Attendee; onClose: () => void; onChanged: () => void }) {
  const [att, setAtt] = useState(attendee);
  const [topup, setTopup] = useState(0);
  const [busy, setBusy] = useState(false);
  const [tab, setTab] = useState<'saldo' | 'ledger' | 'qr'>('saldo');
  const ledger = useAsync<LedgerEntry[]>(() => AttApi.ledger(att.id), [att.id, tab === 'ledger']);

  const refresh = async () => { try { setAtt(await AttApi.get(att.id)); } catch { /* */ } onChanged(); };

  const doTopup = async () => {
    if (topup <= 0) { toast.err('Ingresa un monto'); return; }
    setBusy(true);
    try {
      const updated = await AttApi.topup(att.id, topup);
      setAtt((a) => ({ ...a, balance: updated.balance ?? a.balance + topup }));
      setTopup(0);
      toast.ok('Recarga aplicada');
      onChanged();
    } catch (e) {
      toast.err(e instanceof ApiError ? e.message : 'No se pudo recargar');
    } finally { setBusy(false); }
  };

  const toggleBlock = async () => {
    setBusy(true);
    try {
      if (att.blocked) await AttApi.unblock(att.id); else await AttApi.block(att.id);
      await refresh();
      setAtt((a) => ({ ...a, blocked: a.blocked ? 0 : 1 }));
      toast.ok(att.blocked ? 'Desbloqueado' : 'Bloqueado');
    } catch (e) {
      toast.err(e instanceof ApiError ? e.message : 'No se pudo actualizar');
    } finally { setBusy(false); }
  };

  return (
    <Modal title={att.display_name} onClose={onClose} wide>
      <div class="spread">
        <div>
          <div class="faint" style="font-size:12.5px">{att.doc_id ? `Doc ${att.doc_id}` : 'Sin documento'}{att.phone ? ` · ${att.phone}` : ''}</div>
        </div>
        <div style="text-align:right">
          <div class="faint" style="font-size:11px;text-transform:uppercase">Saldo</div>
          <div class="mono" style={`font-size:24px;font-weight:700;color:${att.balance > 0 ? 'var(--green)' : 'var(--text)'}`}>{cop(att.balance)}</div>
        </div>
      </div>

      <div class="tabs" style="margin:6px 0 0">
        <button class={`tab ${tab === 'saldo' ? 'active' : ''}`} onClick={() => setTab('saldo')}>Recargar</button>
        <button class={`tab ${tab === 'ledger' ? 'active' : ''}`} onClick={() => setTab('ledger')}>Movimientos</button>
        <button class={`tab ${tab === 'qr' ? 'active' : ''}`} onClick={() => setTab('qr')}>QR</button>
      </div>

      {tab === 'saldo' && (
        <div class="col">
          <Field label="Monto a recargar">
            <input class="input mono" inputMode="numeric" value={topup ? cop(topup) : ''} placeholder="$0"
              onInput={(e) => setTopup(copInput((e.target as HTMLInputElement).value))} />
          </Field>
          <div class="row wrap">
            {[10000, 20000, 50000, 100000].map((v) => (
              <button key={v} class="btn sm" onClick={() => setTopup((t) => t + v)}>+{cop(v)}</button>
            ))}
          </div>
          <div class="row">
            <button class="btn primary grow" disabled={busy} onClick={doTopup}>{busy ? <Spinner /> : 'Aplicar recarga'}</button>
            <button class={`btn ${att.blocked ? '' : 'danger'}`} disabled={busy} onClick={toggleBlock}>
              {att.blocked ? 'Desbloquear' : 'Bloquear'}
            </button>
          </div>
        </div>
      )}

      {tab === 'ledger' && (
        <div>
          {ledger.loading && <Loading />}
          {ledger.error && <ErrorState message={ledger.error} onRetry={ledger.reload} />}
          {!ledger.loading && !ledger.error && (ledger.data ?? []).length === 0 && <EmptyState icon="◦" title="Sin movimientos" />}
          {!ledger.loading && !ledger.error && (ledger.data ?? []).length > 0 && (
            <div class="table-wrap">
              <table class="tbl">
                <thead><tr><th>Fecha</th><th>Tipo</th><th class="num">Monto</th><th class="num">Saldo</th></tr></thead>
                <tbody>
                  {ledger.data!.map((l) => (
                    <tr key={l.id}>
                      <td class="faint">{dateTime(l.created_at)}</td>
                      <td><span class="tag">{l.kind}</span></td>
                      <td class="num mono" style={l.amount >= 0 ? 'color:var(--green)' : 'color:var(--red)'}>{l.amount >= 0 ? '+' : ''}{cop(l.amount)}</td>
                      <td class="num mono">{l.balance_after != null ? cop(l.balance_after) : '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}

      {tab === 'qr' && (
        <div style="text-align:center">
          {att.qr_token ? (
            <>
              <div class="qr-box" dangerouslySetInnerHTML={{ __html: qrSvg(att.qr_token, { size: 220 }) }} />
              <div class="faint mono" style="margin-top:12px;font-size:12px;word-break:break-all">{att.qr_token}</div>
            </>
          ) : (
            <EmptyState icon="⛶" title="Sin token QR" hint="Este asistente no tiene token asignado." />
          )}
        </div>
      )}
    </Modal>
  );
}

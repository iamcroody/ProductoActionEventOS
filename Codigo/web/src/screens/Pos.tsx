import { useState } from 'preact/hooks';
import { Caja, Orders, Attendees, Events, ApiError } from '../lib/api';
import type { OrderPayload } from '../lib/api';
import { useAsync } from '../lib/hooks';
import { useHotkeys } from '../lib/hotkeys';
import { cop, copInput } from '../lib/format';
import { toast } from '../lib/toast';
import { Modal, Field, Spinner, Loading } from '../components/ui';
import { AttendeeSearch, ProductPicker, useCart } from '../components/order';
import type { Attendee, CajaSession } from '../lib/types';

export function Pos() {
  const caja = useAsync<CajaSession | null>(() => Caja.current(), []);
  const ev = useAsync(() => Events.active().catch(() => null), []);
  const defaultTopup = ev.data?.default_topup ?? 0;
  const cart = useCart();
  const [attendee, setAttendee] = useState<Attendee | null>(null);
  const [cashConfirmed, setCashConfirmed] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [success, setSuccess] = useState<{ total: number; cash: number; balance: number } | null>(null);

  // F3 cobra el carrito listo; ESC limpia el asistente seleccionado (los modales
  // se cierran solos con ESC). Las funciones referenciadas se evalúan al pulsar,
  // ya inicializadas tras el render.
  useHotkeys({
    F3: () => { if (open && !success) cobrar(); },
    Escape: () => {
      if (success) return;
      if (attendee) { setAttendee(null); setCashConfirmed(false); }
    },
  });

  if (caja.loading) return <Loading />;
  const session = caja.data;
  const open = !!session && session.status !== 'closed';

  const balance = attendee?.balance ?? 0;
  const overage = Math.max(0, cart.total - balance);
  const fromBalance = Math.min(balance, cart.total);
  const needsCash = overage > 0;

  const refreshAttendee = async () => {
    if (!attendee) return;
    try { setAttendee(await Attendees.get(attendee.id)); } catch { /* ignore */ }
  };

  const cobrar = async () => {
    if (!attendee) { toast.err('Selecciona un asistente'); return; }
    if (cart.lines.length === 0) { toast.err('El carrito está vacío'); return; }
    if (needsCash && !cashConfirmed) { toast.err('Confirma el efectivo recibido'); return; }
    setSubmitting(true);
    const payload: OrderPayload = {
      request_id: crypto.randomUUID(),
      items: cart.lines.map((l) => ({ product_id: l.product.id, qty: l.qty })),
      payers: [{ attendee_id: attendee.id }],
      cash_confirmed: needsCash ? cashConfirmed : false,
    };
    try {
      const order = await Orders.create(payload);
      setSuccess({
        total: order.total ?? cart.total,
        cash: order.paid_cash ?? overage,
        balance: order.paid_from_balance ?? fromBalance,
      });
      cart.clear();
      setCashConfirmed(false);
      await refreshAttendee();
    } catch (e) {
      toast.err(e instanceof ApiError ? e.message : 'No se pudo registrar la venta');
    } finally {
      setSubmitting(false);
    }
  };

  if (!open) {
    return <CajaClosed session={session} onOpened={(s) => caja.setData(s)} />;
  }

  return (
    <div>
      <div class="page-head spread">
        <div>
          <h1>Caja</h1>
          <p>Sesión abierta · base {cop(session!.opening_float)}</p>
        </div>
        <CloseCajaButton onClosed={() => caja.reload()} />
      </div>

      <div class="pos">
        <div class="pos-left">
          {attendee ? (
            <div class={`attendee-banner ${attendee.blocked ? '' : ''}`}>
              <div>
                <div style="font-size:19px;font-weight:700">{attendee.display_name}</div>
                <div class="faint" style="font-size:13px">
                  {attendee.doc_id ? `Doc ${attendee.doc_id}` : 'Sin documento'}
                  {attendee.blocked ? ' · ' : ''}
                  {attendee.blocked ? <span class="pill red">Bloqueado</span> : null}
                </div>
              </div>
              <div class="bal">
                <div class="l">Saldo</div>
                <div class="v mono" style={balance > 0 ? 'color:var(--green)' : 'color:var(--red)'}>{cop(balance)}</div>
              </div>
              <button class="btn ghost sm" onClick={() => { setAttendee(null); setCashConfirmed(false); }}>Cambiar</button>
            </div>
          ) : (
            <div class="card pad-lg">
              <div class="spread" style="margin-bottom:14px;gap:10px">
                <div>
                  <strong style="font-size:15px">Buscar o registrar asistente</strong>
                  <div class="faint" style="font-size:12.5px;margin-top:2px">
                    Empieza una venta: busca por nombre/documento, escanea su QR o regístralo al instante.
                  </div>
                </div>
                <span class="faint no-print" style="font-size:11px;white-space:nowrap">F2 buscar</span>
              </div>
              <AttendeeSearch onSelect={setAttendee} autoFocus defaultTopup={defaultTopup} />
            </div>
          )}

          <div class="card">
            <ProductPicker onPick={cart.add} />
          </div>
        </div>

        <aside class="cart" aria-label="Carrito">
          <div class="cart-head spread">
            <strong style="font-size:16px">Carrito</strong>
            {cart.count > 0 && <button class="btn ghost sm" onClick={cart.clear}>Vaciar</button>}
          </div>
          <div class="cart-items">
            {cart.lines.length === 0 ? (
              <div class="state" style="padding:28px 12px"><div class="faint">Toca un producto para agregarlo</div></div>
            ) : (
              cart.lines.map((l) => (
                <div class="cart-row" key={l.product.id}>
                  <div class="nm">
                    <div class="t">{l.product.name}</div>
                    <div class="p">{cop(l.product.price)} c/u</div>
                  </div>
                  <div class="qty">
                    <button onClick={() => cart.setQty(l.product.id, l.qty - 1)} aria-label="Menos">−</button>
                    <span class="n mono">{l.qty}</span>
                    <button onClick={() => cart.setQty(l.product.id, l.qty + 1)} aria-label="Más">+</button>
                  </div>
                  <div class="mono" style="min-width:74px;text-align:right;font-weight:600">{cop(l.product.price * l.qty)}</div>
                </div>
              ))
            )}
          </div>

          <div class="cart-foot">
            {needsCash && cart.lines.length > 0 && (
              <>
                <div class="tot-line"><span>Del saldo</span><span class="mono">{cop(fromBalance)}</span></div>
                <div class="cash-warn">
                  Saldo insuficiente. Cobra <strong>{cop(overage)}</strong> en efectivo.
                  <label class="toggle" style="margin-top:10px">
                    <input type="checkbox" checked={cashConfirmed} onChange={(e) => setCashConfirmed((e.target as HTMLInputElement).checked)} />
                    Confirmar efectivo recibido
                  </label>
                </div>
              </>
            )}
            <div class="tot-line big"><span>Total</span><span class="mono">{cop(cart.total)}</span></div>
            <button
              class="btn primary lg block"
              disabled={submitting || cart.lines.length === 0 || !attendee || (needsCash && !cashConfirmed)}
              onClick={cobrar}
            >
              {submitting ? <Spinner /> : `Cobrar ${cop(cart.total)}`}
            </button>
          </div>
        </aside>
      </div>

      {success && (
        <Modal title="Venta registrada" onClose={() => setSuccess(null)}
          footer={<button class="btn primary" onClick={() => setSuccess(null)} autoFocus>Continuar</button>}>
          <div style="text-align:center;padding:8px 0">
            <div style="font-size:42px;color:var(--green)">✓</div>
            <div style="font-size:26px;font-weight:700;margin-top:6px" class="mono">{cop(success.total)}</div>
            <div class="muted" style="margin-top:10px">
              Del saldo {cop(success.balance)}{success.cash > 0 ? ` · Efectivo ${cop(success.cash)}` : ''}
            </div>
          </div>
        </Modal>
      )}
    </div>
  );
}

function CajaClosed({ session, onOpened }: { session: CajaSession | null | undefined; onOpened: (s: CajaSession) => void }) {
  const [float, setFloat] = useState(0);
  const [busy, setBusy] = useState(false);

  const openCaja = async () => {
    setBusy(true);
    try {
      const s = await Caja.open(float);
      onOpened(s);
      toast.ok('Caja abierta');
    } catch (e) {
      toast.err(e instanceof ApiError ? e.message : 'No se pudo abrir la caja');
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <div class="page-head"><h1>Caja</h1><p>No hay una sesión de caja abierta.</p></div>
      <div class="card pad-lg" style="max-width:440px">
        <h3 style="margin:0 0 4px">Abrir caja</h3>
        <p class="muted" style="margin:0 0 18px">Ingresa la base inicial de efectivo para comenzar a vender.</p>
        <Field label="Base inicial (efectivo)">
          <input class="input mono" inputMode="numeric" value={float ? cop(float) : ''} placeholder="$0"
            onInput={(e) => setFloat(copInput((e.target as HTMLInputElement).value))} />
        </Field>
        <button class="btn primary lg block" style="margin-top:18px" disabled={busy} onClick={openCaja}>
          {busy ? <Spinner /> : 'Abrir caja'}
        </button>
      </div>
    </div>
  );
}

function CloseCajaButton({ onClosed }: { onClosed: () => void }) {
  const [show, setShow] = useState(false);
  const [counted, setCounted] = useState(0);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<{ expected_cash: number; variance: number } | null>(null);

  const close = async () => {
    setBusy(true);
    try {
      const r = await Caja.close(counted);
      setResult({ expected_cash: r.expected_cash, variance: r.variance });
    } catch (e) {
      toast.err(e instanceof ApiError ? e.message : 'No se pudo cerrar la caja');
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <button class="btn" onClick={() => { setShow(true); setResult(null); setCounted(0); }}>Cerrar caja</button>
      {show && (
        <Modal title="Cerrar caja" onClose={() => { setShow(false); if (result) onClosed(); }}
          footer={result
            ? <button class="btn primary" onClick={() => { setShow(false); onClosed(); }}>Listo</button>
            : <><button class="btn ghost" onClick={() => setShow(false)}>Cancelar</button>
              <button class="btn primary" disabled={busy} onClick={close}>{busy ? <Spinner /> : 'Confirmar cierre'}</button></>}>
          {result ? (
            <div style="text-align:center;padding:6px 0">
              <div class="muted">Efectivo esperado</div>
              <div class="mono" style="font-size:24px;font-weight:700">{cop(result.expected_cash)}</div>
              <div class="divider" />
              <div class="muted">Diferencia</div>
              <div class="mono" style={`font-size:28px;font-weight:700;color:${result.variance === 0 ? 'var(--green)' : Math.abs(result.variance) > 0 ? 'var(--amber)' : 'var(--text)'}`}>
                {result.variance > 0 ? '+' : ''}{cop(result.variance)}
              </div>
              <div class="faint" style="font-size:12.5px;margin-top:8px">
                {result.variance === 0 ? 'Cuadre exacto' : result.variance > 0 ? 'Sobrante' : 'Faltante'}
              </div>
            </div>
          ) : (
            <Field label="Efectivo contado">
              <input class="input mono" inputMode="numeric" autoFocus value={counted ? cop(counted) : ''} placeholder="$0"
                onInput={(e) => setCounted(copInput((e.target as HTMLInputElement).value))} />
            </Field>
          )}
        </Modal>
      )}
    </>
  );
}

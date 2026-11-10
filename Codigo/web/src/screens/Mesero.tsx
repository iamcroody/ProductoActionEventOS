import { useState } from 'preact/hooks';
import { Orders, ApiError } from '../lib/api';
import type { OrderPayload } from '../lib/api';
import { cop } from '../lib/format';
import { toast } from '../lib/toast';
import { Modal, Field, Spinner } from '../components/ui';
import { AttendeeSearch, ProductPicker, useCart } from '../components/order';
import type { Attendee } from '../lib/types';

export function Mesero() {
  const cart = useCart();
  const [attendee, setAttendee] = useState<Attendee | null>(null);
  const [table, setTable] = useState('');
  const [cashConfirmed, setCashConfirmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);

  const overage = attendee ? Math.max(0, cart.total - attendee.balance) : cart.total;
  const needsCash = overage > 0;

  const send = async () => {
    if (!attendee) { toast.err('Selecciona un asistente'); return; }
    if (cart.lines.length === 0) { toast.err('Agrega productos'); return; }
    if (needsCash && !cashConfirmed) { toast.err('Confirma el efectivo'); return; }
    setBusy(true);
    const payload: OrderPayload = {
      request_id: crypto.randomUUID(),
      table_label: table.trim() || undefined,
      items: cart.lines.map((l) => ({ product_id: l.product.id, qty: l.qty })),
      payers: [{ attendee_id: attendee.id }],
      cash_confirmed: needsCash ? cashConfirmed : false,
    };
    try {
      await Orders.create(payload);
      setDone(true);
      cart.clear();
      setCashConfirmed(false);
    } catch (e) {
      toast.err(e instanceof ApiError ? e.message : 'No se pudo enviar el pedido');
    } finally {
      setBusy(false);
    }
  };

  const reset = () => { setDone(false); setAttendee(null); setTable(''); };

  return (
    <div style="max-width:560px;margin:0 auto;padding-bottom:88px">
      <div class="page-head"><h1>Mesero</h1><p>Toma el pedido y envíalo a cocina/bar.</p></div>

      <div class="card" style="margin-bottom:14px">
        <Field label="Mesa / Ubicación">
          <input class="input" placeholder="Ej. Mesa 4" value={table}
            onInput={(e) => setTable((e.target as HTMLInputElement).value)} />
        </Field>
      </div>

      {attendee ? (
        <div class="attendee-banner" style="margin-bottom:14px">
          <div>
            <div style="font-weight:700;font-size:17px">{attendee.display_name}</div>
            <div class="faint" style="font-size:12.5px">Saldo {cop(attendee.balance)}</div>
          </div>
          <button class="btn ghost sm right" onClick={() => setAttendee(null)}>Cambiar</button>
        </div>
      ) : (
        <div class="card" style="margin-bottom:14px"><AttendeeSearch onSelect={setAttendee} /></div>
      )}

      <div class="card">
        <ProductPicker onPick={cart.add} />
      </div>

      {cart.lines.length > 0 && (
        <div class="card" style="margin-top:14px">
          <strong>Pedido</strong>
          <div class="divider" />
          {cart.lines.map((l) => (
            <div class="cart-row" key={l.product.id}>
              <div class="nm"><div class="t">{l.product.name}</div></div>
              <div class="qty">
                <button onClick={() => cart.setQty(l.product.id, l.qty - 1)}>−</button>
                <span class="n mono">{l.qty}</span>
                <button onClick={() => cart.setQty(l.product.id, l.qty + 1)}>+</button>
              </div>
              <div class="mono" style="min-width:70px;text-align:right">{cop(l.product.price * l.qty)}</div>
            </div>
          ))}
          {needsCash && attendee && (
            <div class="cash-warn" style="margin-top:12px">
              Saldo insuficiente. Cobra <strong>{cop(overage)}</strong> en efectivo.
              <label class="toggle" style="margin-top:10px">
                <input type="checkbox" checked={cashConfirmed}
                  onChange={(e) => setCashConfirmed((e.target as HTMLInputElement).checked)} />
                Confirmar efectivo
              </label>
            </div>
          )}
        </div>
      )}

      <div class="mesero-foot">
        <div class="spread" style="max-width:560px;margin:0 auto;width:100%">
          <div>
            <div class="faint" style="font-size:12px">Total · {cart.count} ítems</div>
            <div class="mono" style="font-size:21px;font-weight:700">{cop(cart.total)}</div>
          </div>
          <button class="btn primary lg" disabled={busy || cart.lines.length === 0 || !attendee || (needsCash && !cashConfirmed)} onClick={send}>
            {busy ? <Spinner /> : 'Enviar pedido'}
          </button>
        </div>
      </div>

      {done && (
        <Modal title="Pedido enviado" onClose={reset}
          footer={<button class="btn primary" onClick={reset} autoFocus>Nuevo pedido</button>}>
          <div style="text-align:center;padding:10px 0">
            <div style="font-size:42px;color:var(--green)">✓</div>
            <div class="muted" style="margin-top:8px">El pedido fue enviado a la estación correspondiente.</div>
          </div>
        </Modal>
      )}
    </div>
  );
}

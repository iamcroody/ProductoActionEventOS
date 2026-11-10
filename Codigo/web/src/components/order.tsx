import { useMemo, useState } from 'preact/hooks';
import { Attendees, Products, Categories, ApiError } from '../lib/api';
import { useAsync, useDebounced } from '../lib/hooks';
import { cop, copInput } from '../lib/format';
import { toast } from '../lib/toast';
import { Loading, EmptyState, ErrorState, Spinner, Field } from './ui';
import type { Attendee, Product } from '../lib/types';

// ---- cart hook ----
export interface CartLine { product: Product; qty: number; }

export function useCart() {
  const [lines, setLines] = useState<CartLine[]>([]);
  const add = (product: Product) =>
    setLines((ls) => {
      const ex = ls.find((l) => l.product.id === product.id);
      if (ex) return ls.map((l) => (l.product.id === product.id ? { ...l, qty: l.qty + 1 } : l));
      return [...ls, { product, qty: 1 }];
    });
  const setQty = (id: string, qty: number) =>
    setLines((ls) => ls.flatMap((l) => (l.product.id === id ? (qty <= 0 ? [] : [{ ...l, qty }]) : [l])));
  const clear = () => setLines([]);
  const total = useMemo(() => lines.reduce((s, l) => s + l.product.price * l.qty, 0), [lines]);
  const count = useMemo(() => lines.reduce((s, l) => s + l.qty, 0), [lines]);
  return { lines, add, setQty, clear, total, count };
}

// ---- Attendee search + QR scan + inline "registrar y vender" ----
export function AttendeeSearch(
  { onSelect, autoFocus, defaultTopup = 0 }:
  { onSelect: (a: Attendee) => void; autoFocus?: boolean; defaultTopup?: number },
) {
  const [q, setQ] = useState('');
  const [token, setToken] = useState('');
  const [scanBusy, setScanBusy] = useState(false);
  const [scanErr, setScanErr] = useState<string | null>(null);
  const [registering, setRegistering] = useState(false);
  const dq = useDebounced(q.trim(), 200);

  const { data, loading, error } = useAsync<Attendee[]>(
    () => (dq.length >= 1 ? Attendees.search(dq) : Promise.resolve([])),
    [dq],
  );

  const scan = async () => {
    const t = token.trim();
    if (!t) return;
    setScanBusy(true);
    setScanErr(null);
    try {
      const a = await Attendees.byToken(t);
      onSelect(a);
      setToken('');
      setQ('');
    } catch (e) {
      setScanErr(e instanceof Error ? e.message : 'Token inválido');
    } finally {
      setScanBusy(false);
    }
  };

  const reset = () => { setQ(''); setRegistering(false); };

  return (
    <div class="col">
      <div class="search-xl">
        <span class="ico">⌕</span>
        <input
          class="input"
          data-hotkey-search
          placeholder="Buscar asistente por nombre o documento…"
          value={q}
          autoFocus={autoFocus}
          onInput={(e) => { setQ((e.target as HTMLInputElement).value); setRegistering(false); }}
        />
      </div>

      <div class="row" style="gap:8px">
        <input
          class="input"
          placeholder="Escanear / pegar token QR"
          value={token}
          onInput={(e) => setToken((e.target as HTMLInputElement).value)}
          onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); scan(); } }}
        />
        <button class="btn" style="white-space:nowrap" onClick={scan} disabled={scanBusy || !token.trim()}>
          {scanBusy ? <Spinner /> : '⛶ Escanear'}
        </button>
      </div>
      {scanErr && <div class="cash-warn" style="background:var(--red-soft);color:var(--red)">{scanErr}</div>}

      {dq.length >= 1 && (
        <div style="min-height:40px">
          {loading && <Loading />}
          {error && <ErrorState message={error} />}
          {!loading && !error && data && data.length > 0 && (
            <div class="results">
              {data.map((a) => (
                <button key={a.id} class="result" onClick={() => { onSelect(a); setQ(''); }}>
                  <div>
                    <div class="rn">{a.display_name} {a.blocked ? <span class="pill red">Bloqueado</span> : null}</div>
                    {a.doc_id && <div class="rd">Doc {a.doc_id}</div>}
                  </div>
                  <div class="rb mono" style="color:var(--green)">{cop(a.balance)}</div>
                </button>
              ))}
            </div>
          )}
          {!loading && !error && data && data.length === 0 && !registering && (
            <div class="card pad-lg" style="text-align:center">
              <div class="faint" style="margin-bottom:14px">No hay asistentes para «{dq}»</div>
              <button class="btn primary lg block" onClick={() => setRegistering(true)}>
                + Registrar y vender
              </button>
            </div>
          )}
          {!loading && !error && data && data.length === 0 && registering && (
            <InlineRegister
              initialName={dq}
              defaultTopup={defaultTopup}
              onCancel={() => setRegistering(false)}
              onCreated={(a) => { onSelect(a); reset(); }}
            />
          )}
        </div>
      )}
    </div>
  );
}

// Inline create-attendee form shown at the till so a cashier can sell to a
// walk-up without leaving the sale. Asks only for name + optional recarga.
function InlineRegister(
  { initialName, defaultTopup, onCancel, onCreated }:
  { initialName: string; defaultTopup: number; onCancel: () => void; onCreated: (a: Attendee) => void },
) {
  const [name, setName] = useState(initialName);
  const [doc, setDoc] = useState('');
  const [phone, setPhone] = useState('');
  const [topup, setTopup] = useState(defaultTopup);
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    const display_name = name.trim();
    const doc_id = doc.trim();
    const phoneVal = phone.trim();
    if (!display_name) { toast.err('Escribe un nombre'); return; }
    if (!doc_id) { toast.err('Escribe el documento'); return; }
    if (!phoneVal) { toast.err('Escribe el celular'); return; }
    setBusy(true);
    try {
      const a = await Attendees.create({
        display_name,
        doc_id,
        phone: phoneVal,
        ...(topup > 0 ? { initial_topup: topup } : {}),
      });
      toast.ok(`${a.display_name} registrado`);
      onCreated(a);
    } catch (e) {
      toast.err(e instanceof ApiError ? e.message : 'No se pudo registrar');
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="card pad-lg col">
      <strong style="font-size:15px">Registrar y vender</strong>
      <Field label="Nombre">
        <input
          class="input"
          value={name}
          autoFocus
          placeholder="Nombre del asistente"
          onInput={(e) => setName((e.target as HTMLInputElement).value)}
          onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); submit(); } }}
        />
      </Field>
      <Field label="Documento">
        <input
          class="input"
          value={doc}
          inputMode="numeric"
          placeholder="Cédula / documento"
          onInput={(e) => setDoc((e.target as HTMLInputElement).value)}
          onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); submit(); } }}
        />
      </Field>
      <Field label="Celular">
        <input
          class="input"
          value={phone}
          inputMode="tel"
          placeholder="300 000 0000"
          onInput={(e) => setPhone((e.target as HTMLInputElement).value)}
          onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); submit(); } }}
        />
      </Field>
      <Field label="Recarga inicial (opcional)">
        <input
          class="input mono"
          inputMode="numeric"
          value={topup ? cop(topup) : ''}
          placeholder="$0"
          onInput={(e) => setTopup(copInput((e.target as HTMLInputElement).value))}
          onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); submit(); } }}
        />
      </Field>
      <div class="row wrap" style="gap:8px">
        <button class="btn primary lg grow" disabled={busy} onClick={submit}>
          {busy ? <Spinner /> : 'Registrar y vender'}
        </button>
        <button class="btn ghost" disabled={busy} onClick={onCancel}>Cancelar</button>
      </div>
    </div>
  );
}

// ---- Product picker ----
export function ProductPicker({ onPick }: { onPick: (p: Product) => void }) {
  const cats = useAsync(() => Categories.list(), []);
  const prods = useAsync(() => Products.list({ active: 1 }), []);
  const [cat, setCat] = useState<string>('all');

  if (prods.loading || cats.loading) return <Loading />;
  if (prods.error) return <ErrorState message={prods.error} onRetry={prods.reload} />;

  const products = prods.data ?? [];
  const categories = (cats.data ?? []).filter((c) => c.active !== 0);
  const filtered = cat === 'all' ? products : products.filter((p) => p.category_id === cat);

  return (
    <div class="col">
      <div class="cat-bar">
        <button class={`cat-chip ${cat === 'all' ? 'active' : ''}`} onClick={() => setCat('all')}>Todo</button>
        {categories.map((c) => (
          <button key={c.id} class={`cat-chip ${cat === c.id ? 'active' : ''}`} onClick={() => setCat(c.id)}>
            {c.name}
          </button>
        ))}
      </div>

      {filtered.length === 0 ? (
        <EmptyState icon="◦" title="Sin productos" hint="No hay productos en esta categoría." />
      ) : (
        <div class="prod-grid">
          {filtered.map((p) => {
            const tracks = p.track_stock === 1;
            const out = tracks && (p.stock_qty ?? 0) <= 0;
            const low = tracks && !out && (p.stock_qty ?? 0) <= 5;
            return (
              <button key={p.id} class={`prod ${out ? 'out' : ''}`} onClick={() => onPick(p)} disabled={out}>
                <div class="pname">{p.name}</div>
                <div class="spread">
                  <span class="pprice">{cop(p.price)}</span>
                  {tracks && (
                    <span class={`pstock ${low ? 'low' : ''}`}>{out ? 'Agotado' : `${p.stock_qty}`}</span>
                  )}
                </div>
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}

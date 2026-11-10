import { useState } from 'preact/hooks';
import { Categories, Products, Inventory, ApiError } from '../../lib/api';
import { useAsync } from '../../lib/hooks';
import { cop, copInput } from '../../lib/format';
import { toast } from '../../lib/toast';
import { Modal, Field, Spinner, Loading, EmptyState, ErrorState } from '../../components/ui';
import type { Category, Product } from '../../lib/types';

export function MenuTab({ locked = false }: { locked?: boolean }) {
  const cats = useAsync<Category[]>(() => Categories.list(), []);
  const prods = useAsync<Product[]>(() => Products.list(), []);
  const lowStock = useAsync(() => Inventory.lowStock(5), []);
  const [editProd, setEditProd] = useState<Product | null>(null);
  const [newProd, setNewProd] = useState(false);
  const [newCat, setNewCat] = useState(false);
  const [adjust, setAdjust] = useState<Product | null>(null);

  const reloadAll = () => { prods.reload(); lowStock.reload(); };

  const catName = (id?: string | null) => cats.data?.find((c) => c.id === id)?.name ?? '—';

  return (
    <div>
      {(lowStock.data ?? []).length > 0 && (
        <div class="card" style="margin-bottom:16px;border-color:var(--amber-soft)">
          <strong style="color:var(--amber)">⚠ Bajo inventario</strong>
          <div class="row wrap" style="margin-top:10px;gap:8px">
            {lowStock.data!.map((p) => (
              <span key={p.id} class={`pill ${p.stock_qty <= 0 ? 'red' : 'amber'}`}>{p.name}: {p.stock_qty}</span>
            ))}
          </div>
        </div>
      )}

      <div class="spread" style="margin-bottom:16px">
        <strong>Categorías</strong>
        <button class="btn sm" disabled={locked} onClick={() => setNewCat(true)}>+ Categoría</button>
      </div>
      {cats.loading ? <Loading /> : (
        <div class="row wrap" style="margin-bottom:24px;gap:8px">
          {(cats.data ?? []).length === 0 && <span class="faint">Sin categorías</span>}
          {(cats.data ?? []).map((c) => (
            <span key={c.id} class={`tag ${c.active === 0 ? '' : ''}`} style="padding:7px 12px;font-size:13px">
              {c.name}{c.active === 0 ? ' (inactiva)' : ''}
            </span>
          ))}
        </div>
      )}

      <div class="spread" style="margin-bottom:16px">
        <strong>Productos</strong>
        <button class="btn primary sm" disabled={locked} onClick={() => setNewProd(true)}>+ Producto</button>
      </div>

      {prods.loading && <Loading />}
      {prods.error && <ErrorState message={prods.error} onRetry={prods.reload} />}
      {!prods.loading && !prods.error && (prods.data ?? []).length === 0 && <EmptyState icon="◦" title="Sin productos" />}
      {!prods.loading && !prods.error && (prods.data ?? []).length > 0 && (
        <div class="table-wrap">
          <table class="tbl">
            <thead><tr><th>Producto</th><th>Categoría</th><th class="num">Precio</th><th class="num">Costo</th><th class="num">Ganancia</th><th class="num">Stock</th><th>Estado</th><th></th></tr></thead>
            <tbody>
              {prods.data!.map((p) => (
                <tr key={p.id}>
                  <td style="font-weight:600">{p.name}</td>
                  <td class="faint">{catName(p.category_id)}</td>
                  <td class="num mono">{cop(p.price)}</td>
                  <td class="num mono faint">{p.cost ? cop(p.cost) : '—'}</td>
                  <td class="num mono" style={`color:${(p.price - (p.cost ?? 0)) >= 0 ? 'var(--green)' : 'var(--red)'}`}>{cop(p.price - (p.cost ?? 0))}</td>
                  <td class="num mono">{p.track_stock === 1 ? p.stock_qty : '∞'}</td>
                  <td><span class={`pill ${p.active === 0 ? 'red' : 'green'}`}>{p.active === 0 ? 'Inactivo' : 'Activo'}</span></td>
                  <td class="num">
                    <div class="row" style="justify-content:flex-end;gap:6px">
                      {p.track_stock === 1 && <button class="btn ghost sm" onClick={() => setAdjust(p)}>Stock</button>}
                      <button class="btn ghost sm" disabled={locked} onClick={() => setEditProd(p)}>Editar</button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {newCat && <CategoryModal onClose={() => setNewCat(false)} onSaved={() => { setNewCat(false); cats.reload(); }} />}
      {(newProd || editProd) && (
        <ProductModal product={editProd} categories={cats.data ?? []}
          onClose={() => { setNewProd(false); setEditProd(null); }}
          onSaved={() => { setNewProd(false); setEditProd(null); reloadAll(); }} />
      )}
      {adjust && <AdjustModal product={adjust} onClose={() => setAdjust(null)} onSaved={() => { setAdjust(null); reloadAll(); }} />}
    </div>
  );
}

function CategoryModal({ onClose, onSaved }: { onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const save = async () => {
    if (!name.trim()) { toast.err('Ingresa el nombre'); return; }
    setBusy(true);
    try { await Categories.create({ name: name.trim(), sort_order: 0 }); toast.ok('Categoría creada'); onSaved(); }
    catch (e) { toast.err(e instanceof ApiError ? e.message : 'Error'); } finally { setBusy(false); }
  };
  return (
    <Modal title="Nueva categoría" onClose={onClose}
      footer={<><button class="btn ghost" onClick={onClose}>Cancelar</button><button class="btn primary" disabled={busy} onClick={save}>{busy ? <Spinner /> : 'Crear'}</button></>}>
      <Field label="Nombre"><input class="input" autoFocus value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} /></Field>
    </Modal>
  );
}

function ProductModal({ product, categories, onClose, onSaved }: { product: Product | null; categories: Category[]; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState(product?.name ?? '');
  const [price, setPrice] = useState(product?.price ?? 0);
  const [cost, setCost] = useState(product?.cost ?? 0);
  const [categoryId, setCategoryId] = useState(product?.category_id ?? '');
  const [track, setTrack] = useState((product?.track_stock ?? 0) === 1);
  const [stock, setStock] = useState(product?.stock_qty ?? 0);
  const [active, setActive] = useState((product?.active ?? 1) === 1);
  const [busy, setBusy] = useState(false);
  const editing = !!product;

  const save = async () => {
    if (!name.trim()) { toast.err('Ingresa el nombre'); return; }
    setBusy(true);
    const body = {
      name: name.trim(), price, cost, category_id: categoryId || undefined,
      track_stock: track ? 1 : 0, stock_qty: stock, active: active ? 1 : 0,
    };
    try {
      if (editing) await Products.update(product!.id, body);
      else await Products.create(body);
      toast.ok(editing ? 'Producto actualizado' : 'Producto creado');
      onSaved();
    } catch (e) { toast.err(e instanceof ApiError ? e.message : 'Error'); }
    finally { setBusy(false); }
  };

  const remove = async () => {
    setBusy(true);
    try { await Products.remove(product!.id); toast.ok('Producto eliminado'); onSaved(); }
    catch (e) { toast.err(e instanceof ApiError ? e.message : 'Error'); } finally { setBusy(false); }
  };

  return (
    <Modal title={editing ? 'Editar producto' : 'Nuevo producto'} onClose={onClose}
      footer={<>
        {editing && <button class="btn danger" style="margin-right:auto" disabled={busy} onClick={remove}>Eliminar</button>}
        <button class="btn ghost" onClick={onClose}>Cancelar</button>
        <button class="btn primary" disabled={busy} onClick={save}>{busy ? <Spinner /> : 'Guardar'}</button>
      </>}>
      <Field label="Nombre"><input class="input" autoFocus value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} /></Field>
      <div class="field-row">
        <Field label="Precio">
          <input class="input mono" inputMode="numeric" value={price ? cop(price) : ''} placeholder="$0"
            onInput={(e) => setPrice(copInput((e.target as HTMLInputElement).value))} />
        </Field>
        <Field label="Costo (valor al por mayor)">
          <input class="input mono" inputMode="numeric" value={cost ? cop(cost) : ''} placeholder="$0"
            onInput={(e) => setCost(copInput((e.target as HTMLInputElement).value))} />
        </Field>
      </div>
      <div class="faint" style="margin:-6px 0 14px;font-size:13px">
        Ganancia por unidad: <strong class="mono" style={`color:${(price - cost) >= 0 ? 'var(--green)' : 'var(--red)'}`}>{cop(price - cost)}</strong>
      </div>
      <div class="field-row">
        <Field label="Categoría">
          <select class="select" value={categoryId ?? ''} onChange={(e) => setCategoryId((e.target as HTMLSelectElement).value)}>
            <option value="">Sin categoría</option>
            {categories.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
          </select>
        </Field>
        <Field label="Inventario">
          <label class="toggle" style="height:46px">
            <input type="checkbox" checked={track} onChange={(e) => setTrack((e.target as HTMLInputElement).checked)} />
            Controlar stock
          </label>
        </Field>
      </div>
      {track && (
        <Field label="Stock actual">
          <input class="input mono" inputMode="numeric" value={String(stock)} onInput={(e) => setStock(parseInt((e.target as HTMLInputElement).value || '0', 10))} />
        </Field>
      )}
      <label class="toggle"><input type="checkbox" checked={active} onChange={(e) => setActive((e.target as HTMLInputElement).checked)} /> Activo (visible en POS)</label>
    </Modal>
  );
}

function AdjustModal({ product, onClose, onSaved }: { product: Product; onClose: () => void; onSaved: () => void }) {
  const [delta, setDelta] = useState(0);
  const [reason, setReason] = useState('restock');
  const [busy, setBusy] = useState(false);
  const save = async () => {
    if (delta === 0) { toast.err('Ingresa una cantidad'); return; }
    setBusy(true);
    try { await Inventory.adjust({ product_id: product.id, delta, reason }); toast.ok('Inventario ajustado'); onSaved(); }
    catch (e) { toast.err(e instanceof ApiError ? e.message : 'Error'); } finally { setBusy(false); }
  };
  return (
    <Modal title={`Ajustar stock · ${product.name}`} onClose={onClose}
      footer={<><button class="btn ghost" onClick={onClose}>Cancelar</button><button class="btn primary" disabled={busy} onClick={save}>{busy ? <Spinner /> : 'Aplicar'}</button></>}>
      <div class="faint">Stock actual: <strong class="mono">{product.stock_qty}</strong></div>
      <div class="field-row">
        <Field label="Cambio (+/-)">
          <input class="input mono" type="number" value={String(delta)} onInput={(e) => setDelta(parseInt((e.target as HTMLInputElement).value || '0', 10))} />
        </Field>
        <Field label="Motivo">
          <select class="select" value={reason} onChange={(e) => setReason((e.target as HTMLSelectElement).value)}>
            <option value="restock">Reabastecimiento</option>
            <option value="waste">Merma</option>
            <option value="adjustment">Ajuste</option>
          </select>
        </Field>
      </div>
    </Modal>
  );
}

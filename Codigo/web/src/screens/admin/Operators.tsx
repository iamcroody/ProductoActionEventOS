import { useState } from 'preact/hooks';
import { Users, ApiError } from '../../lib/api';
import { useAsync } from '../../lib/hooks';
import { toast } from '../../lib/toast';
import { Modal, Field, Spinner, Loading, EmptyState, ErrorState } from '../../components/ui';
import type { Operator, Role } from '../../lib/types';

const ROLES: Role[] = ['admin', 'caja', 'mesero', 'cocina', 'bar', 'auditor'];

export function OperatorsTab() {
  const { data, loading, error, reload } = useAsync<Operator[]>(() => Users.list(), []);
  const [edit, setEdit] = useState<Operator | null>(null);
  const [create, setCreate] = useState(false);
  const [pinFor, setPinFor] = useState<Operator | null>(null);

  return (
    <div>
      <div class="spread" style="margin-bottom:16px">
        <strong>Operadores</strong>
        <button class="btn primary sm" onClick={() => setCreate(true)}>+ Operador</button>
      </div>

      {loading && <Loading />}
      {error && <ErrorState message={error} onRetry={reload} />}
      {!loading && !error && (data ?? []).length === 0 && <EmptyState icon="◦" title="Sin operadores" />}
      {!loading && !error && (data ?? []).length > 0 && (
        <div class="table-wrap">
          <table class="tbl">
            <thead><tr><th>Usuario</th><th>Nombre</th><th>Rol</th><th>Estado</th><th></th></tr></thead>
            <tbody>
              {data!.map((u) => (
                <tr key={u.id}>
                  <td style="font-weight:600">{u.username}</td>
                  <td>{u.full_name}</td>
                  <td><span class="pill blue" style="text-transform:capitalize">{u.role}</span></td>
                  <td><span class={`pill ${u.active === 0 ? 'red' : 'green'}`}>{u.active === 0 ? 'Inactivo' : 'Activo'}</span></td>
                  <td class="num">
                    <div class="row" style="justify-content:flex-end;gap:6px">
                      <button class="btn ghost sm" onClick={() => setPinFor(u)}>PIN</button>
                      <button class="btn ghost sm" onClick={() => setEdit(u)}>Editar</button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {create && <OperatorModal onClose={() => setCreate(false)} onSaved={() => { setCreate(false); reload(); }} />}
      {edit && <OperatorModal operator={edit} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); reload(); }} />}
      {pinFor && <PinModal operator={pinFor} onClose={() => setPinFor(null)} />}
    </div>
  );
}

function OperatorModal({ operator, onClose, onSaved }: { operator?: Operator; onClose: () => void; onSaved: () => void }) {
  const editing = !!operator;
  const [username, setUsername] = useState(operator?.username ?? '');
  const [fullName, setFullName] = useState(operator?.full_name ?? '');
  const [role, setRole] = useState<Role>(operator?.role ?? 'caja');
  const [pin, setPin] = useState('');
  const [active, setActive] = useState((operator?.active ?? 1) === 1);
  const [busy, setBusy] = useState(false);

  const save = async () => {
    if (!fullName.trim()) { toast.err('Ingresa el nombre'); return; }
    setBusy(true);
    try {
      if (editing) {
        await Users.update(operator!.id, { full_name: fullName.trim(), role, active: active ? 1 : 0 });
      } else {
        if (!username.trim()) { toast.err('Ingresa el usuario'); setBusy(false); return; }
        if (pin.length < 4) { toast.err('PIN de al menos 4 dígitos'); setBusy(false); return; }
        await Users.create({ username: username.trim(), full_name: fullName.trim(), role, pin });
      }
      toast.ok(editing ? 'Operador actualizado' : 'Operador creado');
      onSaved();
    } catch (e) { toast.err(e instanceof ApiError ? e.message : 'Error'); }
    finally { setBusy(false); }
  };

  const remove = async () => {
    setBusy(true);
    try { await Users.remove(operator!.id); toast.ok('Operador eliminado'); onSaved(); }
    catch (e) { toast.err(e instanceof ApiError ? e.message : 'Error'); } finally { setBusy(false); }
  };

  return (
    <Modal title={editing ? 'Editar operador' : 'Nuevo operador'} onClose={onClose}
      footer={<>
        {editing && <button class="btn danger" style="margin-right:auto" disabled={busy} onClick={remove}>Eliminar</button>}
        <button class="btn ghost" onClick={onClose}>Cancelar</button>
        <button class="btn primary" disabled={busy} onClick={save}>{busy ? <Spinner /> : 'Guardar'}</button>
      </>}>
      <div class="field-row">
        <Field label="Usuario">
          <input class="input" value={username} disabled={editing} onInput={(e) => setUsername((e.target as HTMLInputElement).value)} />
        </Field>
        <Field label="Rol">
          <select class="select" value={role} onChange={(e) => setRole((e.target as HTMLSelectElement).value as Role)}>
            {ROLES.map((r) => <option key={r} value={r} style="text-transform:capitalize">{r}</option>)}
          </select>
        </Field>
      </div>
      <Field label="Nombre completo"><input class="input" value={fullName} onInput={(e) => setFullName((e.target as HTMLInputElement).value)} /></Field>
      {!editing && (
        <Field label="PIN inicial">
          <input class="input mono" inputMode="numeric" value={pin} onInput={(e) => setPin((e.target as HTMLInputElement).value.replace(/\D/g, '').slice(0, 8))} />
        </Field>
      )}
      {editing && <label class="toggle"><input type="checkbox" checked={active} onChange={(e) => setActive((e.target as HTMLInputElement).checked)} /> Activo</label>}
    </Modal>
  );
}

function PinModal({ operator, onClose }: { operator: Operator; onClose: () => void }) {
  const [pin, setPin] = useState('');
  const [busy, setBusy] = useState(false);
  const save = async () => {
    if (pin.length < 4) { toast.err('PIN de al menos 4 dígitos'); return; }
    setBusy(true);
    try { await Users.setPin(operator.id, pin); toast.ok('PIN actualizado'); onClose(); }
    catch (e) { toast.err(e instanceof ApiError ? e.message : 'Error'); } finally { setBusy(false); }
  };
  return (
    <Modal title={`Cambiar PIN · ${operator.username}`} onClose={onClose}
      footer={<><button class="btn ghost" onClick={onClose}>Cancelar</button><button class="btn primary" disabled={busy} onClick={save}>{busy ? <Spinner /> : 'Guardar'}</button></>}>
      <Field label="Nuevo PIN">
        <input class="input mono" inputMode="numeric" autoFocus value={pin} onInput={(e) => setPin((e.target as HTMLInputElement).value.replace(/\D/g, '').slice(0, 8))} />
      </Field>
    </Modal>
  );
}

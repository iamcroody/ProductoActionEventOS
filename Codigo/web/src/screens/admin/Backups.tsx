import { useState } from 'preact/hooks';
import { Backups, ApiError } from '../../lib/api';
import { useAsync } from '../../lib/hooks';
import { dateTime } from '../../lib/format';
import { toast } from '../../lib/toast';
import { Spinner, Loading, EmptyState, ErrorState } from '../../components/ui';
import type { Backup } from '../../lib/types';

function fmtSize(b?: number): string {
  if (!b) return '—';
  if (b < 1024) return `${b} B`;
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)} KB`;
  return `${(b / 1024 / 1024).toFixed(1)} MB`;
}

export function BackupsTab() {
  const { data, loading, error, reload } = useAsync<Backup[]>(() => Backups.list(), []);
  const [creating, setCreating] = useState(false);
  const [verifying, setVerifying] = useState<string | null>(null);
  const [results, setResults] = useState<Record<string, boolean>>({});

  const create = async () => {
    setCreating(true);
    try { await Backups.create(); toast.ok('Respaldo creado'); reload(); }
    catch (e) { toast.err(e instanceof ApiError ? e.message : 'Error'); } finally { setCreating(false); }
  };

  const verify = async (id: string) => {
    setVerifying(id);
    try {
      const r = await Backups.verify(id);
      setResults((m) => ({ ...m, [id]: r.valid }));
      r.valid ? toast.ok('Respaldo íntegro') : toast.err('Respaldo corrupto');
    } catch (e) { toast.err(e instanceof ApiError ? e.message : 'Error'); }
    finally { setVerifying(null); }
  };

  return (
    <div>
      <div class="spread" style="margin-bottom:16px">
        <div><strong>Respaldos</strong><div class="faint" style="font-size:13px">Copias de seguridad de la base de datos.</div></div>
        <button class="btn primary sm" disabled={creating} onClick={create}>{creating ? <Spinner /> : '+ Crear respaldo'}</button>
      </div>

      {loading && <Loading />}
      {error && <ErrorState message={error} onRetry={reload} />}
      {!loading && !error && (data ?? []).length === 0 && <EmptyState icon="◦" title="Sin respaldos" hint="Crea el primer respaldo." />}
      {!loading && !error && (data ?? []).length > 0 && (
        <div class="table-wrap">
          <table class="tbl">
            <thead><tr><th>Nombre</th><th>Fecha</th><th class="num">Tamaño</th><th>Verificación</th><th></th></tr></thead>
            <tbody>
              {data!.map((b) => (
                <tr key={b.id}>
                  <td style="font-weight:600" class="mono">{b.name ?? b.id}</td>
                  <td class="faint">{dateTime(b.created_at)}</td>
                  <td class="num mono">{fmtSize(b.size)}</td>
                  <td>{results[b.id] === undefined ? <span class="faint">—</span> : <span class={`pill ${results[b.id] ? 'green' : 'red'}`}>{results[b.id] ? 'Íntegro' : 'Corrupto'}</span>}</td>
                  <td class="num"><button class="btn ghost sm" disabled={verifying === b.id} onClick={() => verify(b.id)}>{verifying === b.id ? <Spinner /> : 'Verificar'}</button></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

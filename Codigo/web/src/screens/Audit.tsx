import { useState } from 'preact/hooks';
import { Oversight, ApiError } from '../lib/api';
import { useAsync } from '../lib/hooks';
import { dateTime } from '../lib/format';
import { Loading, ErrorState, EmptyState, Spinner } from '../components/ui';
import { toast } from '../lib/toast';
import type { AuditEntry, IntegrityResult } from '../lib/types';

export function Audit() {
  const { data, loading, error, reload } = useAsync<AuditEntry[]>(() => Oversight.audit({ limit: 200 }), []);
  const [checking, setChecking] = useState(false);
  const [result, setResult] = useState<{ integrity: IntegrityResult; auditOk: boolean } | null>(null);

  const verify = async () => {
    setChecking(true);
    setResult(null);
    try {
      const [integrity, audit] = await Promise.all([Oversight.integrity(), Oversight.verify()]);
      setResult({ integrity, auditOk: audit.intact });
      if (integrity.ok && audit.intact) toast.ok('Integridad verificada');
      else toast.err('Se detectaron inconsistencias');
    } catch (e) {
      toast.err(e instanceof ApiError ? e.message : 'No se pudo verificar');
    } finally {
      setChecking(false);
    }
  };

  return (
    <div>
      <div class="page-head spread">
        <div><h1>Auditoría</h1><p>Registro inmutable de operaciones y verificación de integridad.</p></div>
        <div class="row no-print">
          <a class="btn ghost sm" href={Oversight.auditCsvUrl()} download>↓ CSV</a>
          <button class="btn primary sm" onClick={verify} disabled={checking}>
            {checking ? <Spinner /> : '⛨ Verificar integridad'}
          </button>
        </div>
      </div>

      {result && (
        <div class="card" style={`margin-bottom:18px;border-color:${result.integrity.ok && result.auditOk ? 'var(--green)' : 'var(--red)'}`}>
          <div class="row" style="gap:14px">
            <div style={`font-size:30px;color:${result.integrity.ok && result.auditOk ? 'var(--green)' : 'var(--red)'}`}>
              {result.integrity.ok && result.auditOk ? '✓' : '⚠'}
            </div>
            <div class="grow">
              <strong>{result.integrity.ok && result.auditOk ? 'Sin manipulaciones detectadas' : 'Inconsistencias detectadas'}</strong>
              <div class="row wrap" style="margin-top:10px;gap:8px">
                <span class={`pill ${result.auditOk ? 'green' : 'red'}`}>Cadena de auditoría {result.auditOk ? 'íntegra' : 'rota'}</span>
                <span class={`pill ${(result.integrity.balance_mismatches?.length ?? 0) === 0 ? 'green' : 'red'}`}>
                  Saldos: {result.integrity.balance_mismatches?.length ?? 0} discrepancias
                </span>
                <span class={`pill ${(result.integrity.stock_mismatches?.length ?? 0) === 0 ? 'green' : 'red'}`}>
                  Inventario: {result.integrity.stock_mismatches?.length ?? 0} discrepancias
                </span>
              </div>
            </div>
          </div>
        </div>
      )}

      {loading && <Loading />}
      {error && <ErrorState message={error} onRetry={reload} />}
      {!loading && !error && data && data.length === 0 && <EmptyState icon="◦" title="Sin registros de auditoría" />}
      {!loading && !error && data && data.length > 0 && (
        <div class="table-wrap">
          <table class="tbl">
            <thead>
              <tr><th>Fecha</th><th>Acción</th><th>Operador</th><th>Detalle</th></tr>
            </thead>
            <tbody>
              {data.map((e) => (
                <tr key={e.id}>
                  <td class="faint" style="white-space:nowrap">{e.at ? dateTime(e.at) : '—'}</td>
                  <td><span class="tag">{e.action}</span></td>
                  <td>{e.operator_name || '—'}</td>
                  <td class="faint" style="max-width:420px;overflow:hidden;text-overflow:ellipsis">{e.detail_json || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

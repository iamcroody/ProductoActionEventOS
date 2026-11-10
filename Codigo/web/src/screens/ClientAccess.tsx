import { useState } from 'preact/hooks';
import { Client, Staff } from '../lib/api';
import { useAsync } from '../lib/hooks';
import { Loading, ErrorState } from '../components/ui';
import { printRegion } from '../lib/print';
import { toast } from '../lib/toast';
import type { ClientInfo, StaffInfo } from '../lib/types';

// Staff-facing panel: shows the event-wide customer QR + URL so clients can
// self-order from their phones on a separate port (no management access), plus
// a PRIVATE staff-access QR (carries the gate key) to onboard staff phones.
export function ClientAccess() {
  const { data, loading, error, reload } = useAsync<ClientInfo>(() => Client.info(), []);
  const staff = useAsync<StaffInfo>(() => Staff.info(), []);
  const [showStaff, setShowStaff] = useState(false);

  const copyUrl = async (url: string) => {
    try {
      await navigator.clipboard.writeText(url);
      toast.ok('URL copiada');
    } catch {
      toast.err('No se pudo copiar');
    }
  };

  return (
    <div>
      <div class="page-head spread no-print">
        <div>
          <h1>Accesos</h1>
          <p>QR público para clientes y QR privado para que tu personal entre desde el celular.</p>
        </div>
      </div>

      {loading && <Loading />}
      {error && <ErrorState message={error} onRetry={reload} />}

      {!loading && !error && data && (
        <div class="client-access">
          <div class="card pad-lg print-region" style="text-align:center;max-width:460px">
            <div class="qr-box" style="margin:0 auto">
              <img
                src={Client.qrUrl}
                alt="QR de acceso de clientes"
                width={260}
                height={260}
                style="display:block;width:260px;height:260px"
              />
            </div>
            <div style="font-weight:700;font-size:18px;margin-top:16px">Pide desde tu celular</div>
            <div class="mono" style="margin-top:6px;word-break:break-all;color:var(--accent-bright)">
              {data.client_url}
            </div>
            <div class="row no-print" style="justify-content:center;margin-top:18px;gap:10px">
              <button class="btn" onClick={() => copyUrl(data.client_url)}>⧉ Copiar URL</button>
              <button class="btn primary" onClick={printRegion}>⎙ Imprimir</button>
            </div>
          </div>

          <div class="card pad-lg no-print" style="max-width:460px">
            <h3 style="margin:0 0 10px">¿Cómo funciona?</h3>
            <p class="muted" style="margin:0;line-height:1.55">
              Los clientes escanean este QR para pedir desde su celular. La web de clientes
              corre en un <strong>puerto separado</strong> y <strong>no da acceso a la gestión</strong>
              {' '}del evento: solo pueden ver el menú y enviar pedidos.
            </p>
            <div class="divider" />
            <p class="muted" style="margin:0;font-size:13.5px">
              Sugerencia: imprime el QR y colócalo en las mesas o en la entrada. Cada
              asistente también tiene su QR personal (en <strong>Asistentes → Ver → QR</strong>)
              que abre la web con su saldo ya cargado.
            </p>
          </div>

          <div class="card pad-lg no-print" style="max-width:460px;border-color:var(--amber)">
            <h3 style="margin:0 0 6px">Acceso del personal (privado)</h3>
            <p class="muted" style="margin:0 0 12px;line-height:1.55">
              Los meseros y cajeros entran desde su celular escaneando este QR. La consola
              de gestión es <strong>invisible</strong> en la red: solo se abre con esta llave,
              así que <strong>muéstralo únicamente a tu personal</strong> — nunca al público.
            </p>
            {!showStaff && (
              <button class="btn" onClick={() => setShowStaff(true)}>👁 Mostrar QR de staff</button>
            )}
            {showStaff && (
              staff.loading ? <Loading /> :
              staff.error ? <ErrorState message={staff.error} onRetry={staff.reload} /> :
              staff.data && (
                <div style="text-align:center">
                  <div class="qr-box" style="margin:0 auto">
                    <img
                      src={Staff.qrUrl}
                      alt="QR de acceso del personal"
                      width={220}
                      height={220}
                      style="display:block;width:220px;height:220px"
                    />
                  </div>
                  <div class="mono" style="margin-top:10px;word-break:break-all;color:var(--amber)">
                    {staff.data.staff_url}
                  </div>
                  <div class="row" style="justify-content:center;margin-top:14px;gap:10px">
                    <button class="btn" onClick={() => copyUrl(staff.data!.staff_url)}>⧉ Copiar enlace</button>
                    <button class="btn ghost" onClick={() => setShowStaff(false)}>Ocultar</button>
                  </div>
                </div>
              )
            )}
          </div>
        </div>
      )}

      <style>{`
        .client-access { display: grid; grid-template-columns: repeat(auto-fit, minmax(320px, 1fr)); gap: 18px; align-items: start; }
      `}</style>
    </div>
  );
}

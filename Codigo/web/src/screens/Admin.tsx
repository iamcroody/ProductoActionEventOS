import { useLocation, useRoute } from 'preact-iso';
import { EventsTab } from './admin/Events';
import { MenuTab } from './admin/Menu';
import { OperatorsTab } from './admin/Operators';
import { SettingsTab } from './admin/Settings';
import { BackupsTab } from './admin/Backups';
import { TipsTab } from './admin/Tips';
import { Attendees } from './Attendees';
import { Events } from '../lib/api';
import { useAsync } from '../lib/hooks';
import type { EventItem } from '../lib/types';

const TABS = [
  { key: 'eventos', label: 'Eventos' },
  { key: 'menu', label: 'Menú' },
  { key: 'asistentes', label: 'Asistentes' },
  { key: 'operadores', label: 'Operadores' },
  { key: 'propinas', label: 'Propinas' },
  { key: 'config', label: 'Configuración' },
  { key: 'respaldos', label: 'Respaldos' },
];

function LockedBanner() {
  return (
    <div
      style="background:var(--amber-soft);border:1px solid var(--amber);border-radius:var(--radius-sm);padding:12px 16px;margin-bottom:18px;display:flex;align-items:center;gap:10px"
    >
      <span style="font-size:18px">🔒</span>
      <div>
        <strong style="color:var(--amber)">Evento iniciado — configuración bloqueada</strong>
        <div class="faint" style="font-size:13px;margin-top:2px">
          El menú, los precios y el inventario quedan fijos. Registrar asistentes y recargar saldo siguen disponibles durante el evento.
        </div>
      </div>
    </div>
  );
}

export function Admin() {
  const { params } = useRoute();
  const loc = useLocation();
  const tab = params.tab || 'eventos';

  // Re-fetch on every tab change so the lock state is current: after activating
  // the event in the Eventos tab and switching to Menú/Asistentes, this picks up
  // the now-active (locked) status instead of a stale mount-time value.
  const eventState = useAsync<EventItem | null>(
    () => Events.active().catch(() => null),
    [tab],
  );
  const activeEvent = eventState.data;
  const isLocked = activeEvent?.status === 'active' || activeEvent?.active === 1;

  // Only the menu/inventory configuration is frozen once the event starts.
  // Registering attendees and recharging balance are normal operation during
  // the event (people keep arriving), so the Asistentes tab is never locked.
  const configLocked = isLocked && tab === 'menu';

  return (
    <div>
      <div class="page-head"><h1>Administración</h1><p>Configuración y operación del sistema.</p></div>
      <div class="tabs">
        {TABS.map((t) => (
          <button key={t.key} class={`tab ${tab === t.key ? 'active' : ''}`}
            onClick={() => loc.route(`/admin/${t.key}`)}>{t.label}</button>
        ))}
      </div>

      {configLocked && <LockedBanner />}

      {tab === 'eventos' && <EventsTab />}
      {tab === 'menu' && <MenuTab locked={isLocked} />}
      {tab === 'asistentes' && <Attendees embedded locked={false} />}
      {tab === 'operadores' && <OperatorsTab />}
      {tab === 'propinas' && <TipsTab />}
      {tab === 'config' && <SettingsTab />}
      {tab === 'respaldos' && <BackupsTab />}
    </div>
  );
}

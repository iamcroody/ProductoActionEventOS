import { useEffect, useRef, useState } from 'preact/hooks';
import { Monitor as MonitorApi, openStream } from '../lib/api';
import { useAsync } from '../lib/hooks';
import { Loading, ErrorState, EmptyState } from '../components/ui';
import type { MonitorDevice, MonitorEvent, MonitorKind, MonitorStats } from '../lib/types';

const FEED_CAP = 150;

// Spanish integer grouping ("12.345"). per_min may be fractional → 1 decimal.
function fmt(n: number | null | undefined): string {
  return Math.round(Number(n ?? 0)).toLocaleString('es-CO');
}
function fmtRate(n: number | null | undefined): string {
  const v = Number(n ?? 0);
  return (Number.isInteger(v) ? v : Math.round(v * 10) / 10).toLocaleString('es-CO');
}

// "hace 3s" / "hace 4m" / "hace 2h". `now` is passed so the 1s tick re-renders.
function hace(ms: number, now: number): string {
  const s = Math.max(0, Math.floor((now - ms) / 1000));
  if (s < 60) return `hace ${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `hace ${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `hace ${h}h`;
  return `hace ${Math.floor(h / 24)}d`;
}

function clock(ms: number): string {
  const d = new Date(ms);
  const p = (x: number) => String(x).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

const KIND_PILL: Record<MonitorKind, string> = { host: 'blue', staff: 'green', cliente: 'amber' };
const KIND_LABEL: Record<MonitorKind, string> = { host: 'Host', staff: 'Staff', cliente: 'Cliente' };

function statusPill(status: number): string {
  const c = Math.floor(status / 100);
  if (c === 2) return 'green';
  if (c === 5) return 'red';
  if (c === 4) return 'amber';
  return 'muted'; // 3xx / other
}

type FeedRow = MonitorEvent & { _id: number };

export function Monitor() {
  // Single initial snapshot: all three sources at once → one Loading/Error path.
  const init = useAsync<[MonitorStats, MonitorDevice[], MonitorEvent[]]>(
    () => Promise.all([MonitorApi.stats(), MonitorApi.devices(), MonitorApi.traffic()]),
    [],
  );

  const [stats, setStats] = useState<MonitorStats | null>(null);
  const [devices, setDevices] = useState<MonitorDevice[]>([]);
  const [feed, setFeed] = useState<FeedRow[]>([]);
  const [connected, setConnected] = useState(false);
  const [now, setNow] = useState(Date.now());
  const idRef = useRef(0);

  // Seed local state from the snapshot (also re-seeds on manual retry).
  useEffect(() => {
    if (!init.data) return;
    const [s, d, t] = init.data;
    setStats(s);
    setDevices(d);
    setFeed(t.slice(0, FEED_CAP).map((e) => ({ ...e, _id: idRef.current++ })));
  }, [init.data]);

  // Live request feed via SSE. Each event prepends (newest on top) and bumps the
  // headline counters optimistically; the poll below reconciles to authoritative
  // values. EventSource reconnects on its own if the stream drops.
  useEffect(() => {
    const es = openStream('monitor', {
      req: (data) => {
        const ev = data as MonitorEvent;
        if (!ev || typeof ev.status !== 'number') return;
        setFeed((prev) => {
          const row: FeedRow = { ...ev, _id: idRef.current++ };
          return [row, ...prev].slice(0, FEED_CAP);
        });
        setStats((prev) => {
          if (!prev) return prev;
          const bucket = `${Math.floor(ev.status / 100)}xx` as '2xx' | '4xx' | '5xx';
          const by_status = { ...prev.by_status };
          if (bucket === '2xx' || bucket === '4xx' || bucket === '5xx') {
            by_status[bucket] = (by_status[bucket] ?? 0) + 1;
          }
          const by_kind = { ...prev.by_kind };
          by_kind[ev.kind] = (by_kind[ev.kind] ?? 0) + 1;
          return { ...prev, total: prev.total + 1, by_status, by_kind };
        });
      },
    });
    es.onopen = () => setConnected(true);
    es.onerror = () => setConnected(false);
    return () => es.close();
  }, []);

  // Quiet poll keeps counters + device list authoritative (per_min/active/devices
  // are server-computed) without the loading flicker a useAsync reload would cause.
  useEffect(() => {
    if (!init.data) return;
    let alive = true;
    const tick = async () => {
      try {
        const [s, d] = await Promise.all([MonitorApi.stats(), MonitorApi.devices()]);
        if (alive) { setStats(s); setDevices(d); }
      } catch { /* keep last good snapshot; stream/poll will recover */ }
    };
    const id = setInterval(tick, 3000);
    return () => { alive = false; clearInterval(id); };
  }, [init.data]);

  // 1s tick drives the "última actividad" relative-time labels.
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);

  if (init.loading && !stats) return <Loading label="Conectando a la red…" />;
  if (init.error && !stats) return <ErrorState message={init.error} onRetry={init.reload} />;
  if (!stats) return <EmptyState title="Sin datos" />;

  const active = stats.active ?? 0;
  const totalDevices = stats.devices ?? devices.length;
  const c4 = stats.by_status['4xx'] ?? 0;
  const c5 = stats.by_status['5xx'] ?? 0;
  const staff = stats.by_kind.staff ?? 0;
  const cliente = stats.by_kind.cliente ?? 0;

  return (
    <div>
      <div class="page-head spread">
        <div>
          <h1>Red en vivo</h1>
          <p>Todo el tráfico de la red del evento, en tiempo real.</p>
        </div>
        <div class={`live-badge ${connected ? 'on' : 'off'}`}>
          <span class="live-dot" />
          {connected ? 'EN VIVO' : 'Reconectando…'}
        </div>
      </div>

      <div class="hero-band">
        <div class="hero-kpi">
          <div class="h-label">Total solicitudes</div>
          <div class="h-value mono">{fmt(stats.total)}</div>
        </div>
        <div class="hero-kpi">
          <div class="h-label">Solicitudes / min</div>
          <div class="h-value mono" style="color:var(--accent-bright)">{fmtRate(stats.per_min)}</div>
        </div>
        <div class="hero-kpi">
          <div class="h-label">Dispositivos activos</div>
          <div class="h-value mono">
            <span style="color:var(--green)">{fmt(active)}</span>
            <span class="faint" style="font-size:0.5em;font-weight:600"> / {fmt(totalDevices)}</span>
          </div>
        </div>
        <div class="hero-kpi mon-chip-cell">
          <div class="h-label">Desglose</div>
          <div class="mon-chips">
            <span class="pill green">staff {fmt(staff)}</span>
            <span class="pill amber">clientes {fmt(cliente)}</span>
            <span class={`pill ${c4 > 0 ? 'amber' : 'muted'}`}>4xx {fmt(c4)}</span>
            <span class={`pill ${c5 > 0 ? 'red' : 'muted'}`}>5xx {fmt(c5)}</span>
          </div>
        </div>
      </div>

      <div class="mon-grid">
        <section class="card mon-panel">
          <div class="spread" style="margin-bottom:14px">
            <h3 style="margin:0">Dispositivos</h3>
            <span class="faint" style="font-size:12.5px">{fmt(devices.length)} en la red</span>
          </div>
          {devices.length === 0 ? (
            <EmptyState icon="◌" title="Sin dispositivos" hint="Esperando conexiones en la red del evento." />
          ) : (
            <div class="table-wrap mon-scroll">
              <table class="tbl">
                <thead>
                  <tr>
                    <th>IP</th>
                    <th>Tipo</th>
                    <th class="num">Solicitudes</th>
                    <th class="num">Última actividad</th>
                  </tr>
                </thead>
                <tbody>
                  {devices.map((d) => (
                    <tr key={d.ip}>
                      <td class="mono" style="font-weight:600">{d.ip}</td>
                      <td><span class={`pill ${KIND_PILL[d.kind]}`}>{KIND_LABEL[d.kind]}</span></td>
                      <td class="num mono">{fmt(d.count)}</td>
                      <td class="num mono faint">{hace(d.last_seen, now)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>

        <section class="card mon-panel">
          <div class="spread" style="margin-bottom:14px">
            <h3 style="margin:0">Tráfico en vivo</h3>
            <span class="faint" style="font-size:12.5px">{fmt(feed.length)} eventos</span>
          </div>
          {feed.length === 0 ? (
            <EmptyState icon="◦" title="Sin tráfico" hint="Las solicitudes aparecerán aquí en tiempo real." />
          ) : (
            <div class="feed mon-scroll">
              {feed.map((e) => (
                <div class="feed-row" key={e._id}>
                  <span class="feed-time mono faint">{clock(e.at)}</span>
                  <span class={`pill ${KIND_PILL[e.kind]} feed-kind`}>{KIND_LABEL[e.kind]}</span>
                  <span class="feed-req">
                    <span class="feed-method mono">{e.method}</span>
                    <span class="feed-path" title={e.path}>{e.path}</span>
                  </span>
                  <span class={`pill ${statusPill(e.status)} feed-status`}>{e.status}</span>
                  <span class="feed-dur mono faint">{fmt(e.dur_ms)} ms</span>
                </div>
              ))}
            </div>
          )}
        </section>
      </div>
    </div>
  );
}

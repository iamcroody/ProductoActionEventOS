import { useEffect, useState } from 'preact/hooks';
import { Pendientes as PendientesApi, openStream, ApiError } from '../lib/api';
import { relTime } from '../lib/format';
import { toast } from '../lib/toast';
import { Loading, ErrorState, EmptyState } from '../components/ui';
import type { PendienteOrder, PendienteItem } from '../lib/types';

export function Pendientes() {
  const [orders, setOrders] = useState<Map<string, PendienteOrder>>(new Map());
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [showHistory, setShowHistory] = useState(false);
  const [historyData, setHistoryData] = useState<PendienteOrder[] | null>(null);
  const [historyLoading, setHistoryLoading] = useState(false);
  const [, tick] = useState(0);

  const upsertOrder = (order: PendienteOrder) =>
    setOrders((prev) => {
      const next = new Map(prev);
      next.set(order.order_id, order);
      return next;
    });

  const applyUpdate = (u: { order_id: string; item_id: string; delivered_qty: number; qty: number }) =>
    setOrders((prev) => {
      const next = new Map(prev);
      const ord = next.get(u.order_id);
      if (!ord) return prev;
      const items = ord.items.map((it) =>
        it.id === u.item_id
          ? { ...it, delivered_qty: u.delivered_qty, pending: u.qty - u.delivered_qty }
          : it,
      );
      const allDone = items.every((it) => it.pending <= 0);
      if (allDone) {
        next.delete(u.order_id);
      } else {
        next.set(u.order_id, { ...ord, items });
      }
      return next;
    });

  const load = () => {
    setLoading(true);
    setError(null);
    PendientesApi.list()
      .then((list) => {
        const m = new Map<string, PendienteOrder>();
        for (const o of list) {
          if (o.items.some((it) => it.pending > 0)) {
            m.set(o.order_id, o);
          }
        }
        setOrders(m);
        setLoading(false);
      })
      .catch((e) => {
        setError(e instanceof ApiError ? e.message : 'Error al cargar pendientes');
        setLoading(false);
      });
  };

  useEffect(() => { load(); }, []);

  // SSE live updates
  useEffect(() => {
    const es = openStream('pendientes', {
      ticket: (data) => {
        const t = data as {
          order_id: string;
          table_label?: string;
          buyer?: string;
          operator?: string;
          items: { id: string; name: string; qty: number }[];
          created_at: number;
          source?: string;
        };
        const order: PendienteOrder = {
          order_id: t.order_id,
          table_label: t.table_label,
          buyer: t.buyer,
          source: (t.source as PendienteOrder['source']) ?? 'operator',
          created_at: t.created_at,
          items: t.items.map((it) => ({
            id: it.id,
            name: it.name,
            qty: it.qty,
            delivered_qty: 0,
            pending: it.qty,
          })),
        };
        upsertOrder(order);
      },
      ticket_update: (data) => {
        const u = data as { order_id: string; item_id: string; delivered_qty: number; qty: number };
        applyUpdate(u);
      },
    });
    es.onerror = () => { /* browser auto-reconnects */ };
    return () => es.close();
  }, []);

  // Refresh relative timestamps
  useEffect(() => {
    const t = setInterval(() => tick((n) => n + 1), 30000);
    return () => clearInterval(t);
  }, []);

  const entregar = async (itemId: string, orderId: string, all = false) => {
    // Optimistic update
    setOrders((prev) => {
      const next = new Map(prev);
      const ord = next.get(orderId);
      if (!ord) return prev;
      const items = ord.items.map((it) => {
        if (it.id !== itemId) return it;
        const newDelivered = all ? it.qty : Math.min(it.qty, it.delivered_qty + 1);
        return { ...it, delivered_qty: newDelivered, pending: it.qty - newDelivered };
      });
      const allDone = items.every((it) => it.pending <= 0);
      if (allDone) {
        next.delete(orderId);
      } else {
        next.set(orderId, { ...ord, items });
      }
      return next;
    });

    try {
      if (all) {
        await PendientesApi.entregarTodo(itemId);
      } else {
        await PendientesApi.entregar(itemId);
      }
    } catch (e) {
      toast.err(e instanceof ApiError ? e.message : 'No se pudo registrar la entrega');
      load(); // Restore real state on error
    }
  };

  const loadHistory = async () => {
    setHistoryLoading(true);
    try {
      const data = await PendientesApi.history();
      setHistoryData(data);
    } catch (e) {
      toast.err(e instanceof ApiError ? e.message : 'Error al cargar historial');
    } finally {
      setHistoryLoading(false);
    }
  };

  const toggleHistory = () => {
    const next = !showHistory;
    setShowHistory(next);
    if (next && historyData === null) loadHistory();
  };

  const sorted = [...orders.values()].sort((a, b) => a.created_at - b.created_at);

  return (
    <div>
      <div class="page-head spread">
        <div>
          <h1>Pendientes</h1>
          <p>Tablero en vivo · {sorted.length} pedido{sorted.length === 1 ? '' : 's'} pendiente{sorted.length === 1 ? '' : 's'}</p>
        </div>
        <div class="row" style="gap:8px">
          <button class="btn ghost sm" onClick={toggleHistory}>
            {showHistory ? '← Pendientes' : 'Historial'}
          </button>
          <button class="btn ghost sm" onClick={load}>↻ Refrescar</button>
        </div>
      </div>

      {!showHistory && (
        <>
          {loading && <Loading />}
          {error && <ErrorState message={error} onRetry={load} />}
          {!loading && !error && sorted.length === 0 && (
            <EmptyState icon="✓" title="Todo entregado" hint="No hay pedidos pendientes de entregar." />
          )}
          {!loading && !error && sorted.length > 0 && (
            <div class="board">
              {sorted.map((order) => (
                <OrderCard key={order.order_id} order={order} onEntregar={entregar} />
              ))}
            </div>
          )}
        </>
      )}

      {showHistory && (
        <div>
          {historyLoading && <Loading />}
          {!historyLoading && historyData !== null && historyData.length === 0 && (
            <EmptyState icon="◦" title="Sin historial" hint="Aún no hay pedidos entregados." />
          )}
          {!historyLoading && historyData && historyData.length > 0 && (
            <div class="board">
              {historyData.map((order) => (
                <OrderCard key={order.order_id} order={order} onEntregar={() => Promise.resolve()} readOnly />
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function OrderCard({
  order,
  onEntregar,
  readOnly = false,
}: {
  order: PendienteOrder;
  onEntregar: (itemId: string, orderId: string, all?: boolean) => Promise<void>;
  readOnly?: boolean;
}) {
  const isCliente = order.source === 'cliente';
  return (
    <div
      class="ticket"
      style={isCliente ? 'border-color:var(--amber);box-shadow:0 0 0 2px var(--amber-soft)' : undefined}
    >
      <div class="ticket-head">
        <div class="tbl">
          {order.buyer || 'Pedido'}
          {isCliente && <span class="pill amber" style="margin-left:8px;font-size:11px">Cliente</span>}
        </div>
        <div class="ago">{relTime(order.created_at)}</div>
      </div>

      <div class="ticket-meta">
        {order.buyer && <div><span class="faint">A nombre de:</span> <strong>{order.buyer}</strong></div>}
        {order.table_label && <div><span class="faint">Ubicación:</span> <strong>{order.table_label}</strong></div>}
      </div>

      <div class="ticket-items">
        {order.items.map((item) => (
          <ItemRow
            key={item.id}
            item={item}
            orderId={order.order_id}
            onEntregar={onEntregar}
            readOnly={readOnly}
          />
        ))}
      </div>
    </div>
  );
}

function ItemRow({
  item,
  orderId,
  onEntregar,
  readOnly,
}: {
  item: PendienteItem;
  orderId: string;
  onEntregar: (itemId: string, orderId: string, all?: boolean) => Promise<void>;
  readOnly: boolean;
}) {
  const [busy, setBusy] = useState(false);
  const done = item.pending <= 0;

  const act = async (all: boolean) => {
    setBusy(true);
    try {
      await onEntregar(item.id, orderId, all);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class={`pitem ${done ? 'done' : ''}`}>
      <div class="pitem-info">
        <div class="pitem-name">{item.name}</div>
        <div class="pitem-counts mono">
          <span>Ped. {item.qty}</span>
          <span style="color:var(--green)">Ent. {item.delivered_qty}</span>
          <span style={`font-weight:700;color:${done ? 'var(--green)' : 'var(--amber)'}`}>
            Falt. {item.pending}
          </span>
        </div>
      </div>
      {!readOnly && (
        <div class="pitem-act">
          {done ? (
            <span class="pill green" style="font-size:11px">Listo</span>
          ) : (
            <>
              <button class="btn sm" disabled={busy} onClick={() => act(false)}>
                Entregar
              </button>
              {item.pending > 1 && (
                <button class="btn primary sm" disabled={busy} onClick={() => act(true)}>
                  Todo
                </button>
              )}
            </>
          )}
        </div>
      )}
    </div>
  );
}

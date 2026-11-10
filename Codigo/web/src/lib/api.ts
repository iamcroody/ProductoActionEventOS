// Centralized fetch wrapper. Same-origin → cookie sent automatically.
// Surfaces backend {error} messages uniformly. 401 broadcasts an event so the
// app can redirect to login.

import type {
  Attendee, AuditEntry, AuditVerify, Backup, CajaSession, Category, ClientInfo, StaffInfo,
  ClosingReport, Config, DashboardReport, EventItem, IntegrityResult,
  InventoryMovement, KitchenItem, KitchenTicket, LedgerEntry, LowStockItem,
  MonitorDevice, MonitorEvent, MonitorStats, Operator, Order, PendienteOrder, Product, TipsReport, User,
} from './types';

export class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

type Query = Record<string, string | number | boolean | undefined | null>;

function qs(params?: Query): string {
  if (!params) return '';
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '') sp.set(k, String(v));
  }
  const s = sp.toString();
  return s ? `?${s}` : '';
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function request<T>(method: string, path: string, body?: unknown, attempt = 0): Promise<T> {
  let res: Response;
  try {
    res = await fetch(`/api${path}`, {
      method,
      headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
      body: body !== undefined ? JSON.stringify(body) : undefined,
      credentials: 'same-origin',
    });
  } catch {
    throw new ApiError('No hay conexión con el servidor', 0);
  }

  if (res.status === 401) {
    window.dispatchEvent(new CustomEvent('aeos:unauthorized'));
    throw new ApiError('Sesión expirada', 401);
  }

  let data: unknown = null;
  const text = await res.text();
  if (text) {
    try { data = JSON.parse(text); } catch { data = text; }
  }

  if (!res.ok) {
    const msg =
      data && typeof data === 'object' && 'error' in data
        ? String((data as { error: unknown }).error)
        : typeof data === 'string' && data
          ? data
          : `Error ${res.status}`;
    // Auto-retry the transient concurrency conflict (the OCC version guard
    // rejected a simultaneous write to the same attendee/product — nothing was
    // committed, so re-sending is safe and invisible to the user). NOT retried:
    // the event-lock 409 ("evento iniciado…"), permanent until the event closes.
    if (res.status === 409 && /concurrenc/i.test(msg) && attempt < 4) {
      await sleep(50 * (attempt + 1) + Math.floor(Math.random() * 60));
      return request<T>(method, path, body, attempt + 1);
    }
    throw new ApiError(msg, res.status);
  }
  return data as T;
}

export const api = {
  get: <T>(path: string, params?: Query) => request<T>('GET', path + qs(params)),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body ?? {}),
  put: <T>(path: string, body?: unknown) => request<T>('PUT', path, body ?? {}),
  del: <T>(path: string) => request<T>('DELETE', path),
};

// ---- Typed endpoints ----------------------------------------------------

export const Auth = {
  login: (username: string, secret: string) =>
    api.post<User>('/auth/login', { username, secret }),
  logout: () => api.post('/auth/logout'),
  me: () => api.get<User>('/auth/me'),
};

export const Health = {
  get: () => api.get<{ ok: boolean; product: string; signature: string }>('/health'),
};

export const ConfigApi = {
  get: () => api.get<Config>('/config'),
  update: (patch: Config) => api.put<Config>('/config', patch),
};

export const Events = {
  list: () => api.get<EventItem[]>('/events'),
  active: () => api.get<EventItem>('/events/active'),
  create: (b: Partial<EventItem>) => api.post<EventItem>('/events', b),
  update: (id: string, b: Partial<EventItem>) => api.put<EventItem>(`/events/${id}`, b),
  activate: (id: string) => api.post(`/events/${id}/activate`),
  close: (id: string) => api.post(`/events/${id}/close`),
};

export const Categories = {
  list: () => api.get<Category[]>('/categories'),
  create: (b: Partial<Category>) => api.post<Category>('/categories', b),
  update: (id: string, b: Partial<Category>) => api.put<Category>(`/categories/${id}`, b),
};

export const Products = {
  list: (params?: Query) => api.get<Product[]>('/products', params),
  create: (b: Partial<Product>) => api.post<Product>('/products', b),
  update: (id: string, b: Partial<Product>) => api.put<Product>(`/products/${id}`, b),
  remove: (id: string) => api.del(`/products/${id}`),
};

export const Inventory = {
  adjust: (b: { product_id: string; delta: number; reason: string }) =>
    api.post('/inventory/adjust', b),
  movements: (product_id?: string) =>
    api.get<InventoryMovement[]>('/inventory/movements', { product_id }),
  lowStock: (threshold = 5) => api.get<LowStockItem[]>('/inventory/low-stock', { threshold }),
};

export const Attendees = {
  search: (q?: string) => api.get<Attendee[]>('/attendees', { q }),
  get: (id: string) => api.get<Attendee>(`/attendees/${id}`),
  byToken: (token: string) => api.get<Attendee>(`/qr/${token}`),
  create: (b: { display_name: string; email?: string; doc_id?: string; phone?: string; initial_topup?: number; no_redeem?: boolean }) =>
    api.post<Attendee>('/attendees', b),
  import: (attendees: unknown[]) => api.post<{ imported: number }>('/attendees/import', { attendees }),
  ledger: (id: string) => api.get<LedgerEntry[]>(`/attendees/${id}/ledger`),
  topup: (id: string, amount: number) => api.post<Attendee>(`/attendees/${id}/topup`, { amount }),
  block: (id: string) => api.post(`/attendees/${id}/block`),
  unblock: (id: string) => api.post(`/attendees/${id}/unblock`),
};

export const Caja = {
  open: (opening_float: number) => api.post<CajaSession>('/caja/open', { opening_float }),
  current: () => api.get<CajaSession | null>('/caja/current'),
  close: (counted_cash: number) =>
    api.post<{ expected_cash: number; variance: number } & CajaSession>('/caja/close', { counted_cash }),
  sessions: () => api.get<CajaSession[]>('/caja/sessions'),
};

export interface OrderPayload {
  request_id: string;
  table_label?: string;
  items: { product_id: string; qty: number }[];
  payers: ({ attendee_id: string } | { qr_token: string })[];
  cash_confirmed: boolean;
}

export const Orders = {
  create: (b: OrderPayload) => api.post<Order>('/orders', b),
  list: (params?: Query) => api.get<Order[]>('/orders', params),
  get: (id: string) => api.get<Order>(`/orders/${id}`),
  void: (id: string) => api.post(`/orders/${id}/void`),
};

export const Kitchen = {
  board: (station: string) => api.get<KitchenTicket[] | KitchenItem[]>('/kitchen/board', { station }),
  preparing: (id: string) => api.post(`/kitchen/items/${id}/preparing`),
  ready: (id: string) => api.post(`/kitchen/items/${id}/ready`),
  delivered: (id: string) => api.post(`/kitchen/items/${id}/delivered`),
  history: (station: string, limit = 50) =>
    api.get<KitchenItem[]>('/kitchen/history', { station, limit }),
};

export const Pendientes = {
  list: () => api.get<PendienteOrder[]>('/pendientes'),
  history: () => api.get<PendienteOrder[]>('/pendientes/history'),
  entregar: (id: string, qty = 1) => api.post(`/pendientes/items/${id}/entregar`, { qty }),
  entregarTodo: (id: string) => api.post(`/pendientes/items/${id}/entregar-todo`),
};

export const Reports = {
  dashboard: () => api.get<DashboardReport>('/reports/dashboard'),
  tips: (pool: number) => api.get<TipsReport>('/reports/tips', { pool }),
  sales: (limit = 100) => api.get<Order[]>('/reports/sales', { limit }),
  closing: () => api.get<ClosingReport>('/reports/closing'),
  exportCsvUrl: (report: string) => `/api/reports/export.csv?report=${encodeURIComponent(report)}`,
  closingXlsxUrl: '/api/reports/closing.xlsx',
  closingCsvUrl: '/api/reports/closing.csv',
};

// Customer-facing web (separate port). QR/PNG endpoints are referenced directly
// as <img src> / downloads, not through the JSON fetch wrapper.
export const Client = {
  info: () => api.get<ClientInfo>('/client-info'),
  qrUrl: '/api/client-qr.png',
  attendeeQrUrl: (id: string) => `/api/attendees/${id}/qr.png`,
};

export const Staff = {
  info: () => api.get<StaffInfo>('/staff-info'),
  qrUrl: '/api/staff-qr.png',
};

export const Oversight = {
  audit: (params?: Query) => api.get<AuditEntry[]>('/audit', params),
  verify: () => api.get<AuditVerify>('/audit/verify'),
  integrity: () => api.get<IntegrityResult>('/integrity'),
  auditCsvUrl: () => '/api/audit/export.csv',
};

export const Users = {
  list: () => api.get<Operator[]>('/users'),
  create: (b: { username: string; full_name: string; role: string; pin: string; password?: string }) =>
    api.post<Operator>('/users', b),
  update: (id: string, b: { full_name: string; role: string; active: number }) =>
    api.put<Operator>(`/users/${id}`, b),
  setPin: (id: string, pin: string) => api.post(`/users/${id}/pin`, { pin }),
  remove: (id: string) => api.del(`/users/${id}`),
};

export const Monitor = {
  stats: () => api.get<MonitorStats>('/monitor/stats'),
  devices: () => api.get<MonitorDevice[]>('/monitor/devices'),
  traffic: () => api.get<MonitorEvent[]>('/monitor/traffic'),
};

export const Backups = {
  list: () => api.get<Backup[]>('/backups'),
  create: () => api.post<Backup>('/backups'),
  verify: (id: string) => api.post<{ valid: boolean }>('/backups/verify', { id }),
};

// ---- SSE ----------------------------------------------------------------

export function openStream(
  topic: string,
  handlers: { [event: string]: (data: unknown) => void },
): EventSource {
  const es = new EventSource(`/api/stream?topic=${encodeURIComponent(topic)}`);
  for (const [event, handler] of Object.entries(handlers)) {
    es.addEventListener(event, (e: MessageEvent) => {
      let parsed: unknown = e.data;
      try { parsed = JSON.parse(e.data); } catch { /* keep raw */ }
      handler(parsed);
    });
  }
  return es;
}

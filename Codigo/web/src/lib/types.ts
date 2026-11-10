// API entity types for Action EventOS.
// Money is integer pesos (COP). Timestamps are unix ms. Booleans are 1/0 ints.

export type Role = 'admin' | 'caja' | 'mesero' | 'cocina' | 'bar' | 'auditor';

export interface User {
  ID: string;
  Username: string;
  FullName: string;
  Role: Role;
}

export interface Operator {
  id: string;
  username: string;
  full_name: string;
  role: Role;
  active: number;
}

export interface Config {
  currency?: string;
  default_topup?: number;
  org_name?: string;
  [k: string]: unknown;
}

export interface EventItem {
  id: string;
  name: string;
  status?: string;
  starts_at?: number | null;
  ends_at?: number | null;
  default_topup?: number;
  split_policy?: string;
  active?: number;
}

export interface Category {
  id: string;
  name: string;
  sort_order?: number;
  active?: number;
}

export interface Product {
  id: string;
  category_id?: string | null;
  name: string;
  price: number;
  cost?: number;
  prep_station?: 'cocina' | 'bar' | 'none';
  track_stock?: number;
  stock_qty?: number;
  sort_order?: number;
  image_path?: string | null;
  active?: number;
}

export interface Attendee {
  id: string;
  display_name: string;
  email?: string | null;
  doc_id?: string | null;
  phone?: string | null;
  balance: number;
  qr_token?: string;
  blocked?: number;
}

export interface PendienteItem {
  id: string;
  name: string;
  qty: number;
  delivered_qty: number;
  pending: number;
}

export interface PendienteOrder {
  order_id: string;
  table_label?: string;
  buyer?: string;
  source: 'operator' | 'mesero' | 'cliente';
  created_at: number;
  items: PendienteItem[];
}

export interface LedgerEntry {
  id: string;
  attendee_id: string;
  kind: string;
  amount: number;
  balance_after?: number;
  note?: string;
  created_at: number;
}

export interface CajaSession {
  id: string;
  operator_id?: string;
  operator_name?: string;
  opening_float: number;
  counted_cash?: number | null;
  expected_cash?: number | null;
  variance?: number | null;
  status?: string;
  opened_at?: number;
  closed_at?: number | null;
}

export interface OrderItem {
  id: string;
  product_id: string;
  name?: string;
  qty: number;
  price?: number;
  status?: string;
  prep_station?: string;
}

export interface Order {
  id: string;
  total: number;
  paid_from_balance?: number;
  paid_cash?: number;
  status: string;
  table_label?: string;
  items: OrderItem[];
  created_at?: number;
  operator_name?: string;
  item_count?: number;
}

export interface KitchenItem {
  id: string;
  order_id: string;
  product_id?: string;
  product_name?: string;
  name?: string;
  qty: number;
  status: string;
  table_label?: string;
  station?: string;
  created_at?: number;
  attendee_name?: string;
}

export interface KitchenTicket {
  order_id: string;
  table_label?: string;
  attendee_name?: string;
  created_at?: number;
  items: KitchenItem[];
}

export interface InventoryMovement {
  id: string;
  product_id: string;
  product_name?: string;
  delta: number;
  reason: string;
  created_at: number;
}

export interface LowStockItem {
  id: string;
  name: string;
  stock_qty: number;
}

export interface DashboardReport {
  total_sold: number;
  total_cost: number;
  total_profit: number;
  profit_margin: number;
  order_count: number;
  avg_consumption: number;
  active_attendees: number;
  overage_cash: number;
  balance_from_sales: number;
  top_products: { name: string; qty_sold: number; revenue?: number; cost?: number; profit?: number }[];
  sales_by_hour: { hour: number | string; total: number; count?: number }[];
  sales_by_operator: { username: string; total: number; count?: number }[];
  low_stock: LowStockItem[];
}

export interface ClientInfo {
  client_url: string;
}

export interface StaffInfo {
  staff_url: string;
}

export interface ClosingReport {
  event: { id: string; name: string; status: string };
  totals: {
    revenue: number;
    cost: number;
    profit: number;
    margin: number;
    order_count: number;
    overage_cash: number;
    balance_from_sales: number;
  };
  attendees: {
    registered: number;
    active: number;
    avg_consumption: number;
    pending_balance: number;
  };
  timing: {
    avg_seconds_per_order: number;
    fastest_operator: { username: string; avg_seconds: number; order_count: number } | null;
  };
  operators: { username: string; total: number; count: number; avg_seconds: number }[];
  top_products: { name: string; qty_sold: number; revenue: number; cost: number; profit: number }[];
  inventory: {
    low_stock: { name: string; stock_qty: number }[];
    out_of_stock: { name: string }[];
  };
}

export interface TipShare {
  name: string;
  role: string;
  sales: number;
  top3: boolean;
  bonus: boolean;
  share: number;
}

export interface TipsReport {
  pool: number;
  bonus_pct: number;
  distributed: number;
  meseros: TipShare[];
  aseo: TipShare[];
}

export interface AuditEntry {
  id: string;
  action: string;
  at?: number;
  user_id?: string;
  operator_name?: string;
  attendee_id?: string;
  detail_json?: string;
  created_at?: number;
}

export interface AuditVerify {
  intact: boolean;
  broken_at?: string | null;
}

export interface IntegrityResult {
  ok: boolean;
  balance_mismatches: unknown[];
  stock_mismatches: unknown[];
  audit: AuditVerify;
}

export interface Backup {
  id: string;
  name?: string;
  created_at?: number;
  size?: number;
  valid?: boolean;
}

// ---- Live network monitor ("Red en vivo") ----
export type MonitorKind = 'host' | 'staff' | 'cliente';

export interface MonitorStats {
  total: number;
  per_min: number;
  devices: number;
  active: number;
  by_kind: { staff?: number; cliente?: number; host?: number };
  by_status: { '2xx'?: number; '4xx'?: number; '5xx'?: number };
}

export interface MonitorDevice {
  ip: string;
  kind: MonitorKind;
  count: number;
  first_seen: number;
  last_seen: number;
}

export interface MonitorEvent {
  at: number;
  ip: string;
  method: string;
  path: string;
  status: number;
  dur_ms: number;
  kind: MonitorKind;
}

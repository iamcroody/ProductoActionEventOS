# Action EventOS — API Reference

Action EventOS runs **two isolated HTTP servers**:

- **Management (staff)** — `http://<LAN-IP>:8080` (flag `-port`). Serves the
  console SPA and the full operator API documented below. Any LAN device must
  present the access key once (`?k=<key>`, supplied by the private staff QR); the
  host machine (loopback) is always allowed. Without the key every path returns a
  plain **404** — the surface is invisible, not just unauthorized.
- **Customer (QR)** — `http://<LAN-IP>:8090` (flag `-client-port`). Serves **only**
  the customer web page and `/api/cliente/*`. It never exposes the operator API or
  the console, so a customer with the QR URL cannot reach management.

All responses are JSON. Operator auth is the `aeos_session` cookie set by login;
the management gate adds an `aeos_gate` cookie. Money is **integer pesos (COP)**.
Timestamps are **unix milliseconds (UTC)**. Booleans for `active`/`track_stock`
are **integers (1/0)**.

Roles: `admin`, `caja`, `mesero`, `cocina`, `bar`, `auditor`. `admin` passes every
role check implicitly.

## Auth
| Method | Path | Roles | Body |
|---|---|---|---|
| POST | `/api/auth/login` | — | `{username, secret}` → user + cookie |
| POST | `/api/auth/logout` | any | — |
| GET | `/api/auth/me` | any | → `{ID, Username, FullName, Role}` |

## Health / Config / Backups / Stream
| Method | Path | Roles | Notes |
|---|---|---|---|
| GET | `/api/health` | — | `{ok, product, signature}` |
| GET | `/api/config` | any | `{currency, default_topup, org_name}` |
| PUT | `/api/config` | admin | `{key: value, …}` |
| GET | `/api/backups` | admin | list |
| POST | `/api/backups` | admin | create manual backup |
| POST | `/api/backups/verify` | admin | `{id}` → `{valid}` |
| GET | `/api/stream?topic=pendientes\|monitor\|*` | any | Server-Sent Events |

## Users (operators) — admin only
`GET /api/users` · `POST /api/users {username, full_name, role, pin, password?}` ·
`PUT /api/users/{id} {full_name, role, active}` ·
`POST /api/users/{id}/pin {pin}` · `DELETE /api/users/{id}`

## Events
`GET /api/events` · `GET /api/events/active` ·
`POST /api/events (admin) {name, starts_at?, ends_at?, default_topup, split_policy}` ·
`PUT /api/events/{id} (admin)` · `POST /api/events/{id}/activate (admin)` ·
`POST /api/events/{id}/close (admin)` (triggers auto-backup)

## Categories / Products / Inventory
`GET /api/categories` · `POST /api/categories (admin) {name, sort_order}` ·
`PUT /api/categories/{id} (admin) {name, sort_order, active}`

`GET /api/products?category=&active=` ·
`POST /api/products (admin) {category_id?, name, price, prep_station(cocina|bar|none), track_stock(1|0), stock_qty, sort_order?, image_path?}` ·
`PUT /api/products/{id} (admin)` · `DELETE /api/products/{id} (admin)` (soft delete)

`POST /api/inventory/adjust (admin,caja) {product_id, delta, reason(restock|waste|adjustment)}` ·
`GET /api/inventory/movements?product_id=` ·
`GET /api/inventory/low-stock?threshold=5`

## Attendees (customers) + Saldo
`POST /api/attendees (admin,caja) {display_name, doc_id?, email?, phone?, no_redeem?, initial_topup?}` → includes `qr_token`.
`doc_id`, `email` and `phone` are the identity fields the customer validates against when ordering by QR; `no_redeem:true` registers an attendee who pays only in cash (never holds redeemable balance) ·
`POST /api/attendees/import (admin,caja) {attendees:[…]}` ·
`GET /api/attendees?q=` (instant search by name/doc) ·
`GET /api/attendees/{id}` · `GET /api/qr/{token}` (resolve a scanned QR token) ·
`GET /api/attendees/{id}/qr.png` (printable per-attendee QR image) ·
`GET /api/attendees/{id}/ledger` ·
`POST /api/attendees/{id}/topup (admin,caja) {amount}` ·
`POST /api/attendees/{id}/block (admin)` · `POST /api/attendees/{id}/unblock (admin)`

## Caja — admin,caja
`POST /api/caja/open {opening_float}` · `GET /api/caja/current` ·
`POST /api/caja/close {counted_cash}` → `{expected_cash, variance}` ·
`GET /api/caja/sessions`

## Orders (Ventas / Consumos)
`POST /api/orders (admin,caja,mesero)`:
```json
{
  "request_id": "client-uuid",
  "table_label": "Mesa 4",
  "items": [{"product_id":"…","qty":2}],
  "payers": [{"attendee_id":"…"}],   // or {"qr_token":"…"}; >1 payer = split
  "cash_confirmed": false             // required true if balance < total
}
```
→ `{id, total, paid_from_balance, paid_cash, status, items, …}`.
`GET /api/orders?status=&limit=` · `GET /api/orders/{id}` ·
`POST /api/orders/{id}/void (admin)` (reverses stock + balance)

## Pendientes (entregas) — cocina,bar,caja,admin
Each sold item is a delivery to dispatch from storage, with partial delivery
(pedido / entregado / faltante). Customer-QR orders arrive flagged for the floor.
`GET /api/pendientes?station=cocina|bar` ·
`GET /api/pendientes/history?station=&limit=` ·
`POST /api/pendientes/items/{id}/entregar` (deliver one unit) ·
`POST /api/pendientes/items/{id}/entregar-todo` (deliver the remaining units)
SSE events on topic `pendientes`: `ticket` (new order), `ticket_update` (delivery change).

## Cliente (customer QR) — served only on the customer port (`:8090`)
Session is an isolated HMAC cookie; these routes never touch management.
`POST /api/cliente/acceso {nombre, correo, cedula, celular}` (also accepts `qr_token`) — validates identity against the attendee record; mismatch → 403 ·
`POST /api/cliente/salir` (end session) ·
`GET /api/cliente/yo` → attendee name, balance, redeemable flag ·
`GET /api/cliente/menu` → event menu ·
`POST /api/cliente/pedido` (place an order; balance is debited, cannot exceed it)

## Monitor (Red en vivo) — admin only
Live view of all LAN traffic across both servers, published on SSE topic `monitor`.
`GET /api/monitor/traffic` (recent request feed) ·
`GET /api/monitor/devices` (devices seen on the network) ·
`GET /api/monitor/stats` (counters)

## QR images
`GET /api/client-qr.png` (any) · `GET /api/staff-qr.png` (admin) — printable QR
images for the public customer URL and the keyed private staff URL, mirroring the
terminal banner and the in-app *Accesos* screen.

## Reports — admin,caja,auditor
`GET /api/reports/dashboard` → total_sold, order_count, avg_consumption,
registered_attendees, active_attendees, overage_cash, balance_from_sales,
pending_balance, top_products[], sales_by_hour[], sales_by_operator[], low_stock[].
`GET /api/reports/sales?limit=` ·
`GET /api/reports/tips` (propinas distribution: top-3 meseros + aseo +5%) ·
`GET /api/reports/export.csv?report=sales|products|attendees|ledger` ·
`GET /api/reports/export.json?report=…`

### Reporte de cierre (end-of-event business report) — admin only
Consolidated snapshot: revenue, cost, profit, margin, attendees, timing,
operators, top products and inventory.
`GET /api/reports/closing` (JSON) ·
`GET /api/reports/closing.xlsx` (multi-sheet Excel workbook) ·
`GET /api/reports/closing.csv` (sectioned CSV)

## Oversight — admin,auditor
`GET /api/audit?action=&user_id=&attendee_id=&limit=` ·
`GET /api/audit/verify` → `{intact, broken_at}` ·
`GET /api/integrity` → `{ok, balance_mismatches[], stock_mismatches[], audit{}}` ·
`GET /api/audit/export.csv`

## Default first-run credentials
Operator `action` / password `croody` (seeded on first launch). The login screen
does **not** display these; change them in Administración → Operadores.

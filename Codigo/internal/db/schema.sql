-- EventPOS schema. Money is INTEGER pesos (COP). Timestamps are INTEGER unix-millis UTC.
-- IDs are UUIDv7 TEXT. Tables marked IMMUTABLE are append-only (enforced by triggers below).
-- This file is embedded in the binary and applied at startup if schema_version is behind.

-- ---------------------------------------------------------------------------
-- Meta
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS schema_meta (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    version       INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS app_config (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    updated_at  INTEGER NOT NULL,
    updated_by  TEXT
);

-- ---------------------------------------------------------------------------
-- Identity & event context
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS events (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    starts_at      INTEGER,
    ends_at        INTEGER,
    status         TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','active','closed')),
    default_topup  INTEGER NOT NULL DEFAULT 50000,
    split_policy   TEXT NOT NULL DEFAULT 'cascade' CHECK (split_policy IN ('cascade','strict_equal')),
    created_at     INTEGER NOT NULL
);
-- At most one active event at a time.
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_one_active ON events(status) WHERE status = 'active';

CREATE TABLE IF NOT EXISTS users (
    id             TEXT PRIMARY KEY,
    username       TEXT NOT NULL UNIQUE,
    full_name      TEXT,
    role           TEXT NOT NULL CHECK (role IN ('admin','caja','mesero','cocina','bar','auditor')),
    pin_hash       TEXT NOT NULL,        -- argon2id of the operator PIN
    password_hash  TEXT,                 -- argon2id; admins only (optional stronger login)
    active         INTEGER NOT NULL DEFAULT 1,
    created_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_users_username ON users(username);

CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,        -- sha256 hash of the raw token (raw never stored)
    user_id     TEXT NOT NULL REFERENCES users(id),
    ip          TEXT,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    revoked_at  INTEGER
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

-- ---------------------------------------------------------------------------
-- Customers & catalog
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS attendees (
    id               TEXT PRIMARY KEY,
    event_id         TEXT NOT NULL REFERENCES events(id),
    qr_token         TEXT NOT NULL UNIQUE,            -- scannable identity
    display_name     TEXT NOT NULL,
    doc_id           TEXT,                            -- cédula (used for client identity match)
    email            TEXT,                            -- correo (used for client identity match)
    phone            TEXT,                            -- celular (used for client identity match)
    no_redeem        INTEGER NOT NULL DEFAULT 0,      -- 1 = sin dinero redimible (no recibe saldo)
    balance          INTEGER NOT NULL DEFAULT 0 CHECK (balance >= 0),  -- cached; ledger is source of truth
    balance_version  INTEGER NOT NULL DEFAULT 0,
    status           TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','blocked')),
    created_at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_attendees_event ON attendees(event_id);
CREATE INDEX IF NOT EXISTS idx_attendees_name ON attendees(event_id, display_name);
CREATE INDEX IF NOT EXISTS idx_attendees_doc ON attendees(event_id, doc_id);

CREATE TABLE IF NOT EXISTS categories (
    id          TEXT PRIMARY KEY,
    event_id    TEXT NOT NULL REFERENCES events(id),
    name        TEXT NOT NULL,
    sort_order  INTEGER NOT NULL DEFAULT 0,
    active      INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_categories_event ON categories(event_id);

CREATE TABLE IF NOT EXISTS products (
    id             TEXT PRIMARY KEY,
    event_id       TEXT NOT NULL REFERENCES events(id),
    category_id    TEXT REFERENCES categories(id),
    name           TEXT NOT NULL,
    price          INTEGER NOT NULL CHECK (price >= 0),
    cost           INTEGER NOT NULL DEFAULT 0 CHECK (cost >= 0),  -- valor al por mayor (costo unitario)
    prep_station   TEXT NOT NULL DEFAULT 'none' CHECK (prep_station IN ('cocina','bar','none')),
    track_stock    INTEGER NOT NULL DEFAULT 1,
    stock_qty      INTEGER NOT NULL DEFAULT 0 CHECK (stock_qty >= 0),  -- cached; movements are source of truth
    stock_version  INTEGER NOT NULL DEFAULT 0,
    sort_order     INTEGER NOT NULL DEFAULT 0,
    image_path     TEXT,
    active         INTEGER NOT NULL DEFAULT 1,
    created_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_products_event ON products(event_id);
CREATE INDEX IF NOT EXISTS idx_products_category ON products(category_id);

-- ---------------------------------------------------------------------------
-- Inventory (IMMUTABLE source of truth)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS stock_movements (
    id            TEXT PRIMARY KEY,
    product_id    TEXT NOT NULL REFERENCES products(id),
    delta         INTEGER NOT NULL,
    reason        TEXT NOT NULL CHECK (reason IN ('initial','restock','sale','waste','adjustment','void')),
    ref_order_id  TEXT,
    balance_after INTEGER NOT NULL,
    user_id       TEXT NOT NULL,
    created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_stockmov_product_time ON stock_movements(product_id, created_at);
CREATE INDEX IF NOT EXISTS idx_stockmov_order ON stock_movements(ref_order_id);

-- ---------------------------------------------------------------------------
-- Sales
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS cash_sessions (
    id             TEXT PRIMARY KEY,
    event_id       TEXT NOT NULL REFERENCES events(id),
    operator_id    TEXT NOT NULL REFERENCES users(id),
    opening_float  INTEGER NOT NULL DEFAULT 0,
    opened_at      INTEGER NOT NULL,
    closed_at      INTEGER,
    expected_cash  INTEGER,
    counted_cash   INTEGER,
    variance       INTEGER,
    status         TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','closed'))
);
-- One open session per operator.
CREATE UNIQUE INDEX IF NOT EXISTS idx_cash_one_open ON cash_sessions(operator_id) WHERE status = 'open';
CREATE INDEX IF NOT EXISTS idx_cash_event ON cash_sessions(event_id);

CREATE TABLE IF NOT EXISTS orders (
    id                 TEXT PRIMARY KEY,
    event_id           TEXT NOT NULL REFERENCES events(id),
    session_id         TEXT REFERENCES cash_sessions(id),
    operator_id        TEXT NOT NULL REFERENCES users(id),
    request_id         TEXT UNIQUE,        -- idempotency key from client
    source             TEXT NOT NULL DEFAULT 'operator',  -- 'operator' | 'mesero' | 'cliente' (QR self-order)
    status             TEXT NOT NULL CHECK (status IN ('open','paid','preparing','ready','delivered','voided')),
    table_label        TEXT,
    subtotal           INTEGER NOT NULL DEFAULT 0,
    total              INTEGER NOT NULL DEFAULT 0,
    paid_from_balance  INTEGER NOT NULL DEFAULT 0,
    paid_cash          INTEGER NOT NULL DEFAULT 0,
    is_split           INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    paid_at            INTEGER,
    voided_at          INTEGER
);
CREATE INDEX IF NOT EXISTS idx_orders_event_status ON orders(event_id, status);
CREATE INDEX IF NOT EXISTS idx_orders_session ON orders(session_id);
CREATE INDEX IF NOT EXISTS idx_orders_created ON orders(created_at);

CREATE TABLE IF NOT EXISTS order_items (
    id            TEXT PRIMARY KEY,
    order_id      TEXT NOT NULL REFERENCES orders(id),
    product_id    TEXT NOT NULL REFERENCES products(id),
    name          TEXT NOT NULL,          -- snapshot
    qty           INTEGER NOT NULL CHECK (qty > 0),
    delivered_qty INTEGER NOT NULL DEFAULT 0,  -- units handed over (partial delivery)
    unit_price    INTEGER NOT NULL,       -- snapshot at sale time
    unit_cost     INTEGER NOT NULL DEFAULT 0,  -- cost snapshot at sale time (for profit)
    line_total    INTEGER NOT NULL,
    prep_station  TEXT NOT NULL DEFAULT 'none',
    prep_status   TEXT NOT NULL DEFAULT 'queued' CHECK (prep_status IN ('queued','preparing','ready','delivered')),
    ready_at      INTEGER,
    delivered_at  INTEGER
);
CREATE INDEX IF NOT EXISTS idx_items_order ON order_items(order_id);
CREATE INDEX IF NOT EXISTS idx_items_station_status ON order_items(prep_station, prep_status);

-- IMMUTABLE
CREATE TABLE IF NOT EXISTS payments (
    id           TEXT PRIMARY KEY,
    order_id     TEXT NOT NULL REFERENCES orders(id),
    attendee_id  TEXT REFERENCES attendees(id),
    method       TEXT NOT NULL CHECK (method IN ('balance','cash')),
    amount       INTEGER NOT NULL CHECK (amount > 0),
    created_at   INTEGER NOT NULL,
    user_id      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_payments_order ON payments(order_id);
CREATE INDEX IF NOT EXISTS idx_payments_attendee ON payments(attendee_id);

CREATE TABLE IF NOT EXISTS order_splits (
    id                TEXT PRIMARY KEY,
    order_id          TEXT NOT NULL REFERENCES orders(id),
    attendee_id       TEXT NOT NULL REFERENCES attendees(id),
    share_amount      INTEGER NOT NULL,
    paid_from_balance INTEGER NOT NULL DEFAULT 0,
    cash_collected    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_splits_order ON order_splits(order_id);

-- ---------------------------------------------------------------------------
-- Balance ledger (IMMUTABLE, source of truth for money)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS balance_ledger (
    id              TEXT PRIMARY KEY,
    attendee_id     TEXT NOT NULL REFERENCES attendees(id),
    type            TEXT NOT NULL CHECK (type IN ('topup','debit','refund','adjustment','transfer_in','transfer_out')),
    amount          INTEGER NOT NULL,                       -- signed: +credit, -debit
    balance_after   INTEGER NOT NULL CHECK (balance_after >= 0),
    ref_order_id    TEXT,
    ref_payment_id  TEXT UNIQUE,                            -- a payment can be ledgered exactly once
    prev_ledger_id  TEXT,                                   -- hash chain
    chain_hash      TEXT,
    user_id         TEXT NOT NULL,
    created_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ledger_attendee_time ON balance_ledger(attendee_id, created_at);

-- ---------------------------------------------------------------------------
-- Audit log (IMMUTABLE)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS audit_log (
    id             TEXT PRIMARY KEY,
    event_id       TEXT,
    at             INTEGER NOT NULL,
    user_id        TEXT,
    attendee_id    TEXT,
    ip             TEXT,
    action         TEXT NOT NULL,
    entity         TEXT,
    entity_id      TEXT,
    products_json  TEXT,
    balance_before INTEGER,
    balance_after  INTEGER,
    detail_json    TEXT,
    prev_audit_id  TEXT,
    chain_hash     TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_log(at);
-- Composite (at, id) so the chain-head lookup "ORDER BY at DESC, id DESC LIMIT 1"
-- in WriteAudit is served by an index walk instead of a full-table sort on every
-- append (keeps audit writes O(log n) at large events).
CREATE INDEX IF NOT EXISTS idx_audit_at_id ON audit_log(at, id);
CREATE INDEX IF NOT EXISTS idx_audit_user ON audit_log(user_id);
CREATE INDEX IF NOT EXISTS idx_audit_attendee ON audit_log(attendee_id);
CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_log(action);

-- ---------------------------------------------------------------------------
-- Backups (metadata)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS backups (
    id          TEXT PRIMARY KEY,
    path        TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    size_bytes  INTEGER NOT NULL,
    sha256      TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('manual','auto','wal_checkpoint')),
    status      TEXT NOT NULL DEFAULT 'ok',
    event_id    TEXT
);

-- ---------------------------------------------------------------------------
-- Immutability triggers (append-only tables)
-- ---------------------------------------------------------------------------
CREATE TRIGGER IF NOT EXISTS trg_stockmov_noupdate BEFORE UPDATE ON stock_movements
BEGIN SELECT RAISE(ABORT, 'stock_movements is immutable'); END;
CREATE TRIGGER IF NOT EXISTS trg_stockmov_nodelete BEFORE DELETE ON stock_movements
BEGIN SELECT RAISE(ABORT, 'stock_movements is immutable'); END;

CREATE TRIGGER IF NOT EXISTS trg_payments_noupdate BEFORE UPDATE ON payments
BEGIN SELECT RAISE(ABORT, 'payments is immutable'); END;
CREATE TRIGGER IF NOT EXISTS trg_payments_nodelete BEFORE DELETE ON payments
BEGIN SELECT RAISE(ABORT, 'payments is immutable'); END;

CREATE TRIGGER IF NOT EXISTS trg_ledger_noupdate BEFORE UPDATE ON balance_ledger
BEGIN SELECT RAISE(ABORT, 'balance_ledger is immutable'); END;
CREATE TRIGGER IF NOT EXISTS trg_ledger_nodelete BEFORE DELETE ON balance_ledger
BEGIN SELECT RAISE(ABORT, 'balance_ledger is immutable'); END;

CREATE TRIGGER IF NOT EXISTS trg_audit_noupdate BEFORE UPDATE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is immutable'); END;
CREATE TRIGGER IF NOT EXISTS trg_audit_nodelete BEFORE DELETE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is immutable'); END;

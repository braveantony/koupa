CREATE TABLE menu_items (
    id        integer PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    name      text    NOT NULL,
    price     integer NOT NULL CHECK (price >= 0),
    available boolean NOT NULL DEFAULT true
);

-- Customer-facing order number: 1..9999, wraps to 1 (shown as 0001..9999).
CREATE SEQUENCE order_number_seq MINVALUE 1 MAXVALUE 9999 CYCLE;

-- Pickup-call sequence: strictly increasing, never wraps.
CREATE SEQUENCE ready_seq_seq START 1 CACHE 1 NO CYCLE;

CREATE TABLE orders (
    id            bigint      PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    number        integer     NOT NULL,
    status        text        NOT NULL DEFAULT 'preparing' CHECK (status IN ('preparing', 'ready')),
    total         integer     NOT NULL CHECK (total >= 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    ready_seq     bigint,
    ready_at      timestamptz,
    ready_attempt integer,
    client_ref    text
);

-- Deliberately NOT unique: steps 0-3 must be able to leave duplicate orders.
CREATE INDEX orders_client_ref_idx ON orders (client_ref);
CREATE INDEX orders_status_idx ON orders (status);

CREATE TABLE order_items (
    order_id     bigint  NOT NULL REFERENCES orders (id),
    menu_item_id integer NOT NULL REFERENCES menu_items (id),
    quantity     integer NOT NULL CHECK (quantity > 0),
    unit_price   integer NOT NULL CHECK (unit_price >= 0)
);

CREATE INDEX order_items_order_id_idx ON order_items (order_id);

CREATE TABLE idempotency_keys (
    client_ref   text        PRIMARY KEY,
    order_id     bigint      REFERENCES orders (id),
    request_hash text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Supports the cleanup: order_id IS NOT NULL AND created_at < now() - interval '25 hours'.
CREATE INDEX idempotency_keys_created_at_idx ON idempotency_keys (created_at);

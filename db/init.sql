-- ============ The real (fixed) design ============
CREATE TABLE inventory (
  product_id INT PRIMARY KEY,
  name       TEXT NOT NULL,
  stock      INT  NOT NULL CHECK (stock >= 0)   -- database itself refuses negative stock
);

CREATE TABLE orders (
  order_id   TEXT PRIMARY KEY,                   -- same message twice -> processed once
  user_id    TEXT NOT NULL,
  product_id INT  NOT NULL REFERENCES inventory(product_id),
  status     TEXT NOT NULL DEFAULT 'CONFIRMED',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (user_id, product_id)                   -- one phone per user
);

INSERT INTO inventory VALUES (1, 'Flash Phone X', 500);

-- ============ The original BROKEN design (for the before/after demo) ============
-- No CHECK constraint, no conditional update: this is how 540 orders happened.
CREATE TABLE naive_inventory (product_id INT PRIMARY KEY, stock INT NOT NULL);
CREATE TABLE naive_orders (id BIGSERIAL PRIMARY KEY, user_id TEXT NOT NULL, created_at TIMESTAMPTZ DEFAULT now());
INSERT INTO naive_inventory VALUES (1, 500);

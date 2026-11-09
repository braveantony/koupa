-- ready_seq is drawn from ready_seq_seq and never reused; enforce it.
CREATE UNIQUE INDEX orders_ready_seq_key ON orders (ready_seq) WHERE ready_seq IS NOT NULL;

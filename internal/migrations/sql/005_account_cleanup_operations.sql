CREATE TABLE account_cleanup_operations (
  idempotency_key text PRIMARY KEY,
  subject_ref text NOT NULL,
  result jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

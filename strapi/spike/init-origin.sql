-- Runs once at origin cluster creation.
-- Mirrors openFGA/scripts/init-origin.sql.

-- Corroborating evidence for R1b: origin-side `calls` deltas tell us whether a
-- statement actually reached the origin or was answered by PgCache.
-- STUDY §5 ("Origin pg_stat_statements") pins track=all / max=10000.
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- PgCache needs REPLICATION to open its logical slot (STUDY risk R1c).
-- POSTGRES_USER is already a superuser here; granted explicitly anyway so the
-- spike's origin matches the lab's origin exactly.
ALTER ROLE spike REPLICATION;

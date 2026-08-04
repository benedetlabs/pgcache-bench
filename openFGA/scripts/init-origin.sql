-- Executado uma unica vez na criacao do cluster de origem.
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- PgCache precisa de REPLICATION para abrir o slot logico.
-- O usuario `fga` e' o dono do banco; damos REPLICATION explicitamente.
ALTER ROLE fga REPLICATION;

-- Helper: reset das estatisticas entre runs (chamado por scripts/run.sh).
CREATE OR REPLACE FUNCTION bench_reset_stats() RETURNS void LANGUAGE sql AS $$
  SELECT pg_stat_statements_reset();
  SELECT pg_stat_reset();
$$;

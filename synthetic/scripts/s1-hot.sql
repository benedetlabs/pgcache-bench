-- S1 — bounded hot set. The keyspace, not the query, is the variable.
--
-- pgbench's own select-only builtin draws aid uniformly from the full table:
--     \set aid random(1, 100000 * :scale)
-- Under -M simple that literal is interpolated into the SQL text, so every draw
-- is a distinct cache key. Measured at scale 10: 169 hits in 121,545 queries,
-- a 0.14% hit ratio. That is not a cache result, it is a keyspace result -- the
-- workload never repeats itself, so nothing could cache it.
--
-- :hot bounds the draw. Set it from the command line with -D hot=NNNN to sweep
-- the hit ratio directly, which is the thing no real subject let us control:
--   -D hot=1000     ~100% hit, the cache's best case
--   -D hot=100000   partial residency
--   -D hot=1000000  equivalent to the builtin, ~0% hit
--
-- Both paths run the identical script. The only difference is -h.
\set aid random(1, :hot)
SELECT abalance FROM pgbench_accounts WHERE aid = :aid;

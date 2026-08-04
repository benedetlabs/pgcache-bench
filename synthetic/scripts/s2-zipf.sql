-- S2 — Zipfian access over the whole table.
--
-- S1's hard cutoff gives a cache either everything or nothing: inside the hot
-- set the hit ratio saturates, outside it is zero. Real access patterns have a
-- tail, and the tail is where a cache's eviction policy actually gets tested.
--
-- random_zipfian(lb, ub, s) is a pgbench builtin, so this needs no external
-- generator. Larger s concentrates the distribution:
--   -D s=1.05   nearly flat, most draws miss
--   -D s=1.2    moderate skew
--   -D s=1.5    sharp, a few thousand keys carry most of the traffic
--
-- Sweeping s produces the hit-ratio-versus-gain curve as a continuous function.
-- No real subject in this platform allowed that: on openFGA and NetBox the hit
-- ratio was whatever the application happened to produce (90% and 40%), and it
-- could not be moved to see what it was worth.
\set aid random_zipfian(1, 100000 * :scale, :s)
SELECT abalance FROM pgbench_accounts WHERE aid = :aid;

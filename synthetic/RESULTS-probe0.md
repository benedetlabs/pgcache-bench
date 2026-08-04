# pgbench — Probe 0

**Ran:** 2026-08-04, local Docker stack ([docker-compose.yml](docker-compose.yml)),
pgbench scale 10 (1,000,000 rows, `pgbench_accounts` 150 MB), origin
`shared_buffers=1GB`, PostgreSQL 17.10, PgCache stock (`ALLOWED_TABLES` set, no
`PINNED_TABLES`).

**Verdict: pgbench passes C9 and is adopted.** It is also the first subject in
this platform on which PgCache has beaten the origin.

Read the caveats before quoting any number: this ran on Docker Desktop on macOS,
where the origin measured **77 µs** per point-select against the **~40 µs**
recorded on AKS nodes. The *shape* of these results transfers. The margins do
not.

---

## Method

Both paths run the identical pgbench binary, the identical script, from the
identical container, against the identical data. They differ in `-h` and `-p`
and nothing else. The client runs **inside the compose network**, not on the
host — a host-to-container hop on Docker Desktop costs more than the effect
being measured.

`-M simple` throughout, which is pgbench's own default. Verified with
`--show-script=select-only`:

```
\set aid random(1, 100000 * :scale)
SELECT abalance FROM pgbench_accounts WHERE aid = :aid;
```

No `BEGIN`, and the literal is interpolated into the SQL text before it is sent.
Path B is therefore "path A plus a cache" with nothing else moved — the confound
that made openFGA's r5 result retractable does not exist here.

PgCache was restarted before each path-B cell and given one full warm-up pass at
the same parameters, then measured on the second pass.

---

## 1. The keyspace result, which comes before every other result

pgbench's own `select-only` builtin, unmodified, 20 s, 1 client:

| path | latency | tps | cache |
|---|---:|---:|---|
| A origin | 80 µs | 12,476 | — |
| B PgCache | 164 µs | 6,079 | 169 hits / 121,376 misses — **0.14%** |

PgCache is 2.05× slower. That is not a caching result, it is a **keyspace**
result: `random(1, 1000000)` under simple protocol produces a distinct SQL text
per draw, so the workload never repeats itself and nothing could cache it.

This is worth stating loudly because it is the default configuration of the most
widely used PostgreSQL benchmark in existence. Anyone benchmarking a read cache
with stock `pgbench -S` measures pure proxy overhead and concludes the cache is
harmful.

---

## 2. Hot-set size — the sign change

[`s1-hot.sql`](scripts/s1-hot.sql), 15 s, 1 client.

| hot set | path | latency | tps | hit ratio |
|---|---|---:|---:|---:|
| 1,000 keys | A origin | 77 µs | 12,980 | — |
| | **B PgCache** | **62 µs** | **16,026** | 95.7% |
| 100,000 keys | A origin | 80 µs | 12,546 | — |
| | B PgCache | 174 µs | 5,733 | 2.9% |

Same query, same data, same binary. The hot set is the only thing that moved,
and it moved the result from **+23% throughput** to **−54%**.

Solving the mix for PgCache's hit cost: at 95.7% hit with misses costing ~174 µs,
`0.957·H + 0.043·174 = 62` gives **H ≈ 57 µs**. PgCache serves a hit ~20 µs
faster than the origin serves the query — and pays ~94 µs extra on a miss.

---

## 3. Break-even, measured

[`s2-zipf.sql`](scripts/s2-zipf.sql) over the full 1M keyspace, 20 s, 8 clients.
`random_zipfian`'s skew parameter moves the hit ratio continuously.

| skew | hit ratio | A origin | B PgCache | PgCache |
|---|---:|---|---|---|
| s=1.05 | 71.2% | 123 µs / 65,204 tps | 191 µs / 41,824 tps | **−36%** |
| s=1.20 | 82.3% | 116 µs / 68,672 tps | 138 µs / 57,798 tps | **−16%** |
| s=1.50 | 97.7% | 121 µs / 66,390 tps | 108 µs / 74,110 tps | **+12%** |

**The break-even sits between 82.3% and 97.7% hit ratio.** The algebra from
section 2 predicts it: with O=77, H=57, M=174,

```
break-even r = (M − O) / (M − H) = (174 − 77) / (174 − 57) = 82.9%
```

which is consistent with s=1.20 still losing at 82.3%.

That formula is the useful artifact of this probe, because it has two terms and
**both must hold**:

1. **H < O** — PgCache's hit must be cheaper than the origin's query. If it is
   not, no hit ratio saves it, because even r=1 leaves H > O.
2. **r > (M−O)/(M−H)** — and the required r rises steeply as O falls toward H.

This retro-explains every previous failure in the platform. openFGA measured
O ≈ 40 µs and H ≈ 45 µs: **condition 1 failed**, so its 90% hit ratio was
irrelevant and no amount of tuning could have worked. NetBox reached only 40%
hit ratio: **condition 2 failed** by a wide margin.

---

## 4. Saturation — where the real gain is

[`s1-hot.sql`](scripts/s1-hot.sql) at `hot=1000`, 20 s, threads=4.

| clients | A origin | B PgCache | tps gain | latency gain |
|---:|---|---|---:|---:|
| 1 | 77 µs / 12,995 | 62 µs / 16,227 | +25% | −19% |
| 8 | 120 µs / 66,815 | 101 µs / 79,343 | +19% | −16% |
| 32 | 259 µs / 123,753 | 195 µs / 164,481 | +33% | −25% |
| 64 | 579 µs / 110,610 | 395 µs / 162,097 | **+47%** | **−32%** |

**The origin's throughput peaks at 32 clients and falls at 64** — 123,753 down to
110,610, the classic saturation collapse. PgCache does not collapse: 164,481 to
162,097, essentially flat.

This is the result no real subject in this platform could produce. openFGA and
NetBox never generated enough load to leave the origin's comfortable region, so
every campaign measured PgCache's overhead and none of them measured its
offload. **The gain grows with load**, which is the opposite of what r5 appeared
to show before that finding was retracted as a warm-up artifact.

---

## 5. Where PgCache loses badly — and why C9 needed correcting

[`s3-range.sql`](scripts/s3-range.sql), `hot=1000`, 15 s, 1 client. An index
range scan plus a `sum()`.

| span | path | latency | tps | hit ratio |
|---|---|---:|---:|---:|
| 100 rows | A origin | 104 µs | 9,651 | — |
| | B PgCache | 159 µs | 6,275 | 95.6% |
| 10,000 rows | A origin | 826 µs | 1,211 | — |
| | B PgCache | **54,023 µs** | **19** | 17.0% |

At 95.6% hit ratio PgCache still loses on the 100-row aggregate. At 10,000 rows
it loses by **65×** and the hit ratio collapses to 17%.

C9 as currently written predicts the opposite: it says expensive origin queries
are the regime a cache needs. That is wrong as stated, and this is the
correction — **query cost has two sources and only one of them is bankable**:

- Cost from *work the cache can skip* (parse, plan, index descent, buffer
  lookups) is banked on every hit. This is what the point-select case shows.
- Cost from *result size* is not skipped. The cache must materialise the rows,
  store them, and ship them on every hit — and it must do so for each distinct
  parameterisation. Here 1,000 heavily-overlapping 10,000-row ranges asked
  PgCache to hold on the order of 10⁷ rows of materialised state.

PgCache's own `mv_size_ratio` exists precisely to gate this. The measurement is
what that gate looks like from outside when the workload sits on the wrong side
of it.

---

## What this does not show

- **No path C.** pgbench has no application, so "PgCache versus the app's own
  cache" — the platform's original question — cannot be asked here at all.
- **No correctness gate yet.** Every campaign to date carried a differential
  correctness check (openFGA's W7: 4,000 pairs, 100.000000% agreement). Nothing
  equivalent has run against pgbench. It must exist before any campaign result
  is published; the design is in [SCENARIOS.md](SCENARIOS.md).
- **No writes.** Everything above is read-only against static data. CDC
  invalidation cost under load is still entirely unmeasured — that is sysbench's
  job.
- **Absolute numbers do not transfer.** Docker Desktop on macOS inflates the
  origin's per-query cost to 77 µs against the ~40 µs measured on AKS. Since
  break-even is `(M−O)/(M−H)`, a *faster* origin raises the required hit ratio
  and could move the crossover above what any workload reaches. **Re-measuring
  H, O and M on AKS is the single most important thing the campaign must do**,
  and it is entirely possible that PgCache's win here does not survive it.

---

## Reproduce

```bash
cd synthetic && docker compose up -d origin client pgcache
docker compose exec -T client pgbench -h origin -U bench -i -s 10 -q bench
docker compose exec -T client pgbench -h origin  -U bench -n -M simple -c1 -j1 -T15 -f /scripts/s1-hot.sql -D hot=1000 bench
docker compose exec -T client pgbench -h pgcache -p 6432 -U bench -n -M simple -c1 -j1 -T15 -f /scripts/s1-hot.sql -D hot=1000 bench
```

Hit ratio from `curl -s http://localhost:59091/metrics | grep pgcache_queries_cache_`.

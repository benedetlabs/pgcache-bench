# 04 — Prior Results: The Local Campaigns

Everything in this document predates the AKS work. It records two campaigns run
on a single MacBook under Docker Compose, in the order they happened:

| | Campaign | Date | Config | Status |
|---|---|---|---|---|
| **A** | Original ladder run | 2026-07-25, ~03:00–03:35 UTC | `run_id` prefix `v3`, 1 repetition, document axis effectively uniform | **Retracted.** Four defects invalidate the performance conclusions. |
| **B** | Corrected re-run | 2026-07-25, ~21:49–22:51 UTC | `run_id` prefixes `r1`/`r2`/`r3`, 3 repetitions, `doc_dist=zipf`, CPU cap removed | Valid for rung E1 only. |

Both campaigns live in the same `results/` tree and the same
`results/all-runs.csv` (50 rows). They are told apart by `run_id` prefix and by
the `doc_dist` column: Campaign A rows predate that column and carry no value
(they render as `?` in `report/dados-consolidados.csv`); Campaign B rows carry
`zipf`.

**The regenerated report mixes them.** `report/RELATORIO.md`, dated
`2026-07-25 22:54 UTC`, presents rung E1 (Campaign B) and rungs E2/E3
(Campaign A) in one continuous set of tables with no marker separating them. Any
E2 or E3 number quoted from that file is a retracted number. This document exists
partly to make that separation explicit.

**A missing artifact, stated up front.** `PLAN.md` §12 points the reader to an
"ERRATA block at the top of `report/RELATORIO.md`". That block is not in the file.
`make report` regenerated `report/RELATORIO.md` at 19:54 local time and overwrote
the hand-written errata. The errata's substance survives only in `PLAN.md` §12's
summary and in [06-defect-log.md](06-defect-log.md). The dangling cross-reference
is itself a documentation defect and is not repaired here — it is recorded.

---

## 1. The host and the two configurations

Single machine, all containers competing for the same cores. The relevant limits
from `docker-compose.yml`:

| Service | Campaign A | Campaign B | Notes |
|---|---|---|---|
| `fga-origin` (Postgres 17) | `cpus: 3.0`, `mem_limit: 4g` | unchanged | `shared_buffers=1GB` fixed at every rung |
| `fga-pgcache` | `cpus: 2.0`, `mem_limit: 6g` | `cpus: 4.0`, `mem_limit: 5g` | Hosts proxy **plus an embedded PostgreSQL 18** |
| `fga-a` / `fga-b` / `fga-c` | `cpus: 2.0`, `mem_limit: 2g` | unchanged | |

Host RAM visible to the Docker VM: **7.75 GiB** (Docker reports this as the
effective limit for any container whose `mem_limit` exceeds it). This number
matters twice — see §2.3 and §6.

Rung E1 mass, from `results/E1/manifest.json`: 2,000 users, 500 groups, 1,000
folders, 10,000 documents, **84,598 tuples**, max folder depth 5. That fits in the
origin's 1 GB `shared_buffers` with room to spare, which is the condition
`PLAN.md` §H0 predicted the cache would lose under.

---

## 2. Campaign A — the original run, and why it is retracted

Campaign A produced the conclusion recorded in `PLAN.md` §12: H1, H2, H3 and H5
refuted; H0 confirmed and amplified; and a headline finding that "the bottleneck
was not the database — the origin sat at 4.8% CPU while OpenFGA saturated 2
vCPU."

A later audit of the committed artifacts found four defects. Each is documented
below with the file that contradicts the published claim. After the audit,
**H1/H2/H3/H5 revert to "not measured"** — the refutation does not survive
without regenerating the numbers. **H4 (correctness) is unaffected**: it depends
on none of the four.

### 2.1 The headline CPU claim has no supporting artifact

The triple `fga-a 198.2% / fga-origin 4.8% / fga-pgcache 0.2%` appears in **no
file under `results/`**. A repository-wide grep for `198.2` returns only two
hits, both in `benchmark-docs/` prose describing the defect itself.

The mechanism: `scripts/run.sh` invoked `docker stats --no-stream` **once, after
the generator had already exited**. Every Campaign A `docker-stats.csv` is a
single 12-line snapshot with three columns (`name,cpu_perc,mem_usage`) and no
timestamp — a measurement of an idle system taken after the load stopped.

Every `fga-a` sample in every Campaign A snapshot, sorted:

| Value | File |
|---:|---|
| 105.01% | `results/_contaminado/A-lad1/docker-stats.csv` |
| **2.84%** | `results/_contaminado-mem8g/w1/C-v3/docker-stats.csv` |
| 2.35% | `results/_contaminado-mem8g/w3/C-v3/docker-stats.csv` |
| 2.20% | `results/E2/w1/A-v3/docker-stats.csv` |
| 2.00% | `results/_contaminado-mem8g/w1/A-v3/docker-stats.csv` |
| 1.91% | `results/E2/w3/C-v3/docker-stats.csv` |
| … 13 further samples, all below 1.7% | |

The maximum outside quarantine is **2.84%**, not 198.2%. The single reading above
100% sits in `results/_contaminado/A-lad1/`, an ad-hoc ladder probe the author had
already quarantined — and in that same file the origin reads **31.31%**, not 4.8%.
Neither half of the published pair is reproducible from any committed file.

Campaign B replaced this with a background sampler at 2 s intervals writing
RFC3339 timestamps for the duration of the window (`ts,name,cpu_perc,mem_usage`,
243–639 samples per run), killed via `trap`.

### 2.2 The document axis had no locality

`PLAN.md` §6 specifies Zipf(α=1.1) over users **and objects**. The generator
seeded document selection with the request sequence number:

```go
d = p.PositiveDocFor(u, int(seq))   // w1
d = p.NegativeDocFor(u, int(seq))   // w3
```

`PositiveDocFor` and `NegativeDocFor` map that parameter through modular
arithmetic — `universe.go`: `m := (r*2654435761 + t*97) % p.Docs`. A monotonic
sequence multiplied by a large prime modulo `Docs` is a **permutation**, not a
skewed draw.

Reproduced by running `TestDocLocality` in
`tools/cmd/fgabench/generator_test.go`, where `-doc-dist=uniform` preserves the
Campaign A behaviour as a control:

```
w1: zipf=529 distintos (33.1%) · uniform=598 distintos (37.4%) em 1600 requisicoes
w3: zipf=598 distintos (37.4%) · uniform=1591 distintos (99.4%) em 1600 requisicoes
```

| Workload | Campaign A behaviour (`uniform`) | After fix (`zipf`) |
|---|---:|---:|
| W3 | 1,591 / 1,600 distinct = **99.4%** | 598 / 1,600 = 37.4% |
| W1 | 598 / 1,600 = 37.4% | 529 / 1,600 = 33.1% |

Without a repeated `object_id` there is no repeated `WHERE` clause — precisely
the condition a read cache exists to exploit. **The Campaign A W3 tables did not
test the hypothesis they claimed to test.** W1 was largely spared because
`PositiveDocFor` already restricts its codomain to documents the user can
actually view, so it repeated regardless of how it was seeded.

Note the discrepancy with [06-defect-log.md](06-defect-log.md) §D-01, which
reports W3 at `1,600 / 1,600 = 100.0% unique`. The reproducible figure from the
committed test is 1,591 / 1,600 = 99.4%. The conclusion is identical; the defect
log's number is rounded up and should be corrected to match the test output.

### 2.3 The PgCache container was CPU-capped below the origin

`docker-compose.yml` gave the origin `cpus: 3.0` and PgCache `cpus: 2.0` — while
PgCache runs a proxy **and an embedded PostgreSQL 18** on those two cores. That
is strictly less CPU to do strictly more work.

At rung E3 it hit the ceiling. From `results/E3/w1/B-v3/docker-stats.csv`,
verbatim:

```
fga-pgcache,202.07%,5.245GiB / 6GiB     <- 202% of a 2.0-CPU limit: saturated
fga-b,0.94%,81.35MiB / 2GiB             <- OpenFGA idle, waiting on the proxy
fga-origin,118.95%,976.7MiB / 4GiB
```

The corresponding run (`v3`, E3/w1/B) completed **26 of 1,600 requests**, dropping
1,575. While the container itself is the binding constraint, that collapse cannot
be attributed to the caching mechanism — it is not an equal comparison.

Campaign B raised the cap to `cpus: 4.0`, above the origin's 3.0. The rescue
hypothesis could then be tested rather than assumed.

### 2.4 Percentiles computed over survivors

The open-loop generator drops requests when its client queue overflows under
saturation. Drops are **not random** — they remove exactly the requests that
would have been slowest. Percentiles over the survivors therefore describe a
population that excludes the tail being measured.

Campaign A published these anyway. Worst cases, from `results/all-runs.csv`:

| Rung / workload / path | Completed | Dropped | Drop ratio | Published p99 |
|---|---:|---:|---:|---:|
| E3 / w1 / B | 26 | 1,575 | 98.4% | 39,454.78 ms |
| E3 / w3 / B | 27 | 1,574 | 98.3% | 37,145.31 ms |
| E3 / w1 / C | 491 | 1,110 | 69.3% | 28,185.72 ms |
| E2 / w3 / B | 748 | 853 | 53.3% | 21,043.19 ms |
| E3 / w1 / A | 1,046 | 555 | 34.7% | 13,366.60 ms |
| E2 / w1 / B | 1,073 | 528 | 33.0% | 12,709.35 ms |

A "p99 of 39,455 ms" derived from 26 samples has no referent. `report.py` now
applies `DROP_LIMIT = 0.10`; anything above is flagged and the gain comparison is
refused with an explicit message instead of a number. The current
`report/RELATORIO.md` carries those ⚠ markers on the E2/E3 rows — but it still
prints the percentiles, and readers do quote them.

### 2.5 What Campaign A numbers may still be used for

Nothing in latency. The E2/E3 rows in `report/RELATORIO.md` and
`report/dados-consolidados.csv` are retained as raw record, not as results.

Two things do survive:

- **H4, correctness.** `PLAN.md` §12: 3,657 decisions verified, zero divergences.
  W7 compares identical Check pairs against A and B and against the analytical
  oracle; it depends on none of the four defects.
- **Query amplification as an order of magnitude.** `queries_per_request` is
  computed from OpenFGA's own `datastore_query_count`, independent of the
  document axis and of the CPU cap. Campaign A observed 102–480 queries per
  `Check`; Campaign B, with real locality, observed 124–178. The claim "one Check
  becomes hundreds of small datastore queries" holds in both.

---

## 3. Campaign B — the corrected re-run

Rung **E1 only**. `doc_dist=zipf`, 3 repetitions per (workload × path), target
rate 40 rps in all nine cells, 60 s measurement window, adaptive warm-up excluded
from the window (30 s for paths A and C; 90–180 s for path B, stopped at the
hit-ratio knee). PgCache restarted before every path-B repetition, so each
repetition starts from a cold cache.

Median across the three repetitions, per column, with min–max in parentheses.
Source: `report/dados-consolidados.csv`, cross-checked against the 27 Campaign B
rows in `results/all-runs.csv`.

### E1 / W1 — `check-hot` (Zipf, ~85% positive)

| Path | p50 ms | p95 ms | p99 ms | queries/req | hit ratio | drop |
|---|---:|---:|---:|---:|---:|---:|
| A · baseline | 12.93 (12.12–12.98) | 22.83 (22.27–23.56) | 40.78 (31.30–47.55) | 131.19 | — | 0.04% |
| B · pgcache | 16.22 (13.96–16.60) | 202.19 (176.43–225.87) | 382.05 (358.07–392.18) | 138.83 | 76.4% (76.3–76.7) | 0.04% |
| C · appcache | 7.78 (7.69–7.83) | 33.84 (30.54–34.05) | 64.01 (57.50–67.30) | 64.46 | — | 0.04% |

PgCache vs. baseline: p50 −25.5% · p95 −785.8% · p99 −836.9%. All three paths held
40 rps; 7,200 samples each.

### E1 / W2 — `check-cold` (uniform, no locality by design)

| Path | p50 ms | p95 ms | p99 ms | queries/req | hit ratio | drop |
|---|---:|---:|---:|---:|---:|---:|
| A · baseline | 13.37 (13.17–22.94) | 28.04 (24.42–15,164.95) | 328.22 (111.39–15,646.75) | 171.47 | — | 8.9% |
| B · pgcache | 21.94 (18.25–50.40) | 3,377.56 (241.71–3,912.06) | 3,970.84 (542.43–4,813.06) | 177.97 | **not measured** | 1.9% |
| C · appcache ⚠ | 14.66 (12.15–25.13) | 3,954.29 (35.35–19,448.38) | 5,269.63 (91.70–20,118.82) | 131.32 | — | 14.6% |

⚠ Path C collapsed (drop > 10%); its percentiles describe survivors only.

W2 is the honest cost of the extra proxy hop: the workload is deliberately
locality-free, so the cache has nothing to hit. The min–max spreads here are
enormous — path A's p95 ranges from 24 ms to 15 s across three repetitions — which
means the median is a summary of an unstable condition, not of a stable one. The
host could not hold this cell steady.

**The path-B hit ratio for W2 is not a measurement.** The consolidated file prints
`0.000000` because that is the median of three repetitions, two of which failed to
scrape:

| Rep | `pgcache_hit_ratio` | Cause |
|---|---:|---|
| `r1-215818` | 0 | `pgcache-metrics-after.txt` was never written — the file is absent from the run directory |
| `r2-221934` | 0.7219 | Valid: 567,840 hits / 780,311 queries |
| `r3-224057` | 0 | Counters ran **backwards** (hits 485,275 → 92,842; registered shapes 37,307 → 25,507). `summary.json` records `pgcache_hits: -392433`, clamped to 0. 406 errors in 2,400 requests. |

Repetition `r3` is a PgCache restart mid-window — the same signature as §6, but
under the corrected `5g` limit rather than `8g`. Peak PgCache memory in
Campaign B reached **4.791 GiB of 5 GiB**. The memory ceiling was made real, but
it was not made comfortable. W2 path B should be treated as one valid repetition,
not three.

### E1 / W3 — `check-deny` (100% negative, full fan-out)

| Path | p50 ms | p95 ms | p99 ms | queries/req | hit ratio | drop |
|---|---:|---:|---:|---:|---:|---:|
| A · baseline | 10.07 (9.36–10.56) | 20.63 (18.99–22.11) | 43.42 (28.03–45.89) | 124.04 | — | 0.04% |
| B · pgcache | 11.36 (10.25–12.25) | 76.21 (57.13–76.63) | 301.32 (251.92–337.21) | 124.17 | **81.9%** (81.1–82.4) | 0.04% |
| C · appcache | 8.52 (8.29–8.61) | 21.35 (21.03–24.15) | 45.69 (38.01–60.67) | 64.02 | — | 0.04% |

PgCache vs. baseline: p50 −12.8% · p95 −269.4% · p99 −594.0%.

**W3 is the cleanest cell in the campaign.** Paths A and B ran 124.04 vs. 124.17
queries per request — a 0.1% difference. They performed the same work. Path C's
64.02 means it did roughly half the datastore work, so its win is partly a
different-workload win, not a faster-at-the-same-workload win. Every A/B/C cell
held 40 rps with a single dropped request out of 7,200.

---

## 4. What the correction changed, and what it did not

Two rescue hypotheses were available after Campaign A: *the cache never had
locality to exploit*, and *the cache container was starved*. Campaign B
eliminated both.

**Locality is now real.** W3's hit ratio rose from unmeasured/incoherent to
**81.9% median (81.1–82.4 across three repetitions)**. The adaptive warm-up
converged on a genuine plateau instead of climbing indefinitely — from
`docs/RUNBOOK.md` §5.4, the per-slice hit ratio during a W3 path-B warm-up:

```
[B/w3] aquecendo ate' o joelho do hit ratio (fatias de 30s, teto 3m0s)...
  30s: hit ratio da fatia 77.0%
  1m0s: hit ratio da fatia 87.8%
  1m30s: hit ratio da fatia 89.4%
  2m0s: hit ratio da fatia 90.6%
  2m30s: hit ratio da fatia 91.4%
[B/w3] joelho em 2m30s (delta 0.0088 < 0.0100)
```

A slice hit ratio that stabilises at 91.4% is a cache that is working.

**The CPU cap is gone.** With `cpus: 4.0` (400% available), PgCache no longer
pins. Measured from the timestamped samplers, path B only, rung E1:

| Cell / rep | Peak within measurement window | Peak in file (incl. warm-up) |
|---|---:|---:|
| w1 / r1 | 183.88% | 298.43% |
| w1 / r2 | 173.67% | 249.98% |
| w1 / r3 | 162.64% | 162.64% |
| w2 / r1 | 186.93% | 194.34% |
| w2 / r2 | 192.32% | 192.32% |
| w2 / r3 | 171.53% | 207.60% |
| w3 / r1 | 107.18% | 140.95% |
| w3 / r2 | 128.93% | 155.82% |
| w3 / r3 | 111.24% | **140.49%** |

The commonly quoted "**140.49% of 400% available**" is the whole-file peak for the
single run `E1/w3/B-r3-224652` — the run the §5 shape figures also come from. It
is not the campaign maximum. The campaign maximum in any file is 298.43%, during
w1/r1 warm-up; the maximum inside any measurement window is 192.32%. In no case
does PgCache reach the 400% ceiling, which is the point: **the container is no
longer the binding constraint.**

**PgCache still lost.** On W3, the cleanest cell, with 82% hit ratio, identical
query amplification, no CPU cap, no drops:

| | baseline (A) | pgcache (B) | ratio |
|---|---:|---:|---:|
| p50 | 10.07 ms | 11.36 ms | 1.13× |
| p95 | 20.63 ms | 76.21 ms | 3.69× |
| p99 | **43.42 ms** | **301.32 ms** | **6.94×** |

Both rescue hypotheses were eliminated and the sign of the result did not change.
That is the strongest statement Campaign B supports.

**A confound that neither campaign resolved.** Path B is not "path A plus a
cache." PgCache only classifies OpenFGA's queries as cacheable when `pgx` is in
`simple_protocol` mode, which also removes prepared statements on the origin. Path
B is therefore "path A **without prepared statements**, plus a cache."
`PLAN.md` §13 specifies running path A in `simple_protocol` to isolate that term.
**It was never run locally.** Some unknown fraction of B's penalty belongs to the
protocol change rather than to the cache.

---

## 5. The isolated mechanism: it is the cost of a MISS

The shape of the loss identifies the mechanism. On W3, p50 is nearly tied (10.07
vs. 11.36 ms) while p99 is 6.9× worse. A uniform per-query overhead would shift
the whole distribution, including the median. It did not. This is a **tail
signature**: most requests are served normally, and a minority pay something very
large.

At 82% hit ratio and ~124 queries per request, roughly **22 queries per request
still miss.** Each miss is not just a passthrough — it triggers shape
registration in the cache's embedded PostgreSQL.

From `results/E1/w3/B-r3-224652/pgcache-metrics-{before,after}.txt`, cumulative
values at the end of the window and the delta across the 60 s measurement window:

| Metric | Before | After | Δ (in window) |
|---|---:|---:|---:|
| `pgcache_cache_queries_registered` (gauge) | 53,004 | **29,300** | −23,704 (evicting) |
| `pgcache_query_registration_latency_seconds_sum` | 1,340.15 | **1,569.82** | 229.67 s |
| `pgcache_query_registration_latency_seconds_count` | 72,751 | **79,767** | 7,016 |
| `pgcache_cache_registration_throttled_total` | 11,898 | **57,425** | 45,527 |
| `pgcache_queries_cache_miss` | 84,760 | 137,340 | 52,580 |
| `pgcache_queries_cache_hit` | 518,997 | 764,419 | 245,422 |

Reading these:

- **Mean registration cost.** Cumulative: 1,569.82 s / 79,767 = **19.7 ms** per
  registration. Restricted to the measurement window: 229.67 s / 7,016 =
  **32.7 ms**. On a path whose p50 for an entire 124-query `Check` is 11.36 ms,
  a single registration is two to three times the cost of a whole request.
- **Nearly every miss attempts a registration.** In-window: 7,016 completed +
  45,527 throttled = 52,543 attempts against 52,580 misses. The match is
  essentially exact. **87% of attempts are throttled**, so the queue is the
  binding resource, and queueing produces exactly the long-tail shape observed.
- **The cache is evicting while it registers.** `cache_queries_registered` is a
  gauge and it *fell* from 53,004 to 29,300 during the window. The cache is
  simultaneously admitting new shapes and discarding old ones — churn, not
  convergence, even after a 120 s warm-up that had reached a 91.4% slice plateau.

The shape space is the underlying cause. `PLAN.md` §1 documents that OpenFGA's
Postgres adapter emits only single-table `SELECT`s with equality predicates —
ideal cache shapes individually. But under `simple_protocol` the parameters are
interpolated into the SQL text, so **every distinct parameter tuple is a distinct
shape.** With 10,000 documents and 2,000 users at rung E1, the shape space is
large enough that a Zipf-skewed workload still generates a steady arrival of
first-time shapes indefinitely. The cache never stops paying admission cost.

That is the finding: **PgCache's problem in this workload is not the cost of a
hit, it is the cost of a miss** — and OpenFGA's amplification of one request into
~124 queries guarantees that even a 82%-hit-ratio cache takes ~22 miss penalties
per user-visible request.

---

## 6. A second memory mistake, recorded for honesty

While correcting the CPU cap (§2.3), the memory limit was raised from `6g` to
`8g`. Host RAM available to the Docker VM is **7.75 GiB**. A `mem_limit` above
physical RAM is a cgroup that can never cut — it is not a limit at all. Docker
reports the container's limit as the host total, which is visible in the samples:

```
2026-07-25T15:57:31Z,fga-pgcache,105.20%,5.12GiB / 7.75GiB
2026-07-25T16:05:42Z,fga-pgcache,83.27%,5.123GiB / 7.75GiB
```

**5.12 GiB at rung E1, with only 84,598 tuples.** PgCache grew until it starved
the VM and restarted mid-window. The evidence is in the before/after metric pair
for `results/_contaminado-mem8g/w3/B-r1-155453/`:

| Metric | Before | After |
|---|---:|---:|
| `pgcache_queries_cache_hit` | 655,992 | **181,460** |
| `pgcache_queries_total` | 751,767 | **232,193** |
| `pgcache_cache_queries_registered` | 84,363 | **31,602** |

Counters do not decrease. These did, which means the process was replaced. The
same run logged **197 errors in 2,400 requests** (`results/_contaminado-mem8g/all-runs.csv`,
row `r1-155453`); the w1 path-B run in the same batch logged 68, and the w2
path-B run collapsed to 19.31 rps achieved with 1,242 dropped and a p50 of
37.9 seconds.

All ten affected runs were quarantined in `results/_contaminado-mem8g/` and the
limit was set to `5g` — a real ceiling below physical RAM, so the cache evicts
rather than exhausts, and if the process insists on more the cgroup kills it,
which is a *result* ("does not fit on this machine") rather than noise.

**This also qualifies §2.3.** The original `6g` was likewise above what a 7.75 GiB
host could spare, so Campaign A effectively ran with no memory ceiling either. The
CPU cap was a genuine configuration defect; the memory cap was a machine
limitation misdescribed as a limit. And as §3 notes, even the corrected `5g`
was not comfortable — Campaign B peaked at 4.791 GiB of 5 GiB and one W2
repetition still restarted.

---

## 7. Coverage gaps

Neither campaign covered the full experimental design.

| Workload | Local status |
|---|---|
| W1 `check-hot` | Campaign B: E1, 3 reps. Campaign A: E2, E3 (retracted). |
| W2 `check-cold` | Campaign B: E1, 3 reps — but path-B hit ratio valid in 1 of 3. Never run in Campaign A. |
| W3 `check-deny` | Campaign B: E1, 3 reps. Campaign A: E2, E3 (retracted). |
| **W4 `list-objects`** | **Never run locally.** No rows in `results/all-runs.csv`. |
| **W5 `mixed-write`** | **Never run locally.** No rows. CDC lag and invalidation window unmeasured. |
| **W6 `ceiling`** | **Never run locally.** No rows. H3 (throughput ceiling under an SLO) has no data at all. |
| W7 `differential` | Run as a gate on every rung in both campaigns. Emits no latency row by design. |

Rung coverage is equally thin. **Campaign B ran E1 and nothing else.** E2
(~1M tuples) and E3 (~10M tuples) exist only as Campaign A data and are
retracted. The scale ladder was designed to find the knee — the point where the
working set stops fitting in the origin's 1 GB `shared_buffers` and a cache starts
to pay for itself. **That knee was never located.** E1's 84,598 tuples sit far
below it by construction, which is why `PLAN.md` predicted H0 would hold there.

Consequently the honest scope of the local work is:

> At rung E1 on a shared laptop, with 82% cache hit ratio, identical query
> amplification and no resource cap, PgCache raises `Check` p99 by ~6.9× on the
> maximum-fan-out workload. This says nothing about rungs where the mass exceeds
> the origin's buffer cache, nothing about `ListObjects`, nothing about behaviour
> under write pressure, and nothing about the sustained-throughput ceiling.

The AKS campaign exists to address the environment confound (shared laptop →
dedicated nodes). It does not, by itself, close the W4/W5/W6 or the E2/E3 gaps.

---

## 8. Source files

| Claim in this document | File |
|---|---|
| Consolidated per-cell medians | `report/dados-consolidados.csv` |
| One row per run, 26 columns | `results/all-runs.csv` |
| Campaign B raw runs | `results/E1/w{1,2,3}/{A,B,C}-r{1,2,3}-*/` |
| Campaign A raw runs | `results/E2/`, `results/E3/`, and the `*-v3/` directories under `results/_contaminado-mem8g/` |
| Quarantined `mem_limit: 8g` runs | `results/_contaminado-mem8g/` |
| Quarantined ad-hoc ladder probe | `results/_contaminado/` |
| Cache internals (hits, shapes, registration latency, throttling) | `results/E1/w*/B-*/pgcache-metrics-{before,after}.txt` |
| CPU/memory time series | `results/E1/w*/*/docker-stats.csv` (4-column, timestamped) |
| Resource limits and the reasoning behind each change | `docker-compose.yml`, `pgcache` service comments |
| Errata summary (the original block is lost) | `PLAN.md` §12 |
| Locality regression test | `tools/cmd/fgabench/generator_test.go` |
| Warm-up knee output | `docs/RUNBOOK.md` §5.4 |
| Per-defect narrative and fixes | [06-defect-log.md](06-defect-log.md) |

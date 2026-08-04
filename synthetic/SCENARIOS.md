# Synthetic campaign — scenarios

Written **before** the AKS campaign runs, so the campaign cannot be steered by
its own results. That is the r8 lesson: the moment a rung is chosen because
PgCache wins there, the lab is fitting a benchmark instead of measuring one.

Evidence behind these choices: [ANALYSIS.md](ANALYSIS.md),
[RESULTS-probe0.md](RESULTS-probe0.md).

---

## The claim under test

> **PgCache pays when `H < O` and the hit ratio exceeds `(M−O)/(M−H)`.
> The gain grows with origin utilisation. Both conditions are properties of the
> workload, not of the cache's configuration.**

`O` = origin cost per query, `H` = PgCache cost on a hit, `M` = PgCache cost on
a miss. Probe 0 measured O=77 µs, H≈57 µs, M≈174 µs on a laptop, break-even
82.9%, observed crossover between 82.3% and 97.7%.

**The campaign's first job is to re-measure H, O and M on AKS**, because the
laptop's O is nearly twice the ~40 µs recorded on those nodes and break-even
rises as O falls. It is entirely possible the crossover moves above anything
reachable and PgCache loses everywhere. That outcome is publishable and must not
be engineered around.

---

## Topology

Two paths. **There is no path C** — pgbench has no application tier and no
cache of its own. Every report must say so in its own words rather than leaving
the reader to assume a three-path comparison.

```
client (pgbench Job) ──▶ origin   (postgres:17, shared_buffers=1GB)   = path A
client (pgbench Job) ──▶ pgcache ──▶ origin                            = path B
```

Cluster: **the existing AKS cluster and its `userpool` spot node pool**, which
the operator created by hand and which no automation in this repo may modify.
One pod per node, `Recreate` strategy on every deployment (D-19).

Origin config frozen at the openFGA/NetBox settings: `shared_buffers=1GB`,
`track_io_timing=on`, `pg_stat_statements`. **The ladder grows the dataset past
the buffer pool; it never shrinks the buffer pool.** Those are different moves
and C9 forbids the second.

---

## Workloads

### W1 — hot-set sweep · *the break-even curve*

[`s1-hot.sql`](scripts/s1-hot.sql). `hot ∈ {1e3, 1e4, 1e5, 1e6}` at fixed
concurrency (8 clients). Reports latency, tps and hit ratio per cell on both
paths.

Produces H, O and M directly, and the measured crossover to check against the
formula. **This is the primary result**, because it is the one that generalises:
a reader can locate their own workload's hit ratio on this curve.

Every cell is published, including `hot=1e6` where probe 0 measured 0.14% hit
ratio and a 2× loss.

### W2 — saturation sweep · *the offload curve*

[`s1-hot.sql`](scripts/s1-hot.sql) at `hot=1000`, clients ∈ {1, 8, 16, 32, 64,
128, 256}.

The headline candidate. Probe 0 showed the origin peaking at 32 clients and
falling at 64 while PgCache stayed flat, giving +47% throughput at the origin's
worst point. **The campaign must push past the origin's collapse point**, or it
repeats openFGA's mistake of measuring a bored origin.

Report origin CPU alongside tps, so the crossover can be stated as "above N%
origin utilisation" rather than as a client count that means nothing off this
hardware.

### W3 — skew sweep · *continuous hit ratio*

[`s2-zipf.sql`](scripts/s2-zipf.sql), `s ∈ {1.05, 1.1, 1.2, 1.3, 1.5}` at 8 and
64 clients.

W1's hard cutoff gives a cache all-or-nothing residency. Zipf has a tail, which
is where eviction is actually exercised. Two concurrency points because the
break-even should *fall* as the origin saturates — if it does not, the
saturation story in W2 is not what it appears to be.

### W4 — protocol axis · *unique to pgbench, never yet measured under load*

[`s1-hot.sql`](scripts/s1-hot.sql) at `hot=1000`, 32 clients,
`-M ∈ {simple, extended, prepared}`, both paths.

No other subject in this platform could run this. On openFGA simple protocol had
to be forced, so it was a confound; here all three modes are pgbench's own
flags and both paths run each mode identically.

C3 records that PgCache parses extended protocol correctly (Strapi spike, 25/25
statements) but that was a correctness check at rest, not a throughput
measurement. `prepared` is the interesting cell: server-side prepared statements
make the *origin* faster, which lowers O and therefore raises break-even.

### W5 — result-size axis · *where it breaks, published deliberately*

[`s3-range.sql`](scripts/s3-range.sql), `span ∈ {1, 10, 100, 1000, 10000}` at
`hot=1000`, 8 clients.

Probe 0 found PgCache losing at 95.6% hit ratio on a 100-row aggregate and by
65× at 10,000 rows. This sweep locates the row count where materialisation stops
paying, and reports PgCache's `mv_*` metrics alongside, so the finding is tied to
the mechanism rather than left as an anomaly.

A lab that only published W1–W4 would be selling. This is the scenario that
makes the campaign a measurement.

### W6 — write mix and CDC · *sysbench, the platform's largest blind spot*

`sysbench oltp_read_write`, `--skip_trx=on` (mandatory — the default wraps reads
in `BEGIN…COMMIT` and C2 fails outright), read/write ratio swept via
`--point_selects` and `--index_updates`.

**Two protocol arms, reported as a pair:** `--db-ps-mode=auto` is the reference,
`disable` is the second arm. Disabling prepared statements slows the origin,
which flatters PgCache; reporting only that arm would be strangling the origin
by the back door. State O for both.

Every campaign to date ran read-only against static data. Nothing is known about
CDC invalidation cost under load, replication lag at throughput, or how pinned
tables behave while their rows change. Report CDC lag as a time series, not a
mean.

### W7 — differential correctness gate · **blocking**

Non-negotiable, and it blocks publication of every cell above.

The platform has never published a performance number without one. openFGA's
gate ran 4,000 pairs and reported 100.000000% agreement with zero divergences,
across every campaign.

Design: N random `aid` values; for each, issue the identical statement to origin
and to PgCache and compare the result byte for byte. Run it **three times** — on
a cold cache, on a warm cache, and immediately after a burst of `UPDATE
pgbench_accounts SET abalance = …` so CDC invalidation is exercised rather than
assumed. Any divergence aborts the campaign.

---

## Scale ladder

| rung | scale | `pgbench_accounts` | versus 1 GB `shared_buffers` |
|---|---:|---:|---|
| P0 | 10 | 150 MB | fits entirely — the hardest case for a cache |
| P1 | 100 | ~1.5 GB | marginally exceeds |
| P2 | 400 | ~6 GB | clearly I/O bound |

P0 first and always. It is the openFGA condition — a fully warm buffer pool — and
if PgCache wins there the result needs no I/O story at all. P1 and P2 raise O,
which lowers break-even, and they must be reported as *scale*, never as the
rung where the cache finally looks good.

**Acceptance to climb:** a rung is only worth running if P0 produced a clean
sign, in either direction, with the correctness gate green. A rung run to rescue
the previous one is the r8 pattern.

---

## Abandon rule, pre-committed

Stop and publish the negative if, on AKS at P0:

1. **H ≥ O.** PgCache's hit costs at least what the origin's query costs. Then no
   hit ratio can help — condition 1 of the claim fails outright, exactly as it
   did on openFGA — and the honest report is that the laptop's slower origin
   manufactured the win.
2. **Break-even lands above 99%.** A cache that needs a 99% hit ratio describes
   no real workload, whatever the curve says.
3. **W7 diverges even once.** Correctness is not traded for throughput.

None of these is fixed by tuning PgCache. If any fires, the finding is the
finding.

---

## What gets reported, always

- Both paths, every cell, including the losses.
- `H`, `O`, `M` and the computed break-even, in the deployment measured — not
  carried over from probe 0.
- Hit ratio next to every latency number. A latency without its hit ratio is
  uninterpretable, which is what made openFGA's r5 retractable.
- Origin CPU next to every throughput number.
- **"No path C"**, in words, in every report.
- **"This measures PgCache's envelope, not an adoption decision for any
  application"**, in words, in every report.

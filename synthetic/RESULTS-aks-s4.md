# Campaigns s3/s4 — writes, percentiles, and two of our own conclusions corrected

**Ran:** 2026-08-04, AKS `eks-1`, namespace `pgcache-synth`, same P0 rung
(scale 10, 150 MB), same three-node topology as campaigns
[s1](RESULTS-aks-s1.md) and [s2](RESULTS-aks-s2.md). 2 repetitions, 45 s per
cell.

**Add a 10% write ratio and PgCache's throughput advantage is gone.** With no
writes it delivers 49,690 tps against the origin's 36,376 — 37% more. With one
transaction in ten writing to the same key range the reads draw from, it delivers
12,308 against 12,573, which is 2% **behind**. Over the same step, read latency at
the p99 goes from 16% below the origin's to 70% above it, while the hit ratio
never leaves 98%.

Correctness held throughout. A gate that samples reads *while* writes are in
flight ran 2,000 comparisons, of which **1,931 were conclusive and none
diverged**.

This pair of campaigns closed the platform's largest hole — the cost of coherence
under write load — and added percentiles, which nothing published before it had.
It also takes back two conclusions of ours, one of them the headline of campaigns
s1 and s2.

---

## What was measured

Read/write mixes built with pgbench's `@weight`, both scripts in autocommit,
`-M simple`. The writes hit the **same** key range the reads query. Writing
somewhere else would have produced a flattering and meaningless number, because
CDC would have had nothing cached to invalidate.

The two paths differ in host and port and nothing else, on three separate nodes,
as in s1 and s2.

---

## 1. The write axis — the advantage disappears

| writes | origin tps | PgCache tps | Δ tps | read p99, origin | read p99, PgCache | Δ p99 | hit ratio | CDC lag |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 0% | 36,376 | 49,690 | **+37%** | 0.313 ms | 0.264 ms | **−16%** | 98.6% | 0 |
| 5% | 19,175 | 20,120 | +5% | 0.294 ms | 0.484 ms | **+65%** | 98.8% | 155 KB |
| 10% | 12,573 | 12,308 | **−2%** | 0.300 ms | 0.511 ms | **+70%** | 98.7% | 102 KB |
| 30% | 4,784 | 4,738 | −1% | 0.312 ms | 0.561 ms | +80% | 98.4% | 225 KB |
| 50% | 2,910 | 2,794 | −4% | 0.319 ms | 0.576 ms | +81% | 98.1% | 202 KB |

Two readings, and the second is the more important one.

### Throughput: the writes drown the gain

**+37% with no writes, +5% at 5%, negative from 10% on.**

The mechanism is arithmetic and not at all subtle. On this bench an `UPDATE`
costs about 25 times a point select — 4.879 ms against 0.191 ms, both measured
directly. In a 90/10 mix the writes therefore take roughly three quarters of the
time budget, and **writes pass through both paths identically**. PgCache neither
accelerates nor delays them; they simply leave too little of the clock for a
read-side gain to show in the total.

The clearest evidence that this is not a caching effect is in the origin's own
column: **path A alone falls from 36,376 tps to 12,573** when one transaction in
ten becomes a write. The bottleneck has stopped being the read, for both paths at
once.

**This is a statement about this regime, not about all workloads.** The
qualification is load-bearing enough to belong right here: what decides the
outcome is not the fraction of transactions that write, but the share of the time
budget the writes consume, and the two only coincide when reads are cheap. The
reads here are 0.191 ms point selects, which is about as cheap as a read gets.
The [multi-tenant campaign](RESULTS-multitenant.md) inverted the ratio — an
eight-statement dashboard that costs the origin 77.9 ms against a single indexed
`UPDATE` — and at the same 10% write ratio the gain there was **+929%**, with a
100% hit ratio. Criterion C11 in `docs/TRIAGE-CRITERIA.md` carries both halves.

### Reads through the cache get slower under write load; reads against the origin do not

This is the measurement the platform had never made, and it is the cost of
coherence becoming visible.

Compare the two read p99 columns. **The origin is flat**: 0.294 to 0.319 ms,
whether 0% or 50% of the mix is writing. Serving a primary-key read, the origin
does not care what the write load is doing.

**PgCache is not flat.** Its read p99 goes from 0.264 ms — faster than the origin,
with no writes — to 0.576 ms, more than double its own starting point, ending up
**81% above** the origin instead of 16% below it.

And the hit ratio **stays at 98%** the whole way. So this is not CDC evicting
entries: the entries are still there and still being served. What changes is that
serving them costs more while the invalidation stream runs alongside.

> **What this means in practice.** Every number published in campaigns s1 and s2 —
> including the +176% throughput at 256 clients — was measured at **0% writes**.
> At a perfectly ordinary OLTP ratio of 10% writes, in this regime, the
> throughput advantage disappears and read p99 worsens by 70%.
>
> That does not invalidate s1 and s2; those campaigns measured what they said they
> measured. But quoting either of them without its write ratio attached is
> misleading.

---

## 2. Correctness under concurrent writes — the gate passed

The `w7` gate in earlier campaigns checks correctness **after** a burst of writes
has settled. That is the easy case. The hard case — and the only one where a
CDC-coherent cache can genuinely be caught serving stale data — is the read that
arrives **while** the writes are in flight.

**The method**, because comparing two hosts under concurrent mutation needs care:
read the origin, read PgCache, read the origin **again**. If the two origin reads
agree, the row was stable across the window and PgCache's answer *must* match. If
they disagree, the row changed mid-window and the sample is **inconclusive** —
counted and reported, never scored as a divergence.

That keeps the test honest in both directions: it cannot manufacture a false
divergence, and it cannot hide a real one.

The writer is **rate-limited on purpose**, to 50/s over 1,000 keys. Without a
limit it would change every key inside every window, every sample would come back
inconclusive, and the test would pass by never concluding anything.

| batch | conclusive, matching | divergent | inconclusive |
|---|---:|---:|---:|
| 1 | 393 | 0 | 7 |
| 2 | 382 | 0 | 18 |
| 3 | 388 | 0 | 12 |
| 4 | 381 | 0 | 19 |
| 5 | 387 | 0 | 13 |
| **total** | **1,931** | **0** | **69** |

**1,931 conclusive comparisons, zero divergences.**

The 69 inconclusive samples are the evidence that the test was not vacuous: they
are keys the writer did in fact change inside the sampling window. A result with
zero inconclusive samples would have meant the writer was not touching the range
being sampled, and the 1,931 matches would have proved nothing about concurrency.

CDC metrics at the end of the window: lag of 33,944 bytes, `staleness` of
**2.876 s**.

**One detail we cannot explain, recorded for that reason:** the
`pgcache_cache_invalidations` counter read **0** for the entire window, despite
6,000 `UPDATE`s being applied and a non-zero CDC lag. Either the counter measures
a specific kind of invalidation this path does not use, or entries are being
updated in place rather than invalidated. We did not confirm which, and are not
going to guess. What is measured is the observable behaviour: the answers match.

---

## 3. The s2 inference about the materialisation gate was wrong

Campaign s2 published an **inference** about the collapse at `span=10000`: that
the materialisation gate was refusing the entries. We instrumented
`pgcache_cache_mv_gate` to turn the inference into a fact.

It was wrong.

| span | origin | PgCache | hit ratio | `mv_admit` | `mv_reject` |
|---:|---:|---:|---:|---:|---:|
| 1 | 0.291 ms | **0.164 ms** | 99.4% | 0 | **1000** |
| 10 | 0.290 ms | 0.404 ms | 98.8% | 1000 | 0 |
| 100 | 0.306 ms | 0.401 ms | 99.0% | 1000 | 0 |
| 1,000 | 0.488 ms | **0.402 ms** | 98.9% | 1000 | 0 |
| 10,000 | 2.216 ms | **769.888 ms** | **15.2%** | **118** | **0** |

Two things the inference got wrong, and the second is the more interesting.

**The gate rejects where PgCache WINS.** At `span=1` it refuses all 1,000 entries
— and that is exactly where PgCache is fastest, 0.164 ms against the origin's
0.291. Refusing to materialise means serving from the plain cache path, which is
cheap. The rejection is a good decision, not a symptom.

**At `span=10000` the gate rejects nothing.** `mv_reject = 0`. It **admits 118**
of 1,000, and the other 882 appear in neither counter.

So the collapse is not a policy refusing work. It is consistent with a **build
queue that cannot keep up**: 882 entries stay pending, never complete inside the
window, and every query for them is a miss. Hence the 15.2% hit ratio.

That remains an inference — but now an inference with the previous candidate
**eliminated by measurement**, which is the difference between a guess and a
hypothesis. Confirming it needs `pgcache_cache_mv_build_queue` recorded per cell,
which this campaign did not do.

The flat cost between `span` 10 and 1,000 is explained by the same table: in that
range the gate admits everything, PgCache serves from the materialised view, and
its cost does not depend on how many rows the origin would have had to scan.

---

## 4. Percentiles — the rigour fix

Campaigns s1 and s2 reported **mean latency**, because that is what pgbench
prints. For a claim about latency the mean is the weakest statistic available: a
cache's damage lives in the tail, since a miss costs several times a hit.

Every cell now records p50, p95 and p99, sampling 1% of transactions via
`--log --sampling-rate`.

**Two method decisions change what the numbers mean.**

**The percentiles come from the read script only.** With writes in the mix, an
unfiltered p99 would describe the WAL flush rather than the cache — and since
writes pass through both paths identically, including them would compress the A/B
difference for a reason that has nothing to do with caching. In the table in §1,
`read p99` is the read script in isolation; the tps column is the whole mix.

**Percentiles from fewer than 200 samples are not reported.** Campaign s3 caught
this the honest way: the `span=10000` cell runs at 10 tps, so 45 s at a 1%
sampling rate produced about four samples, and the resulting "p99" came out at
296 ms against a **mean of 794 ms** in that same s3 run. A p99 below the mean is
arithmetically impossible for latency, and is the signal that the sample means
nothing. The harness now prints `-`.

**The good news:** where sampling is adequate, the p99 tracks the mean closely. In
the `w5` cells with no writes, the origin runs 0.291 ms mean and 0.463 ms p99;
PgCache runs 0.164 and 0.299. The conclusions s1 and s2 drew from means survive
at the p99.

---

## 5. A defect of ours, for the record

The `w6` cells in campaign s3 came out with misaligned columns and were discarded.
With more than one `-f` script, pgbench prints a block per script **in addition
to** the summary, each with its own `- latency average = ...` line. The parser's
pattern was not anchored to the start of the line, matched all three, and the
variable became a multi-line string that broke the `printf` silently.

It did not trip the empty-value guard, because a multi-line string is not empty.
Fixed with a start-of-line anchor and an `exit` on the first match; `w6` was rerun
from scratch as campaign s4. Everything reported above comes from that rerun.

---

## 6. What changes in the overall reading

Before these campaigns the summary was: *PgCache pays when `H < O` and the hit
ratio clears break-even, and the gain grows with load.*

That is still true — **for read workloads**. What was missing:

> The write ratio is as eliminating a condition as the hit ratio. At 10% writes
> over cheap reads, an unremarkable OLTP mix, the throughput advantage disappears
> and read p99 worsens by 70%, even at a 98% hit ratio.

Where PgCache still delivers a great deal: read-dominated workloads against an
origin under pressure. Where it does not: anything with significant write traffic
in the same key range — **when the reads are cheap enough for the writes to own
the clock**. The [multi-tenant campaign](RESULTS-multitenant.md) is the same
10% write ratio over expensive reads, and it keeps its gain.

---

## What is still unmeasured

- **Reads and writes on separate ranges.** Here the two deliberately share the
  same 1,000 keys, which is the worst case for invalidation. A real application
  usually writes to a small subset and reads from a large one. That gradient was
  not swept.
- **`mv_build_queue`**, which would confirm or demolish the new explanation of the
  `w5` cliff.
- **`pgcache_cache_invalidations` reading zero** under 6,000 `UPDATE`s. Either an
  instrumentation anomaly or real behaviour — we do not know.
- **Rungs P1 and P2**, with datasets larger than memory.
- **An application-level cache to compare against.** There is no application
  here, so this is origin versus PgCache and nothing else. It measures PgCache's
  envelope; it is not an adoption verdict for any application.

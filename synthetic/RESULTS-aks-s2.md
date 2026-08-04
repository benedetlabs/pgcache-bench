# Campaign s2 — the three axes s1 did not cover

**Ran:** 2026-08-04, AKS `eks-1`, namespace `pgcache-synth`, chart revision 3.
Same rung (P0, scale 10), same three-node topology, same protocol as
[campaign s1](RESULTS-aks-s1.md). 2 repetitions, 45 s per cell.

Three questions s1 left open: does the crossover predicted by the formula
actually sit where the formula says; does the win survive the protocol real
applications use; and where does the cache stop paying as results get larger.

Answers: **yes**, **yes but smaller**, and **it is not a size rule — it is a
cliff**, which corrects a criterion this platform wrote three days ago.

---

## w3 — Zipfian skew · the crossover, measured independently

8 clients. `random_zipfian` over the full 1M keyspace; skew moves the hit ratio
continuously.

| skew | hit ratio | A latency | A tps | B latency | B tps | tps |
|---|---:|---:|---:|---:|---:|---:|
| 1.05 | 67.3% | 254 µs | 31,528 | 296 µs | 27,024 | −14% |
| 1.10 | 76.5% | 245 µs | 32,714 | 258 µs | 31,085 | −5% |
| 1.20 | 86.0% | 254 µs | 31,541 | 222 µs | 36,124 | **+15%** |
| 1.30 | 93.1% | 251 µs | 31,914 | 195 µs | 41,120 | **+29%** |
| 1.50 | 98.6% | 249 µs | 32,156 | 160 µs | 50,088 | **+56%** |

**The crossover sits between 76.5% and 86.0%.** Campaign s1's w1 gave
H≈146 µs, M≈574 µs, O≈245 µs at this concurrency, and therefore predicted

```
break-even = (M − O)/(M − H) = (574 − 245)/(574 − 146) = 77%
```

The prediction came from a hot-set sweep with a hard cutoff; the crossover was
then observed on a completely different access distribution with a continuous
tail. They agree. That is the C10 formula surviving an independent test rather
than being fitted to the data that produced it.

Note the origin's throughput is flat across the whole sweep — 31.5k to 32.7k.
The origin does not care how skewed the access is; **only the cache does.** The
entire spread in this table belongs to path B.

---

## w4 — protocol axis · the one no other subject could run

32 clients, `hot=1000`. Both paths run each mode. On openFGA simple protocol had
to be forced on path B *only*, making it "path A without prepared statements plus
a cache" — the confound that made nine cells retractable. That confound cannot be
constructed here.

| protocol | hit | A latency | A tps | B latency | B tps | tps |
|---|---:|---:|---:|---:|---:|---:|
| simple | 98.6% | 515 µs | 62,128 | 267 µs | 119,824 | **+93%** |
| extended | 99.0% | 569 µs | 56,192 | 259 µs | 123,624 | **+120%** |
| prepared | 98.7% | 339 µs | **94,344** | 257 µs | 124,364 | **+32%** |

Two things, and the second is the one that matters for anyone reading this to
make a decision.

**PgCache is nearly protocol-independent.** 119,824 / 123,624 / 124,364 — a 4%
spread. Its serving cost does not depend on how the statement arrived.

**The origin is not.** 56,192 to 94,344, a **68% spread**. Server-side prepared
statements save it parse and planning on every query; extended protocol without
prepare is its *worst* mode, costing an extra round trip for no plan reuse.

So **the honest headline depends on the protocol the application uses**. s1's
+176% at 256 clients was measured at `-M simple`. In `prepared` — the default for
pgjdbc after five executions, and for most production drivers — the gain at 32
clients is **+32%, not +93%**. Still a win, and still with 24% lower latency, but
a third of the margin.

Any future report that quotes a number from this platform has to say which
protocol produced it.

---

## w5 — result size · a cliff, not a slope

8 clients, `hot=1000`, `SELECT sum(abalance) … WHERE aid BETWEEN :lo AND :lo+:span`.

| span | rows scanned | hit | A latency | A tps | B latency | B tps | tps |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 2 | 99.4% | 320 µs | 25,020 | 156 µs | 51,342 | **+105%** |
| 10 | 11 | 98.9% | 339 µs | 23,625 | 396 µs | 20,234 | −14% |
| 100 | 101 | 99.1% | 355 µs | 22,548 | 396 µs | 20,221 | −10% |
| 1,000 | 1,001 | 99.1% | 549 µs | 14,566 | 393 µs | 20,340 | **+40%** |
| 10,000 | 10,001 | **16.4%** | 2,396 µs | 3,338 | **609,315 µs** | **13** | **−100%** |

Non-monotonic, and the shape is the finding.

**PgCache's cost is flat at ~394 µs from span=10 to span=1,000.** It stores the
*result* — one aggregated row — so the number of rows the origin had to scan does
not reach it. The origin's cost is not flat: 339 → 355 → 549 µs. That is why the
sign flips back to positive at span=1,000: the origin has become expensive enough
to clear PgCache's fixed cost, exactly the mechanism C9 describes.

**Then, at span=10,000, it does not get slowly worse. It falls off a cliff.** The
hit ratio collapses from 99.1% to 16.4% — PgCache largely stops *admitting* these
entries, so almost every query pays the miss path, and at 609 ms per transaction
the path is 254× worse than the uncached origin.

The observable fact is the hit-ratio collapse. The likely mechanism — and this is
inference, not measurement — is dependency tracking: a `sum()` over 10,001 rows
returns one row but *depends on* 10,001, and with 1,000 distinct offsets that is
of the order of 10⁷ dependency edges to maintain for CDC invalidation.
PgCache's `mv_size_ratio` is the documented gate on exactly this. Confirming it
would need the `mv_*` metrics recorded per cell, which this campaign did not do.

---

## This corrects C9c, which this platform wrote three days ago

C9c was added on 2026-08-04 from the laptop probe and says: *"cost from result
size is not bankable."*

**That is too strong, and s2 falsifies it.** At span=1,000 the origin scans 1,001
rows, PgCache serves the aggregate flat, and PgCache **wins by 40% at a 99.1% hit
ratio**. Scan cost *is* bankable — that is the good case, not the bad one.

There was also a live suspicion that the laptop's 65× loss at span=10,000 was an
artifact of Docker Desktop's constrained memory. **It is not.** It reproduces on a
node with 30 GB, and worse: 254× instead of 65×.

Corrected statement:

> A cache banks the origin's scan cost right up to a capacity threshold, and at
> that threshold it stops admitting the entry rather than degrading gracefully.
> Past it the workload pays the miss path plus proxy overhead on nearly every
> query. The relevant quantity is not the size of the result but the size of the
> footprint the result must be invalidated against.

The practical test is unchanged and cheap: sweep the span and look for the hit
ratio falling off, rather than trusting a latency curve that looks smooth right up
to the edge.

---

## What still is not measured

- **Writes.** Everything above is static data apart from the w7 gate's update
  burst. CDC invalidation cost under sustained write load remains this platform's
  largest blind spot, and it is sysbench's job — with `--skip_trx=on` mandatory,
  and the `--db-ps-mode` arms reported in pairs.
- **P1 and P2.** P0 fits in `shared_buffers` entirely. Higher rungs raise O and
  lower break-even; they are scale, and must be reported as scale.
- **The `mv_*` metrics**, which would turn the w5 inference into a measurement.
- **Percentiles.** pgbench reports mean latency natively; p99 needs `--log`, and
  at 140,000 tps that is a separate arm rather than a flag.
- **Path C, permanently.** There is no application here. This is PgCache's
  envelope, not an adoption verdict.

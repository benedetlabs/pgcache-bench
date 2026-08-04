# Multi-tenant SaaS — the scenario that was supposed to be the hardest

**Campaign mt1 · AKS · 2026-08-04**

Multi-tenant came first precisely because it looked like the worst case. In a
SaaS every query carries `WHERE tenant_id = :t`, and the intuition was that this
multiplies the space of distinct queries by the number of active tenants,
dragging the hit ratio down until the cache is worth nothing.

That is not what happened. **PgCache delivered between 6.4× and 12.6× the
origin's throughput, at a 100% hit ratio in every single cell** — at 32 clients,
1,370 requests per second against 110, with the dashboard answering in 23.4 ms
instead of 290.6 ms. The advantage survived a 10% write ratio, the same level of
writing that had erased the gain entirely one campaign earlier.

Two things qualify that headline and both are in this report rather than under
it: the **differential correctness gate did not run for this workload**, and the
tenant count — the whole reason this scenario was chosen — turned out not to
matter, which falsifies the hypothesis the campaign was built on.

---

## What was measured

An application-shaped schema: seven tables, 463 MB, foreign keys and indexes of
the kind an ORM would create. `tenant_id` denormalised into `orders` and
`order_item` and indexed on `(tenant_id, created_at DESC)` and
`(tenant_id, status)` — the pattern real multi-tenant applications use, so that
every query can filter directly instead of joining its way back to the tenant.
Two hundred tenants, 2,500 orders each.

The 463 MB fit inside the origin's 1 GB `shared_buffers`. That is deliberate: it
is the **hardest** case for a cache, because the origin is already serving
everything from memory. It is exactly the condition that sank openFGA.

**The request** is a dashboard render — eight statements, all filtering by
tenant: order count by status, revenue sum, customer count, a page of recent
orders with `ORDER BY … LIMIT … OFFSET`, a count filtered by status, a join
against `customer`, a join against `product` aggregated per product, and a page
of line items. Besides the tenant, each request draws a pagination offset from
ten and a status from four, because it is that variability — not the tenant —
that makes a real application's query space grow.

Writes are a single statement: an `UPDATE orders` through a subquery indexed by
`tenant_id`. The mix is pgbench `-f script@weight`, autocommit, `-M simple`.

The two paths run on three separate nodes of the cluster, all in the
`brazilsouth-1` zone, and differ only in the database host and port. Nothing
else — a cross-zone split or a co-located origin would have been enough to
explain a result of this size on its own.

**On warm-up**, which is where this platform has got it wrong three times: every
path B cell was warmed with repeated 30 s passes until the hit ratio stopped
climbing — less than one percentage point of change between consecutive passes.
Two passes were enough in most cells, three at 200 tenants. A fixed warm-up pass
is a guess wearing the costume of a protocol, and it has already retracted nine
cells of a campaign of ours.

**Repeatability.** Three cells in this campaign share the same nominal
configuration — 8 clients, 200 tenants, no writes — and were measured
independently as part of three different sweeps: 83.05, 83.18 and 83.27 ms on the
origin, 7.085, 7.088 and 7.100 ms through PgCache. A spread of 0.3% and 0.2%.
That is the yardstick for judging the small differences later in this report.

---

## The correctness gate did not run for this workload

Every previous campaign on this platform blocked publication of any performance
number until it had compared, query by query, the origin's answer against
PgCache's. This report is publishing without that.

The previous gates always passed — thousands of comparisons across three
subjects, zero divergences, including reads arriving while writes were in flight
(campaign [s4](RESULTS-aks-s4.md), 1,931 conclusive comparisons, none divergent).
But this workload contains query shapes those gates never exercised: two-table
joins, `GROUP BY` with `ORDER BY`, pagination with `OFFSET`. That is precisely
where a divergence would be most plausible, and it is untested.

**Running the gate against this workload is the next thing to do**, before any
other scenario. Until it has, every number below is subject to it.

---

## The magnitude, and where it comes from

A dashboard request costs the origin **32.2 ms** at a single client with no
concurrency. It is not a toy query: eight statements with aggregations and joins
over one tenant's 2,500 orders and their line items. (A standalone single-client
measurement, run outside the sweep, put the same request at 39.5 ms and 25.3 rps;
the sweep's 32.2 ms is the figure used throughout, because it is the cell path B
was actually compared against.)

Varying concurrency, with 200 active tenants:

| clients | origin | PgCache | gain |
|---:|---|---|---:|
| 1 | 32.2 ms · 31 rps | 5.07 ms · 197 rps | **+535%** |
| 4 | 53.5 ms · 75 rps | 5.63 ms · 711 rps | +848% |
| 8 | 83.1 ms · 96 rps | 7.09 ms · 1,129 rps | +1076% |
| 16 | 148.7 ms · 108 rps | 11.80 ms · 1,356 rps | +1156% |
| 32 | 290.6 ms · 110 rps | 23.35 ms · 1,370 rps | +1145% |

The large number at the bottom is the sum of two effects, and they are worth
separating, because only one of them will follow a reader home.

**The first row is caching alone.** With one client there is no queueing anywhere
— not at the origin, not at the proxy — and PgCache still answers in 5.07 ms
against 32.2 ms. That is **6.4× faster**, with no help from concurrency at all.
Any reader whose origin is not under pressure should expect something of this
shape, not the bottom row.

**The rest is the origin saturating.** Read the middle column top to bottom: the
origin climbs from 31 to 110 requests per second and then stops. From 16 to 32
clients it gains two requests per second while its latency nearly doubles, 148.7
to 290.6 ms. It is saturated. PgCache over the same step goes from 1,356 to
1,370 rps with latency rising from 11.8 to 23.4 ms.

A dashboard at 290 ms is a dashboard the user experiences as broken. The same
dashboard through the cache answers in 23 ms.

### The tenant count did not matter

The hypothesis that motivated choosing this scenario simply did not hold.

| active tenants | origin | PgCache | gain | hit ratio |
|---:|---|---|---:|---:|
| 1 | 79.2 ms · 101 rps | 6.96 ms · 1,149 rps | +1038% | 100% |
| 10 | 81.2 ms · 99 rps | 7.00 ms · 1,143 rps | +1055% | 100% |
| 50 | 82.5 ms · 97 rps | 7.07 ms · 1,131 rps | +1066% | 100% |
| 200 | 83.3 ms · 96 rps | 7.09 ms · 1,129 rps | +1076% | 100% |

From one tenant to two hundred, PgCache's latency rises 1.8% — 6.96 to 7.09 ms —
and the hit ratio never leaves 100%. The gain moves 38 percentage points on a
base of a thousand, which is under 4% in relative terms, and it moves *upward*,
because the origin slows slightly faster than the cache does.

Two hundred tenants, ten pagination offsets and four status values across eight
statements is a query space in the thousands. It was enough to require a third
warm-up pass instead of two, and nothing beyond that. The query space of a SaaS
with this shape is large, but not large on the scale that would matter.

---

## Why this won so heavily, having lost on openFGA

The mechanism had already shown up in the [query-shape probe](RESULTS-shapes.md)
that preceded this campaign, and here it holds at scale.

In that probe, ten different query shapes were measured in isolation. PgCache's
latency stayed between 0.162 and 0.182 ms for all of them — twelve per cent
between the cheapest query in the set and the most expensive. The origin's ran
from 0.228 to 0.431 ms, eighty-nine per cent.

> PgCache charges an essentially fixed price per answer. The origin charges for
> the work. The gain is, almost exactly, how much work the query costs the origin.

A multi-tenant dashboard costs the origin a great deal — eight statements,
aggregations, joins — and costs PgCache the same as any other answer. That is the
order of magnitude.

openFGA was the exact opposite. Its origin served each query in 39.5 to 42.2 µs
and PgCache in 41.9 to 47.0 µs. There was no work to save; all that was left was
the proxy's overhead, multiplied by the 104 to 171 queries each `Check` issued.
The same product, the same mechanism, opposite results, because the origin was in
opposite regimes. Criterion C9 in `docs/TRIAGE-CRITERIA.md` is that comparison
written down.

---

## What this corrects about the write threshold

Criterion **C11** was recorded from campaign [s4](RESULTS-aks-s4.md): above
roughly 10% writes, the throughput advantage disappears. That campaign measured
exactly that — at 10% writes the gain fell from +37% to −2%.

Here, at the same 10% writes, the gain is still **+929%**.

| writes | tenants receiving writes | origin | PgCache | gain | hit ratio |
|---:|---:|---|---|---:|---:|
| 0% | — | 83.2 ms · 96 rps | 7.10 ms · 1,127 rps | +1074% | 100% |
| 5% | 1 | 79.0 ms · 101 rps | 7.20 ms · 1,111 rps | +1000% | 100% |
| 5% | 10 | 79.9 ms · 100 rps | 7.30 ms · 1,096 rps | +996% | 100% |
| 5% | 200 | 79.8 ms · 100 rps | 7.50 ms · 1,067 rps | +967% | 100% |
| 10% | 1 | 78.5 ms · 102 rps | 7.42 ms · 1,079 rps | +958% | 100% |
| 10% | 200 | 77.9 ms · 103 rps | 7.55 ms · 1,060 rps | +929% | 100% |

We entered this campaign expecting the explanation to be write **concentration**:
in s4 the reads and the writes shared the same thousand keys, the worst possible
case for invalidation, whereas a real application writes to a small subset and
reads from a large one.

**That hypothesis was wrong.** Compare the two 10% rows: writes concentrated on a
single tenant give +958%, spread across all two hundred give +929%. Twenty-nine
points out of nine hundred. The 5% rows move the same way and by as little —
+1000%, +996%, +967% as the writes spread from one tenant to ten to two hundred.

The direction is the one the hypothesis predicted, and the movement is larger
than this campaign's repeatability: PgCache's latency rises 1.8% between the two
10% cells, against the 0.2% spread measured across three repeats of an identical
configuration. So concentration probably does something. It just cannot be what
separates −2% from +929%.

What does explain it is the **ratio between the cost of a read and the cost of a
write**.

In campaign s4 the read was a 0.191 ms point select and the write a 4.879 ms
`UPDATE` — the write cost twenty-five times more. With 10% of transactions
writing, they consumed about **three quarters of the time budget**. And because
writes pass identically through both paths, the cache had nothing to accelerate
in the part that owned the clock.

Here the read is a dashboard the origin serves in 78 to 83 ms across this sweep,
and the write is one indexed `UPDATE`. At 10%
writes they consume a negligible fraction of the time. Nearly all of it is left
for the cache to work on.

> The correct rule is not "what fraction of transactions write". It is **what
> fraction of the time the writes consume**. An application with expensive reads
> tolerates a far higher write ratio before the cache stops paying.

This **qualifies** C11 rather than retracting it. Campaign s4 correctly measured
what it said it measured, in a regime where reads were extremely cheap. The error
was generalising from one regime to all of them.

---

## What this report cannot support

**No correctness gate.** Stated above in its own section because it is the first
thing a reviewer should weigh, not the last: every number here is provisional
until the differential gate runs against these query shapes.

**Mean latency only.** pgbench reports the mean natively; percentiles need
per-transaction logging, which this campaign did not collect. For a claim about
latency the mean is the weakest statistic available, because a cache's damage
lives in the tail. Campaign s4 has the percentiles for its own workload; this one
does not.

**No application-level cache to compare against.** There is no application here,
so the comparison has two paths, origin and PgCache, and no third. This measures
the product's envelope under application-shaped load; it is not an adoption
verdict. The question an adoption verdict has to answer — whether PgCache beats
the cache the application already ships — cannot be asked in this campaign in
either direction, and on the one subject where it was asked, openFGA, it was
close enough that the answer turned on configuration.

**One rung only.** The 463 MB fit in the origin's memory. That is the hardest
case for the cache, so the result is conservative on that axis — but nothing here
says what happens when the dataset exceeds memory.

**Two hundred tenants.** A SaaS with tens of thousands of active tenants has a
query space orders of magnitude larger. This campaign does not come close, and
the extrapolation is not safe: the curve was flat between 1 and 200, but nothing
guarantees it stays flat at 20,000.

---

## Where this leaves the platform

After two real subjects that failed triage, and a run of synthetic campaigns
dominated by primary-key point selects, this is the first scenario in which
PgCache delivers the kind of gain that would justify adopting it — and it
appeared when the load started to look like an actual application.

Read across the campaigns, the picture is: PgCache pays when the origin has real
work to save, and the real work is in the composed queries of a request — joins,
aggregations, pagination — not in the primary-key lookup where this lab spent
almost all of its time.

The correctness gate is missing. After that, the e-commerce and CMS scenarios.

---

*Raw data: [`data/mt-raw.md`](data/mt-raw.md). Related campaigns:
[query-shape probe](RESULTS-shapes.md), [write axis](RESULTS-aks-s4.md).*

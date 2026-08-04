# Is it worth putting a cache in front of Postgres?

**Report from a benchmark lab · August 2026**

This document is the whole story: what we tried, what went wrong, what we had to
retract, and what we finally measured. It is meant to be read end to end. The raw
numbers and the detail of each campaign live in the individual reports, linked
throughout.

---

## 1. The question

PgCache is a read cache that sits between an application and PostgreSQL. It
speaks the Postgres wire protocol, so the application does not know it is there —
you change the connection host and that is the whole integration. What makes it
interesting is coherence: instead of relying on a TTL, it follows the origin
database's replication log and invalidates the affected entries when the data
changes. In theory that gives you a cache's gain without a cache's classic
problem, which is serving stale data.

The question we set out to answer was simple to state:

> Does putting PgCache in front of a real application improve its latency and
> throughput, compared to having no cache — and compared to the cache the
> application already ships with?

And a second one, usually left implicit in comparisons like this, which we made
explicit from the start:

> Does it return the same answers?

### The detail that decides almost everything

There is one thing about PgCache that looks like an implementation detail and
turns out to be the central fact of this entire report: **a cache hit is not a
memory read.**

The PgCache image embeds a PostgreSQL of its own, and that is where the entries
live. When the cache hits, what happens is a full SQL round trip to that local
database — connection, parse, execution, result transport. Cheaper than going to
the origin, but not free, and nowhere near the nanosecond scale of a Redis `GET`.

That means PgCache has a **floor**. There is a minimum cost per query it cannot
go below. If the origin already serves faster than that floor, no amount of
configuration helps — the cache becomes pure added cost.

It took us two entire campaigns to work out that this was what was happening.

---

## 2. How we decided to measure

We built three paths, running the same application binary, against the same data,
on the same machine class:

- **Path A — baseline.** The application goes straight to Postgres. No cache.
- **Path B — PgCache.** The application goes to PgCache, which goes to Postgres.
- **Path C — the application's own cache.** The application uses the cache it
  ships with, against Postgres directly.

The only difference between A and B had to be the database address. Anything else
different and the comparison loses its meaning — we will come back to this,
because that is exactly where we got it wrong.

Path C exists because it is the honest competitor. It proves nothing to show that
PgCache beats an uncached database if the application already has a cache of its
own doing the same job for free. An application cache **eliminates** the query;
PgCache **accelerates** the query. Those are different things, and the difference
matters.

### A correctness gate that blocks everything

Before any performance number, every campaign runs a differential check: the same
query is issued to the origin and to PgCache and the results are compared byte for
byte. Any divergence aborts the campaign.

That gate has never failed. Across every campaign, on every subject, including
immediately after write bursts to force CDC invalidation — and, later, with reads
arriving **while** writes were in flight — PgCache returned exactly the same
result as the origin. Twelve thousand differential comparisons on openFGA alone,
six thousand more on the synthetic bench, zero divergences.

**If you take one conclusion from this report, take that one: PgCache's
correctness was never in question.** Everything else here is about performance.

One exception, stated up front because it is the newest result and it is not yet
covered: the multi-tenant campaign in section 10 published without the gate. That
section says so again where it belongs.

---

## 3. First subject: openFGA

openFGA is an authorization service — you ask "can user X read document Y?" and it
answers. We picked it because it looked perfect: written in Go, Postgres as its
primary store, and one property that is pure gold for a read cache.
**Amplification:** a single `Check` call generates between 104 and 171 database
queries, roughly 110 per request in steady state. If the cache absorbs each of
them, the gain per user-facing request is multiplied a hundredfold.

We ran eight campaigns, two on a laptop and six on AKS. Most of that time went on
fixing the lab rather than measuring the product — pods that would not start, Spot
nodes Azure reclaimed mid-window, results lost when an ephemeral volume died with
its pod. Not interesting, but it explains the calendar.

### What we measured, and why it was wrong

Campaign r5 produced a clean-looking table. PgCache lost, and the loss grew with
load — at 160 requests per second, path B's p99 was 159.92 ms against the
baseline's 16.17 ms, a factor of 9.9. We wrote that up as a finding about the
product. It was a finding about our configuration, and two things were wrong at
once.

**First: we never warmed the cache properly.** PgCache spends an initial period
learning which queries are worth keeping. We had written adaptive warm-up code —
it warms in slices until the hit ratio stops climbing — and never passed the flag
that enabled it. All 52 runs of r5 recorded `warmup_s = 45`, while the hit ratio
only levelled off at 2m30s. We measured a cold cache and called it PgCache.

**Second: we never used pinned tables.** The documentation describes a feature
where selected tables are preloaded and kept current by CDC rather than
invalidated, and our use case was literally its example — single-table reads, no
joins. We ran at defaults.

We re-ran it as r6, with warm-up to the knee of the curve and `PINNED_TABLES` set,
changing nothing else. The p99 of the three workloads fell from 25.16 / 21.94 /
28.68 ms to 15.25 / 12.92 / 13.12 ms. The penalty over an uncached origin went
from 1.93× / 1.86× / 2.81× to 1.20× / 1.09× / 1.35×. That 159.92 ms at 160 rps
became **16.74 ms** — 1.05× the baseline, effectively parity with having no cache
at all.

**We retracted r5's claim.** It is marked as retracted in the documents, with the
reason. A lab that does not retract is worth nothing.

Two details from that repair cut against the intuition. **The hit ratio went the
wrong way while latency improved** — W1 from 90.2% to 83.2%, W3 from 89.9% to
79.0%, both substantially faster. Pinning does not create more hits; it makes
misses cheap, and with ~110 queries per `Check` the dominant term is cost of miss ×
amplification. Anyone tuning this workload for hit ratio is optimising the wrong
number. **And the bill is cold start:** the hit-ratio knee moved from 2m30s stock
to 6m00s (W1) and 5m15s (W3) with pinning, while W2 hit our 7-minute ceiling in all
three repetitions without converging — so its numbers come from a cache still
forming, which makes them conservative but not converged.

### The real problem

Even repaired, PgCache did not beat the uncached origin: parity to a 35% p99
penalty, depending on workload.

Its standing against path C is worth stating precisely, because we have overstated
it in both directions before. Across the campaigns as a whole, openFGA's own
in-process cache won most cells. The first sign that this could change came from
the configuration probe, where a single pinned run landed at a 15.81 ms p99 against
path C's 16.37 — one treatment run against two controls, which the probe itself
recorded as "would be the first time" rather than as a result. Campaign r6 then
confirmed it across three repetitions: with pinned tables and warm-up to the knee,
PgCache beat path C in four of the seven conditions where all three paths were
comparable — 15.25 against 16.24 ms on W1, 12.92 against 13.83 on W2. It still lost
W3, the deny-heavy workload, where the origin is fastest to begin with and there is
least to gain.

Stated honestly, then: configured properly, PgCache beat the application's own cache
on the tail in most of the steady-state conditions we measured — and still trailed
the uncached origin. That is not a result anyone would deploy on.

Which is when we measured what we should have measured on day one. The origin served
each query in **39.5 to 42.2 microseconds**. PgCache served them in **41.9 to 47.0**.

About 5 microseconds of overhead per query. For a network proxy that is excellent.
But the sign is wrong, and no amount of configuration fixes a wrong sign —
multiplied by the 104 to 171 queries behind each `Check`, those 5 microseconds
became a visible per-request penalty.

Why was the origin so fast? The entire dataset — 84,598 tuples — fitted comfortably
inside a 1 GB `shared_buffers`, at a **99.93%** buffer pool hit rate. We had built a
read cache in front of a database that was already serving entirely from memory.

> **What we learned:** amplification multiplies whichever side is bigger. Many
> expensive queries is the best possible case for a cache. Many cheap ones is the
> worst, because what gets multiplied is the proxy's overhead.

That became criterion **C9**: the origin's cost per query must sit comfortably
above the proxy's floor. And a rule written alongside it, because we caught
ourselves tempted — **do not fix this by strangling the origin.** Capping the
database's memory to force I/O produces a scenario, not a measurement. We designed
such a campaign, r8, and killed it before it ran.

One confound r6 could not remove: path B carried `simple_protocol`, without which
PgCache classified about 99.97% of these queries as non-cacheable. That makes path B
"path A without prepared statements, plus a cache". Section 8 turns that confound
into a measurement.

---

## 4. Second subject: NetBox

We changed applications. NetBox is a network inventory and IP address management
tool — Django, and **Postgres is not an option, it is a requirement**, which is
rare and welcome.

It passed everything we knew how to check. Reads run in autocommit, outside
transactions. No views, no recursive CTEs, no tables without a primary key. High
amplification — the UI prefix list issues 117 statements, of which 100 are two
repeated shapes, which is the ideal pattern for a cache. And no native data cache,
so the comparison would be clean: NetBox's mandatory Redis stores a configuration
revision, not query results.

Before spending cluster time, we ran a cheap probe one evening, on a laptop. We
instrumented a single request and asked: how much of its time is database?

| endpoint | wall time | DB time | DB share | µs/query |
|---|---:|---:|---:|---:|
| device list (API, 13 queries) | 91.49 ms | 25 ms | 27.3% | 1,923 |
| prefix list (UI, 117 queries) | 190.34 ms | 6 ms | **3.2%** | **51.3** |

The database is between 3% and 27% of the request. The rest is Django —
serialization, permission evaluation, template rendering.

That is a **ceiling**. Even a cache that answered instantly could not improve the
prefix list by more than 3%. Hit ratio does not matter, configuration does not
matter: a cache at the data layer can only give back the share of the time the
data layer actually owns.

It failed for the second reason too, the openFGA one: at 51.3 µs per query the
origin is already at or below PgCache's serving floor of roughly 45 µs, and
PgCache served the same queries at 179.5 µs. It was slower on every endpoint, at
both data volumes, stock and with pinned tables and a warm cache. Its hit ratio
never got above 40.2%, and pinning moved it from 35% to 40% and changed no
verdict.

That became criterion **C9b**. The two together are worth more than their sum:

> **openFGA** failed because its origin was **too fast to beat** — 40 microseconds
> per query, with the datastore owning most of the request.
>
> **NetBox** failed because its application was **too slow for the database to
> matter** — queries that are expensive in absolute terms, but a minority of a
> request dominated by Python.

Opposite failures on the same axis. And what became clear is that the eight
criteria we had until then only ever asked *can this be cached?* None of them
asked *is it worth caching?*

One honest caveat, since the verdict rests on the harness: the probe ran
`django.test.Client` in-process under Docker Desktop on macOS, which inflates the
Python side and understates the database's share, and a real gunicorn deployment on
Linux would move that share up. That threatens the ceiling argument, not the per-query
one — both paths ran in the same process on the same machine.

---

## 5. The pivot

Two readings were available. One: PgCache is not worth it. The other: we had not
yet built a scenario in which the question could be answered.

What pointed to the second was an uncomfortable detail. Every openFGA campaign ran
at between 40 and 160 requests per second against an origin with a 99.93% buffer
hit rate. **The origin was bored.** It never came close to real work. (We did push
to 320 rps in r6, where the uncached baseline and PgCache both collapsed with 45.9%
and 50.6% dropped requests — and above a 10% drop ratio percentiles are meaningless,
because drops remove exactly the requests that would have been slowest. That point
taught us nothing about caching either.)

A cache does not help a bored database. Its value appears when the database is
struggling, and we had never generated enough load for that, because the real
applications could not generate it. The bottleneck was always the application.

So we changed strategy: instead of hunting for an application where PgCache would
shine, measure **PgCache's envelope** directly, with a tool that can saturate a
database.

That changes the question, and it should be said plainly. A synthetic tool has no
application tier, so there is no path C. The question stops being *"is this worth
adopting for my application?"* and becomes *"what does this product cost, what does
it deliver, and under what conditions?"* The second does not replace the first — but
it answers something the first, the way we were going about it, never would.

### Choosing the tool

We assessed five: pgbench, sysbench, HammerDB, BenchBase and YCSB. The deciding
detail in each case was how they talk to Postgres, verified in the source code
rather than the documentation.

**pgbench won by a wide margin.** PgCache needs values interpolated into the SQL
text, and pgbench does that **by default** — `simple` is its default `-M` mode.
Every other tool needs it forced, and forcing it de-optimises the origin, which
makes the cache look better for reasons that have nothing to do with the cache.
That is the error that spoiled campaign r5. pgbench also gives us what no real
application did: **control over the hit ratio**, varied continuously by one line of
script. On openFGA the ratio was 90% and that was that; on NetBox, 40%.

**sysbench was adopted second, scoped to the write and CDC axis**, with
`--skip_trx=on` mandatory and the prepared-statement arm reported in pairs, because
its driver defaults would otherwise re-create the r5 confound.

**BenchBase was rejected**, despite being the most attractive on the surface — the
only one with realistic multi-table workloads (Wikipedia, Twitter). Our first pass
deferred it over a protocol question: pgjdbc defaults to extended protocol with
`prepareThreshold=5`, so after five executions every statement becomes a
server-side prepared statement, and forcing `preferQueryMode=simple` would run
every workload in a mode no BenchBase user runs.

Reading the source settled it more bluntly, and made the protocol question moot.
`Worker.java` calls `setAutoCommit(false)` **in its constructor** — once, for the
connection's whole lifetime — and every transaction is then `executeWork(...)`
followed by `conn.commit()`. With autocommit off, pgjdbc emits `BEGIN` before the
first statement of each one, so every read in every BenchBase workload runs inside
an explicit transaction. PgCache passes everything inside `BEGIN…COMMIT` straight
through.

That is criterion C2, and it is fatal rather than inconvenient: the call sits in
the constructor, not per transaction, and there is no `skip_trx` equivalent to
turn off. Escaping it means patching BenchBase, at which point it is not
BenchBase.

We had already measured what this costs on an earlier subject: the same statement
was served from cache 25 times out of 25 outside a transaction, and **0 out of 25
inside one**, with a pre-warmed entry still not served.

**HammerDB was rejected** because its PostgreSQL driver runs the TPC-C transactions
as functions on the server: the client sends one opaque call, PgCache never sees
the statements, and there is nothing to cache. **YCSB was rejected** as a separate
subject, its read path being the same point select in a worse protocol; its
read/write profiles were reproduced as pgbench scripts instead.

---

## 6. The formula

The first pgbench probe, still on the laptop, produced the finding that
reorganised everything we had done up to then.

We measured three things:

- **O** — what a query costs on the origin
- **H** — what PgCache costs when it **hits**
- **M** — what PgCache costs when it **misses** (because a miss is a trip to the
  cache *and* a trip to the origin)

If `r` is the hit ratio, PgCache is ahead when its average cost is below the
origin's:

```
r · H  +  (1 − r) · M   <   O
```

Solving for `r`:

```
r  >  (M − O) / (M − H)
```

That is schoolbook arithmetic, but it has two consequences that are not obvious,
and both must hold.

**First: if `H ≥ O`, it is over.** If a hit costs the same as or more than the
origin's query, no hit ratio saves it — not even 100%. This is not a tuning
problem; it is structural.

**Second: a faster origin demands a higher hit ratio.** As `O` falls towards `H`,
the required hit ratio rises steeply. A very fast database does not merely shrink
the gain — it raises the price of entry.

On the laptop the measurement was O = 77 µs, H ≈ 57 µs, M ≈ 174 µs, so break-even
was 82.9%. Sweeping Zipfian skew moved the hit ratio continuously and the observed
crossover landed between 82.3% (PgCache −16%) and 97.7% (PgCache +12%), consistent
with the prediction.

### What it explains retroactively

This is where the formula pays for itself.

**openFGA:** `O ≈ 40 µs`, `H ≈ 45 µs`. Condition 1 failed. That 90% hit ratio we
spent two campaigns trying to improve was **irrelevant** — no configuration
existed. We spent weeks hunting a tuning problem that was an arithmetic problem.

**NetBox:** a 40% hit ratio against a much higher bar. It failed condition 2 by a
wide margin.

Had this criterion existed earlier, neither subject would have reached a campaign.
It became **C10**.

---

## 7. The results on Kubernetes

We took it to AKS: three dedicated nodes, one per component. That is not fussiness
— path B runs the origin **and** PgCache, while path A runs only the origin. If
they shared CPU, path B would have less processor for the origin exactly at the
concurrency where the comparison is decided.

Before believing any number we checked that all three nodes were in the same
availability zone. A cross-zone split would have added tens of microseconds to one
path and explained the entire result on its own.

Data volume: 1 million rows, 150 MB, against 1 GB of `shared_buffers`. We chose the
**hardest** case for the cache on purpose — the origin serves everything from
memory, which is precisely the condition that sank openFGA.

### The correctness gate

Three passes of 2,000 statements: cold cache, warm cache, and immediately after an
`UPDATE` on 5,000 rows to force CDC invalidation.

6,000 comparisons. **Zero divergences.**

### Calibration

| | value | conditions |
|---|---:|---|
| **O** — origin | **167 µs** | path A |
| **H** — PgCache on a hit | **119 µs** | at a 98.5% hit ratio |
| **M** — PgCache on a miss | **614 µs** | at a 5.9% hit ratio |

Break-even: `(614 − 167) / (614 − 119)` = **90.3%** at one client.

**For the first time in this platform, both conditions hold.** `H = 119 < O = 167`.

A useful surprise: on the laptop the origin cost 77 µs, and I had predicted that
real hardware would make it faster, raising the bar and perhaps erasing the gain.
The opposite happened — 167 µs on AKS, more than double — because here every query
crosses a real node-to-node hop that containers on one laptop did not. A hit crosses
that same single hop; a miss crosses two, which is why `M` rose further still, from
174 to 614 µs.

### The saturation curve

This is the result no real subject could produce. We held the hit ratio fixed near
98.6% and varied only concurrency:

| clients | origin (latency / tps) | PgCache (latency / tps) | tps gain |
|---:|---|---|---:|
| 1 | 196 µs / 5,106 | 115 µs / 8,681 | +70% |
| 8 | 255 µs / 31,391 | 154 µs / 51,930 | +65% |
| 16 | 321 µs / 49,988 | 178 µs / 89,882 | +80% |
| 32 | 516 µs / **62,078** | 265 µs / 120,964 | +95% |
| 64 | 1,073 µs / 59,687 | 506 µs / 126,518 | +112% |
| 128 | 2,183 µs / 58,661 | 965 µs / 132,688 | +126% |
| 256 | 5,053 µs / 50,706 | 1,827 µs / **140,099** | **+176%** |

Read the middle column top to bottom. **The origin peaks at 32 clients and then
loses ground** — 62,078 down to 50,706 transactions per second, an 18% decline, with
its latency rising tenfold over that stretch. That is the classic saturation
collapse: more clients contending for the same resource produce less useful work.
PgCache does not turn over. It climbs to 140,099, and across the whole sweep its
latency rises 16× (115 µs to 1,827) against the origin's 26× (196 to 5,053). At 256
clients: **2.8× the throughput at 36% of the latency.**

Note where the **smallest** gain is — at a single client. The comparison almost
everyone runs first is PgCache's worst case here, and it still wins by 70%.

A defect from this campaign, for the record: three cells recorded a hit ratio of
`0.0000` because a single 10-second `curl` to the metrics endpoint timed out. Scraped
by hand afterwards, the same cache was at 99.25% (12,089,948 hits against 91,810
misses). Those cells' latency and throughput come from pgbench and are unaffected,
but a latency without its hit ratio is uninterpretable — that is what made r5
retractable — so the fix was three attempts, a 30-second timeout, and aborting the
cell rather than recording a zero that reads like a cache failure.

### The crossover, confirmed independently

The calibration predicted that at 8 clients the crossover would sit at **77%** hit
ratio (H ≈ 146 µs, M ≈ 574 µs, O ≈ 245 µs at that concurrency). We tested it with a
completely different access distribution — Zipfian, with a continuous tail instead
of a hard cutoff:

| skew | hit ratio | result |
|---|---:|---|
| low | 67.3% | PgCache −14% |
| | 76.5% | PgCache −5% |
| | 86.0% | PgCache **+15%** |
| | 93.1% | PgCache **+29%** |
| high | 98.6% | PgCache **+56%** |

The crossover landed between 76.5% and 86.0%. The prediction came from one
experiment and the confirmation from another, so the formula was not fitted to the
data that confirmed it. Worth noticing too: the origin's throughput is **flat**
across that entire table, 31,528 to 32,714 tps. The origin does not care how skewed
the access is; the whole spread belongs to the cache.

---

## 8. Three things that qualify the result

A report that stopped at the previous section would be selling. These three are not
footnotes.

### The protocol moves the headline

Real applications almost always use prepared statements — the Java driver's default
after five executions, and most drivers' — which save Postgres from parsing and
planning on every query. We tested all three modes at 32 clients, both paths running
each:

| mode | origin | PgCache | gain |
|---|---:|---:|---:|
| simple | 62,128 tps | 119,824 tps | **+93%** |
| extended | 56,192 tps | 123,624 tps | **+120%** |
| prepared | **94,344 tps** | 124,364 tps | **+32%** |

**PgCache is nearly indifferent to the protocol** — a 4% spread across the three
modes. Its cost of serving does not depend on how the statement arrived.

**The origin is not.** It spreads 68% between its best and worst mode: server-side
prepared statements save it parse and planning on every query, while extended
protocol without prepare is its worst mode, paying an extra round trip for no plan
reuse.

Direct consequence: those +176% at 256 clients were measured in `simple` mode. In
`prepared`, at 32 clients, the gain is **+32%, not +93%**. Still a win, still with
24% lower latency — but a third of the margin.

**Every number leaving this platform from now on has to say which protocol
produced it.**

### There is a cliff, and it gives no warning

We tested queries that aggregate ranges of rows, varying the range size, at 8
clients:

| rows scanned | origin | PgCache | hit ratio | result |
|---:|---:|---:|---:|---|
| 2 | 320 µs | 156 µs | 99.4% | **+105%** |
| 11 | 339 µs | 396 µs | 98.9% | −14% |
| 101 | 355 µs | 396 µs | 99.1% | −10% |
| 1,001 | 549 µs | 393 µs | 99.1% | **+40%** |
| 10,001 | 2,423 µs | **714,506 µs** | **14.9%** | **−100%** |

Look at PgCache's column: **393 to 396 microseconds, flat**, from 11 to 1,001 rows.
It stores the *result* — a single aggregated value — so how many rows the origin
had to scan never reaches it. The origin's cost does grow: 339 → 355 → 549 µs. That
is why the sign turns positive again at 1,001 rows.

And then, at 10,001 rows, it does not get slightly worse. **It falls off a cliff.**
The hit ratio collapses from 99.1% to 14.9% — PgCache largely stops accepting these
entries — and the path becomes **295× worse** than the uncached origin. Both
repetitions agree: 609 ms at 16.4% hit and 820 ms at 13.4%.

That forced us to correct a criterion **we had written the same morning**. The
laptop version said "cost coming from result size is not bankable". That is wrong:
at 1,001 rows the cache banks the scan cost and wins by 40%. What is true is
something else:

> A cache banks the origin's scan cost right up to a capacity threshold, and at
> that threshold it **stops admitting** the entry rather than degrading gracefully.
> Past it, nearly every query pays the miss path plus proxy overhead. The governing
> quantity is not the size of the result but the size of the footprint it must be
> invalidated against.

We suspected the 65× loss we had seen on the laptop was an artifact of Docker
Desktop's constrained memory. It was not. It reproduces on a 30 GB node, worse.

**The practical lesson:** when evaluating, watch the **hit ratio**, not the
latency. The latency curve looks smooth right up to the edge of the cliff.

### This is not an adoption verdict

There is no path C in the synthetic work: no application, so no application cache to
compare against. What it measures is the **product's envelope** — under what
conditions it pays, and how much. If your application has a cache that eliminates the
query before it leaves the process, the relevant comparison is a different one, and
this bench did not answer it. On the one real subject where all three paths ran, that
comparison went both ways: a configured PgCache beat the in-process cache on the tail
at 40 to 160 rps, while at 320 rps the in-process cache held the full rate with a
29.69 ms p99 and zero drops as both PgCache and the uncached origin collapsed. A cache
that removes the query outright is the one that survives saturation.

---

## 9. Writes, which qualify everything above

Everything in the sections above was measured at **zero writes**. That is not a
configuration detail — it is a condition almost no production system lives in, and
it was the platform's largest hole. None of the eight earlier campaigns had written
a row during measurement.

We closed it by mixing reads and writes, with the writes landing on **the same key
range** the reads query. Writing somewhere else would produce a flattering,
meaningless number, because CDC would have nothing cached to invalidate.

| writes | origin tps | PgCache tps | gain | read p99 A | read p99 B |
|---:|---:|---:|---:|---:|---:|
| 0% | 36,376 | 49,690 | **+37%** | 0.313 ms | 0.264 ms |
| 5% | 19,175 | 20,120 | +5% | 0.294 ms | 0.484 ms |
| 10% | 12,573 | 12,308 | **−2%** | 0.300 ms | 0.511 ms |
| 30% | 4,784 | 4,738 | −1% | 0.312 ms | 0.561 ms |
| 50% | 2,910 | 2,794 | −4% | 0.319 ms | 0.576 ms |

**The throughput advantage shrinks to +5% at 5% writes and turns negative at 10%**
— a completely ordinary OLTP ratio.

The reason is arithmetic. On this bench an `UPDATE` costs about 25 times a point
select (4.879 ms against 0.191 ms, measured directly), and **writes pass through
both paths identically**: the cache neither accelerates nor delays them. In a 90/10
mix they consume most of the time and drown the gain on the read side. Total
throughput falls from 36,376 to 12,573 on the uncached path alone — the bottleneck
has stopped being the read.

**And there is a second effect, which is the cost of coherence becoming visible.**
Compare the last two columns. The origin's read p99 is **flat**: 0.294 to 0.319 ms,
whether writes are 0% or 50%. PgCache's climbs from 0.264 ms to 0.576 ms. At the
10% mix that matters most it is 0.511 ms against the origin's 0.300 — **70%
worse**; at 50% writes it is 81% above the origin.

The hit ratio holds at 98% throughout, so this is not CDC evicting entries. They are
still there and still being served; serving them simply costs more while the
invalidation stream runs alongside.

This does not retract the earlier sections — they measured what they said they
measured. But **no number from them may be quoted without its write ratio
attached.** It became criterion **C11**, and section 10 qualifies it further.

### Coherence held

We tested the one situation in which a CDC-coherent cache can actually be caught
serving stale data: a read arriving **while** writes are in flight.

The method needs care, because under concurrent mutation two reads can legitimately
differ. We read the origin, then PgCache, then the origin **again**. If the two
origin reads agree, the row was stable during the window and PgCache's answer must
match. If they disagree, the sample is inconclusive and is discarded — never scored
as a divergence. The writer is deliberately rate-limited to 50 writes per second
over 1,000 keys; unthrottled, it would change every key inside every window, every
sample would come back inconclusive, and the test would pass by never concluding
anything.

**1,931 conclusive comparisons, zero divergences.** The 69 inconclusive ones prove
the test was not empty: they are keys the writer did in fact change inside the
sampling window. CDC lag at the end of the window was 33,944 bytes, staleness
2.876 s.

One thing from that run I cannot explain, so I record it: the
`pgcache_cache_invalidations` counter read **0** for the whole window, despite 6,000
`UPDATE`s being applied and CDC lag being non-zero. Either it tracks a kind of
invalidation this path does not use, or entries are being updated in place rather
than invalidated. I did not confirm which. What is measured is the observable
behaviour: the answers match.

### And an inference of ours that fell

The cliff section said the materialisation gate was *refusing* the oversized entries.
We instrumented the counter and it showed the opposite, twice over. At `span=1`, where
PgCache is **fastest** (0.164 ms against the origin's 0.291), the gate rejects all
1,000 entries — refusing to materialise means serving from the cheap plain-cache path,
so the rejection is a good decision, not a symptom. At `span=10000`, where the collapse
happens, `mv_reject` is **zero**: the gate admits 118 of 1,000 and the remaining 882
appear in neither counter.

So the cliff is not a policy refusing work. It is consistent with a build queue that
cannot keep up. Still inference — but inference with the previous candidate eliminated
by measurement, which is the difference between a guess and a hypothesis.

### Percentiles, and a defect of ours

Earlier campaigns reported mean latency, because that is what pgbench prints — the
weakest statistic available for a latency claim, since a cache's damage lives in the
tail. All cells now record p50, p95 and p99 from a 1% transaction sample, taken from
the **read script only**: with writes in the mix an unfiltered p99 would describe the
WAL flush rather than the cache, and since writes pass identically through both paths,
including them would compress the A/B difference for reasons unrelated to caching.

Percentiles from fewer than 200 samples are not reported at all. The `span=10000`
cell runs at about 10 transactions per second, so 45 seconds at 1% sampling gave
roughly four samples and a "p99" of 296 ms against a **mean of 794 ms** — and a p99
below the mean is arithmetically impossible for latency, which is how you know the
sample means nothing. Where sampling is adequate the p99 tracks the mean closely: in
the write-free range sweep the origin runs 0.291 ms mean against 0.463 p99, PgCache
0.164 against 0.299. Conclusions drawn from means in the earlier sections survive.

The defect: the first attempt at this campaign produced misaligned columns and was
discarded. With more than one `-f` script, pgbench prints one block per script **in
addition** to the summary, each with its own `latency average = ...` line. Our
parser's pattern was not anchored to the start of the line, matched all three, and the
variable became a multi-line string that broke `printf` silently — without tripping
the empty-value guard, because a multi-line string is not empty.

---

## 10. The multi-tenant dashboard: the first order-of-magnitude gain

Everything up to here — eight openFGA campaigns, two probes and three synthetic
campaigns — was measured on primary-key point lookups. That is the shape a cache
has least to gain on, and we had never left it. The next two campaigns did.

### First, a twenty-minute probe: nothing is uncacheable

Before building composite requests, we measured ten query shapes in isolation
against an application-shaped schema — seven tables, 463 MB, 8 clients, simple
protocol.

| shape | origin | PgCache | gain |
|---|---:|---:|---:|
| `q01` PK lookup | 0.228 ms | 0.165 ms | +38% |
| `q03` `COUNT(*)` with WHERE | 0.241 ms | 0.162 ms | +48% |
| `q08` `ORDER BY … LIMIT 20` | 0.258 ms | 0.174 ms | +48% |
| `q06` `GROUP BY` + aggregate | 0.259 ms | 0.168 ms | +54% |
| `q07` `IN (10 ids)` | 0.261 ms | 0.182 ms | +44% |
| `q09` `LEFT JOIN` | 0.292 ms | 0.173 ms | +69% |
| `q04` two-table join | 0.330 ms | 0.171 ms | **+92%** |
| `q10` correlated `EXISTS` | 0.392 ms | 0.176 ms | **+123%** |
| `q05` three-table join | 0.431 ms | 0.179 ms | **+141%** |
| `q02` FK listing + LIMIT | 0.429 ms | 0.163 ms | **+163%** |

No shape was uncacheable, all ten reached 100% hit ratio, and all ten beat the
origin. But the mechanism is the point, and it is in the two middle columns.
PgCache's costs span **0.162 to 0.182 ms** — twelve per cent between the cheapest
and the most expensive query in the set. The origin's span **0.228 to 0.431 ms** —
eighty-nine per cent.

> PgCache charges a nearly fixed price per answer, whether it is a primary-key
> lookup or a three-table join. The origin charges for the work. The gain is,
> almost exactly, how much work the query costs the origin.

Sorted by origin cost, that table's gain is monotonically increasing. It is the
measured version of C9, and it says plainly that the PK lookup where this lab spent
almost all of its time is the **worst** case in the set.

Two defects of ours surfaced in that probe, and both are worth writing down. The
first run reported **0.0% hit ratio on all ten shapes**, including the PK lookup.
That was not a result, it was configuration: `helm --set` splits unescaped commas
into separate assignments, so `pgcache.allowedTables=product,category,...` was
discarded whole — `helm get values` came back empty and the deployment kept the
previous subject's table list. PgCache was working perfectly and refusing to cache
tables nobody had allowed. This had **already been logged as a defect** in the
openFGA lab, and I stepped on it again. The durable fix is not escaping the comma;
it is keeping the list in `values.yaml`, where it is recorded in the release and
cannot be mistyped.

The second run, with the tables allowed, produced a plausible and wrong table: `q01`
at 19.5% hit ratio losing 42%, while `q02` hit 100% and won 164%. The natural reading
would have been "some shapes cache better than others". It was warm-up: one 15-second
pass does not warm 20,000 distinct entries, and with two 20-second passes the same
`q01` on the same keyspace reaches **100% hit ratio and +38%**. Not a capacity limit
either — zero evictions, 8.4 GB used of a 26.9 GB budget, and the keyspace swept alone
reaches 100% at 500, 5,000 and 20,000 keys, falling to 44% only at 100,000. That table
was measuring **warm-up speed**, not cacheability. It is the same defect that retracted
nine cells of openFGA's r5, and the third time it has appeared. The rule that stands:
no path-B cell is worth anything without evidence that its hit ratio stabilised. A
fixed warm-up pass is a guess dressed as a protocol.

### Then the campaign: a SaaS dashboard

We chose multi-tenant first precisely because it looked like the worst case. In a
SaaS, every query carries `WHERE tenant_id = :t`, and the intuition was that this
multiplies the space of distinct queries by the number of active tenants, driving
the hit ratio down until the cache is worth nothing.

That is not what happened.

The workload is a dashboard render: **eight statements**, all filtered by tenant —
order counts by status, revenue sum, customer count, a recent-orders page with
`ORDER BY … LIMIT … OFFSET`, a status-filtered count, a join with `customer`, a join
with `product` aggregated by product, and a page of items. Each request also draws a
random pagination offset and status, because that variability — not the tenant — is
what makes a real application's query space grow. Two hundred tenants, 2,500 orders
each, 463 MB against 1 GB of `shared_buffers`, so once again the origin serves
entirely from memory: the hardest case for the cache, and the one that sank openFGA.

Each path-B cell was warmed with repeated 30-second passes until the hit ratio
stopped rising — less than one percentage point between consecutive passes. Two
passes were enough in most cells, three with 200 tenants.

| clients | origin | PgCache | gain |
|---:|---|---|---:|
| 1 | 32.2 ms · 31 rps | 5.07 ms · 197 rps | **+535%** |
| 4 | 53.5 ms · 75 rps | 5.63 ms · 711 rps | +848% |
| 8 | 83.1 ms · 96 rps | 7.09 ms · 1,129 rps | +1076% |
| 16 | 148.7 ms · 108 rps | 11.80 ms · 1,356 rps | +1156% |
| 32 | 290.6 ms · 110 rps | 23.35 ms · 1,370 rps | +1145% |

The throughput ratio runs from **6.4× at a single client to 12.6× at sixteen**, and
the large numbers at the bottom are the sum of two effects, which are worth
separating.

**The first row is pure cache.** With a single client there is no queueing anywhere,
not in the origin and not in the proxy, and PgCache still answers in 5.07 ms against
32.2 ms — **6.4 times faster**. None of that comes from concurrency.

**The rest comes from the origin saturating.** Look at the middle column: the origin
climbs from 31 to 110 requests per second and then stops. From 16 to 32 clients it
gains two requests per second while its latency doubles, from 148.7 to 290.6 ms.
PgCache over the same interval goes from 1,356 to 1,370 rps with latency rising from
11.8 to 23.4 ms — a throughput ratio of 12.6× at 16 clients and 12.5× at 32. A
dashboard at 290 ms is one the user experiences as broken. The same dashboard through
the cache answers in 23 ms.

**The hit ratio was 100% in every cell, and the number of tenants did not matter.**
From one tenant to two hundred the gain moves by less than four percentage points —
+1038%, +1055%, +1066%, +1076% — and the hit ratio never leaves 100%. Two hundred
tenants multiplied by ten pagination offsets and four statuses is a few thousand
distinct queries: enough to require three warm-up passes instead of two, and nothing
more. The query space of a SaaS with this shape is large, but not large on the scale
that would matter.

### This qualifies the write threshold from section 9

The write axis said: above roughly 10% writes, the throughput advantage is gone. At
10% it had fallen from +37% to −2%.

Here, at the same 10% writes, the gain is still **+929%**.

| writes | tenants written to | origin | PgCache | gain | hit |
|---:|---:|---|---|---:|---:|
| 0% | — | 83.2 ms · 96 rps | 7.10 ms · 1,127 rps | +1074% | 100% |
| 5% | 1 | 79.0 ms · 101 rps | 7.20 ms · 1,111 rps | +1000% | 100% |
| 5% | 10 | 79.9 ms · 100 rps | 7.30 ms · 1,096 rps | +996% | 100% |
| 5% | 200 | 79.8 ms · 100 rps | 7.50 ms · 1,067 rps | +967% | 100% |
| 10% | 1 | 78.5 ms · 102 rps | 7.42 ms · 1,079 rps | +958% | 100% |
| 10% | 200 | 77.9 ms · 103 rps | 7.55 ms · 1,060 rps | +929% | 100% |

We entered this campaign expecting the explanation to be write **concentration**: in
the earlier sweep, reads and writes shared the same thousand keys, the worst possible
case for invalidation, whereas a real application writes to a small subset and reads
from a large one.

**That hypothesis was wrong.** Compare the two 10% rows: writes concentrated on a
single tenant give +958%, spread across all two hundred give +929%. Twenty-nine
points out of nine hundred. Concentration barely matters.

What matters is the **ratio between the cost of a read and the cost of a write**.

In the earlier campaign the read was a 0.191 ms point select and the write a 4.879 ms
`UPDATE` — the write cost twenty-five times more. With 10% of transactions writing,
they consumed roughly three quarters of the time budget, and since writes pass
identically through both paths, the cache had nothing to accelerate in the part that
dominated the clock. Here the read is a dashboard of roughly 79 ms and the write is
one indexed `UPDATE`. At 10% writes they take a negligible fraction of the time, and
almost all of it is left for the cache to work on.

> The rule is not "what fraction of transactions write". It is **what fraction of the
> time budget writes consume**. An application with expensive reads tolerates a much
> higher write ratio before the cache stops paying.

That **qualifies** the earlier criterion rather than retracting it. The write-axis
campaign measured its own regime correctly, in a regime where reads were extremely
cheap. The error was generalising from one regime to all of them. The corrected test
is to measure read and write costs separately and compute `w·W / (w·W + (1−w)·R)`;
above roughly half, the cache cannot reach the part of the request that matters,
whatever the hit ratio.

### What this campaign cannot support

**The differential correctness gate did not run for this workload.** Every previous
campaign in this platform blocked publication of any performance number until the
origin's and PgCache's answers had been compared query by query. This one published
without it. The previous gates always passed — thousands of comparisons, three
subjects, zero divergences, including with reads arriving during in-flight writes —
but this workload contains query shapes those gates never exercised: two-table joins,
`GROUP BY` with `ORDER BY`, pagination with `OFFSET`. That is precisely where a
divergence would be most plausible. Running the gate against this workload is the
next thing to do, before any other scenario, and until then the numbers above are
subject to it.

**Mean latency only.** No percentiles were collected here, and for a latency claim the
mean is the weakest statistic available.

**One rung, two hundred tenants, no path C.** The 463 MB fit in the origin's memory,
which makes the result conservative on that axis but says nothing about datasets that
exceed it. A SaaS with tens of thousands of active tenants has a query space orders of
magnitude larger, and the curve being flat from 1 to 200 does not guarantee it stays
flat at 20,000. And there is no application here, so there is no application cache to
compare against: nothing in this campaign says what an in-process cache would have
done with the same dashboard, and on the one real subject where all three paths ran,
that comparison was close in both directions.

---

## 11. What we know

**PgCache is semantically transparent.** Thousands of differential comparisons across
three subjects, including immediately after CDC invalidation and with reads arriving
during in-flight writes. Zero divergences, every time. The one workload not yet
covered by a gate is the multi-tenant dashboard, and that is stated where it belongs.

**It pays when two conditions hold, and both are properties of the workload, not of
the configuration.** A hit must cost less than the origin's query, and the hit ratio
must clear `(M − O) / (M − H)`.

**The gain is the origin's work.** PgCache's cost per answer is close to fixed — 0.162
to 0.182 ms across ten query shapes, a 12% spread — while the origin's scales with
what the query asks for. That is why a primary-key lookup gains 38% and a three-table
join gains 141%, and why an eight-statement dashboard gains an order of magnitude.

**The gain also grows with load.** This is the opposite of what campaign r5 appeared to
show before we retracted it. The more the origin struggles, the more the cache
delivers — which is exactly when you need it. With the origin in collapse, 2.8× the
throughput at 36% of the latency on point selects, and up to 12.6× on the dashboard.

**The margin depends heavily on the driver's protocol.** +93% in simple mode, +32%
with prepared statements, at the same concurrency and the same hit ratio.

**There is a cliff on queries with a large invalidation footprint**, and it gives no
warning in the latency curve. Watch the hit ratio.

**Writes eliminate the gain when reads are cheap.** With a point-select read, a 10%
write ratio erases the throughput advantage and worsens read p99 by 70% even at a 98%
hit ratio. With an expensive read, the same 10% write ratio leaves the gain at +929%.
The governing quantity is the share of the time budget the writes consume, not the
share of transactions.

**Where percentiles exist, they agree with the means**, so conclusions drawn from mean
latency in the earlier campaigns survive.

## 12. What we still do not know

**Whether the multi-tenant numbers survive a correctness gate.** They have not been
checked, and those shapes are the least-exercised ones in the platform. This is the
next thing to run.

**Reads and writes on separate key ranges, for cheap reads.** In the write sweep both
deliberately shared the same 1,000 keys, the worst case for invalidation. The
multi-tenant campaign showed concentration was not the deciding factor there, but that
was in a regime where writes barely registered in the time budget. The gradient has
still not been swept where reads are cheap.

**Pinned tables under write load.** They are updated in place rather than invalidated,
which in principle protects them from exactly the effect measured in section 9. We did
not test it.

**Datasets larger than memory.** We only ran the rung where the data fits in
`shared_buffers`. Larger rungs raise the origin's cost and lower the break-even bar —
but they must be reported as scale, not as the rung where the cache finally looked
good.

**Percentiles at the top of the throughput range.** pgbench reports mean latency
natively; p99 needs per-transaction logging, which at 140,000 transactions per second
is a separate experiment rather than a flag.

**Real applications that pass the criteria.** That is still the original question, and
it is still open. What we have now is a triage that costs half an hour on a laptop —
measure `O`, `H` and `M`, compute the break-even, compare it against the hit ratio the
real workload produces, and check what share of the request the datastore owns — rather
than days of cluster time. What the multi-tenant result adds is a description of the
profile worth looking for: composite read requests with joins, aggregation and
pagination, against an origin under pressure, with writes that are cheap relative to
the reads.

---

## Appendix — where the detail lives

| document | contents |
|---|---|
| [`docs/TRIAGE-CRITERIA.md`](docs/TRIAGE-CRITERIA.md) | criteria C1–C11, each with the case that taught it |
| [`openFGA/benchmark-docs/`](openFGA/benchmark-docs/) | the eight openFGA campaigns, including the r5 retraction |
| [`netbox/RESULTS-probe0.md`](netbox/RESULTS-probe0.md) | the probe that settled NetBox in one evening |
| [`synthetic/ANALYSIS.md`](synthetic/ANALYSIS.md) | the five-tool assessment, with the code evidence behind each verdict |
| [`synthetic/RESULTS-probe0.md`](synthetic/RESULTS-probe0.md) | the local probe that produced the formula |
| [`synthetic/RESULTS-aks-s1.md`](synthetic/RESULTS-aks-s1.md) | calibration and the saturation curve on AKS |
| [`synthetic/RESULTS-aks-s2.md`](synthetic/RESULTS-aks-s2.md) | crossover, protocol and the cliff |
| [`synthetic/RESULTS-aks-s4.md`](synthetic/RESULTS-aks-s4.md) | writes, concurrent-write correctness, percentiles |
| [`synthetic/RESULTS-shapes.md`](synthetic/RESULTS-shapes.md) | the ten query shapes, measured in isolation |
| [`synthetic/RESULTS-multitenant.md`](synthetic/RESULTS-multitenant.md) | the multi-tenant SaaS dashboard |
| [`synthetic/SCENARIOS.md`](synthetic/SCENARIOS.md) | the campaign designs, written before they ran |

Per-cell raw data is in `synthetic/results-s1-cells.tsv` and
`synthetic/results-s2-cells.tsv`. Every experiment is reproducible from the commands
at the end of each report.

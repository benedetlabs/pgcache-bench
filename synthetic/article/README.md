# Putting a cache in front of Postgres: a multi-tenant SaaS, measured

**A complete walkthrough of campaign mt1 — the setup, the reasoning, the
mistakes, every artifact, and how to run it again.**

*PgCache benchmark platform · AKS · 2026-08-04*

---

## Table of contents

1. [What this is, and who it is for](#1-what-this-is-and-who-it-is-for)
2. [The question](#2-the-question)
3. [Background: the one fact that decides everything](#3-background-the-one-fact-that-decides-everything)
4. [Why multi-tenant, and what we expected to happen](#4-why-multi-tenant-and-what-we-expected-to-happen)
5. [The trick: a pgbench script is a request](#5-the-trick-a-pgbench-script-is-a-request)
6. [The infrastructure, and the traps in it](#6-the-infrastructure-and-the-traps-in-it)
7. [Step 1 — the schema](#7-step-1--the-schema)
8. [Step 2 — denormalising the tenant](#8-step-2--denormalising-the-tenant)
9. [Step 3 — the request](#9-step-3--the-request)
10. [Step 4 — the measurement protocol](#10-step-4--the-measurement-protocol)
11. [Sweep 1 — does the tenant count break the cache?](#11-sweep-1--does-the-tenant-count-break-the-cache)
12. [Sweep 2 — separating caching from saturation](#12-sweep-2--separating-caching-from-saturation)
13. [Sweep 3 — writes, and a correction to our own criterion](#13-sweep-3--writes-and-a-correction-to-our-own-criterion)
14. [Why this won, where an earlier subject lost](#14-why-this-won-where-an-earlier-subject-lost)
15. [What this does not prove](#15-what-this-does-not-prove)
16. [Reproducing it](#16-reproducing-it)
17. [Appendix: every artifact](#17-appendix-every-artifact)

---

## 1. What this is, and who it is for

This is the full account of one benchmark campaign: what we built, why each
decision was made the way it was, what the numbers came out as, and what we got
wrong along the way.

It is written for someone who has not seen the rest of this repository. You do
not need to know what PgCache is, what came before, or what any of our internal
criteria mean — everything is explained where it is first used.

It is also written to be **run again**. Every SQL file, every workload script
and every driver is in this folder, and section 16 is the complete command
sequence. That matters more than it sounds: when this campaign actually ran, the
schema and the workload scripts lived only in `/tmp` inside a Kubernetes pod.
When the pod went away, so did they. Reconstructing them for this article is the
first time the campaign became reproducible from the repository at all.

**The short version of the result:** PgCache answered an eight-statement SaaS
dashboard between **6.4× and 12.6× faster** than the uncached database, with a
**100% cache hit ratio in every cell we measured**, and the advantage survived a
10% write ratio — a level of writing that had wiped out the gain completely in an
earlier campaign.

The rest of this document is why that number should be believed, and the four
places where it should not be over-read.

---

## 2. The question

**PgCache** is a read cache that sits between an application and PostgreSQL. It
speaks the PostgreSQL wire protocol, so the application does not know it is
there — you point the connection string at the cache instead of the database and
change nothing else.

What makes it interesting is *coherence*. Most caches go stale and rely on a TTL:
you accept serving data that is up to N seconds old. PgCache instead follows the
origin database's replication log (its change stream) and invalidates the
affected entries when the underlying rows change. In principle that gives you a
cache's speed without a cache's classic problem.

The platform this campaign belongs to exists to answer one question:

> Does putting PgCache in front of a database actually make an application
> faster — and under what conditions does it stop being worth it?

Two subjects had already been tried and both failed, for opposite reasons. This
campaign is the third approach: instead of hunting for a real application where
the cache would shine, **model the shape of a real application's load directly**
and measure the envelope.

---

## 3. Background: the one fact that decides everything

Here is the thing about PgCache that sounds like an implementation detail and
turns out to drive every result in this document:

> **A cache hit is not a memory read. It is a full SQL round trip.**

The PgCache image embeds its own PostgreSQL instance, and that is where cached
entries live. When the cache "hits", what happens is a complete query against
that local database — connection, parse, execute, ship the rows back. It is
cheaper than going to the origin, but it is not free, and it is nowhere near the
nanoseconds of a Redis `GET`.

This means PgCache has a **floor**: a minimum cost per query that it cannot go
below. And it produces a rule that is easy to state and easy to forget:

> If the origin database already answers faster than that floor, no amount of
> configuration will help. The cache becomes pure overhead.

That is exactly what happened to the first subject we tried. Its database served
each query in 39.5–42.2 microseconds; PgCache served the same queries in
41.9–47.0. About 5 µs of overhead per query — genuinely good behaviour for a
network proxy — but the wrong **sign**. Multiplied across the 104–171 queries
that subject issued per request, those 5 µs became a visible penalty.

The reason the origin was so fast: its entire dataset fit in memory, running at a
**99.93% buffer hit rate**. We had built a read cache in front of a database that
was already serving entirely from RAM.

Keep that in mind for section 7, where you will see us deliberately do it again.

### The two paths

Every campaign compares two identical stacks that differ in exactly one thing:

- **Path A — the baseline.** The client talks straight to PostgreSQL.
- **Path B — PgCache.** The client talks to PgCache, which talks to PostgreSQL.

Same binary, same data, same machine class, same network. The connection host and
port is the only difference. Anything else that differs makes the comparison
meaningless.

You will see references elsewhere in this repository to a **path C** — the
application's *own* built-in cache, the honest competitor. There is **no path C
in this campaign**, because there is no application here: we are driving SQL
directly. That is a real limitation and section 15 comes back to it.

---

## 4. Why multi-tenant, and what we expected to happen

Three application shapes were on the table: an e-commerce catalogue, a
multi-tenant SaaS, and a content site. We ran multi-tenant first **on purpose,
because we expected it to be the worst one.**

The reasoning went like this. In a multi-tenant SaaS, every query is scoped to a
customer organisation:

```sql
SELECT ... FROM orders WHERE tenant_id = 47 ...
```

Under the simple query protocol — which is what PgCache needs, and we will come
back to why — the literal value is written into the SQL text before it is sent.
So `tenant_id = 47` and `tenant_id = 48` are two *different query strings*, and
therefore two different cache entries.

With 200 active tenants, every statement in the application multiplies into 200
distinct cache entries. Add pagination and filters and it multiplies again. The
hypothesis was that this **explosion of the query space** would drive the hit
ratio into the ground, and that the cache would be worth little or nothing.

If the cache survived the hardest case, the easier ones would need no defending.

**The hypothesis was wrong**, and section 11 shows by how much. That is worth
stating up front: this campaign was designed to find a limit and did not find
it, which is a different and less comfortable thing than a campaign designed to
find a win.

---

## 5. The trick: a pgbench script is a request

The tool is **pgbench**, which ships with PostgreSQL. It was chosen over four
alternatives for one decisive reason, explained in section 6.

pgbench has an obvious-looking weakness: it is built to hammer a database with
single statements, and every earlier campaign of ours used it that way — one
primary-key lookup at a time against `pgbench_accounts`. Real applications do not
do that. They issue eight, or twenty, or a hundred statements to render one
screen. That property is called **query amplification**, and it is the single
biggest factor in whether a cache helps, because whatever the cache does per
query gets multiplied by it.

The trick that made this campaign possible:

> **A pgbench `-f` script is one transaction, and pgbench reports its latency as
> one number.**

So a script containing eight statements is a **request** containing eight
statements, and what pgbench measures is per-request latency — the unit a user
actually experiences, and the unit an adoption decision is made in.

Amplification stops being a property of some application we have to go find, and
becomes a parameter we write down. That is the entire reason a synthetic tool can
model an application here.

---

## 6. The infrastructure, and the traps in it

### Why pgbench and not the alternatives

We assessed five tools: pgbench, sysbench, HammerDB, BenchBase and YCSB. The
deciding factor in every case was **how the tool talks to PostgreSQL**, checked
by reading each one's source rather than its documentation.

PgCache needs the **simple query protocol**: values interpolated into the SQL
text, rather than sent separately as bound parameters. pgbench does this **by
default** — `-M simple` is its default mode, and nothing has to be forced.

That sounds minor and is not. In an earlier campaign against a real application,
simple protocol had to be forced on the cached path only. So path B was not "path
A plus a cache" — it was "path A with prepared statements disabled, plus a
cache", and the two effects could not be separated. Nine cells of that campaign's
conclusion had to be retracted. **With pgbench there is no such confound to
make**, because both paths run the tool's own default.

The others each had a blocking problem:

- **BenchBase** (the most attractive on paper — real Wikipedia and Twitter
  workloads) calls `setAutoCommit(false)` in its `Worker` constructor, once, for
  the connection's whole lifetime. Every read in every workload therefore runs
  inside an explicit `BEGIN…COMMIT`, and **PgCache passes through everything
  inside a transaction**. Not tunable, not a flag — it is in the constructor.
- **HammerDB** runs its TPC-C transactions as server-side PostgreSQL functions.
  The client sends one opaque call and the body executes inside the server, so
  the cache never sees the statements at all.
- **YCSB** duplicates what pgbench already does, in a worse protocol.
- **sysbench** is viable but needs two non-default flags, one of which
  (`--db-ps-mode=disable`) makes the *origin* slower and would flatter the cache
  for the wrong reason.

### The cluster layout

Three components, **three separate Kubernetes nodes**:

```
                  ┌──────────────┐
                  │   loadgen    │   pgbench, postgres:17 image
                  │  (node 1)    │
                  └──────┬───────┘
                         │
            ┌────────────┴────────────┐
            │                         │
     path A │                         │ path B
            ▼                         ▼
    ┌──────────────┐          ┌──────────────┐
    │    origin    │◄─────────│   pgcache    │
    │   (node 3)   │   miss   │   (node 2)   │
    │ postgres:17  │          │ + embedded PG│
    └──────────────┘          └──────────────┘
```

**Why one pod per node, enforced by anti-affinity.** This is not tidiness. Path B
runs *both* the origin and PgCache; path A runs only the origin. If they shared a
node, path B would have less CPU available for the origin than path A had — at
exactly the concurrency where the comparison is decided. The result would be an
artifact of co-location.

The nodes come from a Kubernetes cluster-autoscaler pool configured 0–5. Nothing
in the chart scales the pool; the pods simply go `Pending` and the autoscaler
provisions what is needed:

```
Normal  TriggeredScaleUp  pod triggered scale-up: [{aks-userpool 1->4 (max: 5)}]
```

**Availability zones were checked before any number was believed.** All three
nodes landed in `brazilsouth-1`. A cross-zone split would have added tens of
microseconds to one path and could have explained a result of this size on its
own — so it is checked, not assumed:

```bash
kubectl get nodes -L topology.kubernetes.io/zone
```

**No CPU limits, requests only.** A CPU limit in Kubernetes becomes a CFS quota
enforced over 100 ms windows: a pod that exceeds it is frozen until the window
turns. That freeze shows up as a p99 spike indistinguishable from a cache miss,
which is precisely the signal we are trying to read. With one pod per node the
isolation comes from the node, and no quota is needed.

---

## 7. Step 1 — the schema

**File: [`sql/01-schema.sql`](sql/01-schema.sql)**

Seven tables shaped like an application, not like a benchmark.

| table | rows | size | role |
|---|---:|---:|---|
| `order_item` | 2,000,000 | 184 MB | line items — the big fact table |
| `orders` | 500,000 | 48 MB | the tenant-scoped centre of the workload |
| `review` | 300,000 | 41 MB | present so not every join hits the same tables |
| `product` | 200,000 | 22 MB | joined from `order_item` |
| `customer` | 100,000 | 9.6 MB | carries the native `tenant_id` |
| `seller` | 500 | 96 kB | small reference |
| `category` | 50 | 32 kB | small reference |
| **total** | | **463 MB** | |

### The cardinalities are not arbitrary

Each ratio was chosen to control a specific property of the workload:

- **100,000 customers over 200 tenants** = 500 customers per tenant.
- **500,000 orders over 100,000 customers** = 5 orders each, so **2,500 orders
  per tenant**. This is the number that matters most: it is what the dashboard's
  aggregates run over, and therefore it sets how much work the origin has to do
  per request. Too few and the query is trivial; too many and the request stops
  looking like a dashboard.
- **2,000,000 line items over 500,000 orders** = 4 per order, giving roughly
  10,000 line items per tenant for the join-and-aggregate statement.

The seeding uses `generate_series` and runs entirely inside the server. The whole
thing takes well under a minute and needs no external loader.

One deliberate detail in the line-item seed:

```sql
INSERT INTO order_item
SELECT g, 1 + (g % 500000), 1 + (g * 7 % 200000), ...
```

The `g * 7 % 200000` scatters products across orders. A naive `g % 200000` would
have correlated line-item id with product id, making the join artificially local
and cheaper than it should be.

### Every foreign key is populated

No NULLs in any FK column. This is not cosmetic: with an all-NULL foreign key,
query planners and ORMs alike can skip a join entirely, and the workload would
quietly stop exercising the thing it was written to exercise.

### The size is a deliberate handicap

**463 MB fits inside the origin's 1 GB `shared_buffers`.** The origin serves the
entire dataset from memory and never touches disk.

Recall section 3: this is precisely the condition that made the first subject
unwinnable. We chose it on purpose. It means the cache cannot win by avoiding
disk I/O — the easy story — and has to win on **work saved** or not at all. Any
result here is conservative on that axis.

### The indexes are the ones an ORM would create

One on every foreign key, plus the columns the workload filters on. Nothing
hand-tuned for the benchmark.

That cuts both ways, which is the point. A schema indexed *better* than a real
application would make the origin look artificially good; a schema indexed
*worse* would hand the cache an easy win. "What the ORM would have done" is the
only defensible default.

---

## 8. Step 2 — denormalising the tenant

**File: [`sql/02-tenant.sql`](sql/02-tenant.sql)**

After step 1, only `customer` carries `tenant_id`. To find an order's tenant you
must join back through `customer`; for a line item, join twice.

Real multi-tenant applications do not do that. They **copy `tenant_id` onto every
table**, precisely so that every query can filter on it directly with one index
and no join.

```sql
ALTER TABLE orders     ADD COLUMN IF NOT EXISTS tenant_id int;
ALTER TABLE order_item ADD COLUMN IF NOT EXISTS tenant_id int;

UPDATE orders     SET tenant_id = 1 + ((customer_id - 1) % 200) WHERE tenant_id IS NULL;
UPDATE order_item oi SET tenant_id = o.tenant_id
  FROM orders o WHERE o.id = oi.order_id AND oi.tenant_id IS NULL;
```

This is not only realism. **It protects the experiment.** If the tenant filter
required a join, every measurement would include that join's cost, and the
question we came to answer — does the tenant count explode the query space? —
would be confounded by it.

The value is *computed*, not looked up: step 1 defined
`customer.tenant_id = 1 + (id % 200)`, so the arithmetic above reproduces it
exactly.

Takes about 45 seconds — one `UPDATE` over 500,000 rows and one over 2,000,000.

### The composite indexes, and a fairness decision

```sql
CREATE INDEX orders_tenant_created ON orders(tenant_id, created_at DESC);
CREATE INDEX orders_tenant_status  ON orders(tenant_id, status);
CREATE INDEX oitem_tenant_product  ON order_item(tenant_id, product_id);
```

The first one earns its keep. The dashboard's recent-orders query is "this
tenant's orders, newest first, first 20". With a plain index on `tenant_id`, the
database finds the tenant's 2,500 rows and then **sorts them** to answer. With
`(tenant_id, created_at DESC)`, the index is already in the required order, so
the planner walks it and stops after 20 rows.

**This decision favours path A.** Without these indexes the origin would be
slower and PgCache would look better. Adding them is the kind of accidental
advantage a benchmark has to refuse — you index for the origin's benefit,
because the origin is the thing you are claiming to beat.

Verify with:

```sql
SELECT count(DISTINCT tenant_id) AS tenants,
       count(*) / count(DISTINCT tenant_id) AS orders_per_tenant
  FROM orders;
--  tenants | orders_per_tenant
--  --------+-------------------
--      200 |              2500
```

---

## 9. Step 3 — the request

**File: [`workload/dashboard.sql`](workload/dashboard.sql)**

Eight statements, all scoped to one tenant. This is a SaaS dashboard render:
summary cards at the top, a list of recent orders, a top-products panel, a page
of line items.

### The variables

```
\set t    random(1, :tenants)     -- which tenant
\set off  random(0, 9)            -- which page (10 pages)
\set st   random(1, 4)            -- which status filter (4 values)
```

`:tenants` is the sweep's independent variable. `:off` and `:st` are the
variability a real application has *beyond* the tenant — a user pages through
results and filters by status, and under simple protocol each combination is a
different SQL string and therefore a different cache entry.

Without them the query space would be 200 entries per statement and the test
would be trivially easy. With them it is 200 × 10 × 4 across eight statements —
thousands of distinct entries. **That was the point: give the query-space
explosion a real chance to happen.**

### Statement by statement

**1 — Order counts by status.** `GROUP BY` over the tenant's 2,500 orders. The
summary cards.

```sql
SELECT status, count(*) FROM orders WHERE tenant_id = :t GROUP BY status;
```

**2 — Revenue and order count.** Two aggregates over the same rows.

**3 — Customer count.** A different table through a different index, so the
request is not satisfied entirely out of one relation's cached pages.

**4 — The recent-orders page.** The statement the composite index exists for.
`OFFSET` makes each page a distinct entry.

```sql
SELECT id, status, total, created_at FROM orders WHERE tenant_id = :t
 ORDER BY created_at DESC LIMIT 20 OFFSET :off * 20;
```

**5 — A filtered count.** The number badge next to a status tab. `COUNT(*)` is
the classic cache target: **expensive to compute, tiny to store.**

**6 — Recent orders with the customer name.** A two-table join — what an ORM
emits when a list view shows a field from a related record.

**7 — Top products.** The most expensive statement in the request: a join across
roughly 10,000 line items, a `GROUP BY`, and a sort on the aggregate. No index
can serve this by walking; the origin has to do the work.

```sql
SELECT p.name, sum(oi.qty)
  FROM order_item oi JOIN product p ON p.id = oi.product_id
 WHERE oi.tenant_id = :t
 GROUP BY p.name ORDER BY 2 DESC LIMIT 10;
```

**8 — A page of line items with product names.** Join plus pagination combined.

### There is no BEGIN, and that is load-bearing

pgbench runs with `-M simple` and the script contains no transaction. Every
statement is its own implicit transaction.

This is not style. **PgCache passes through everything inside `BEGIN…COMMIT`** —
a script wrapped in a transaction would measure precisely nothing. It is the same
property that disqualified BenchBase in section 6, and it is worth checking in
any real application before assuming a cache can help it.

### What it costs before any caching

A single client, no concurrency, straight to the origin:

```
latency average = 39.511 ms
tps = 25.31
```

**Roughly 40 milliseconds per request.** Not a toy query — this is what a real
dashboard costs, and it is three orders of magnitude more expensive than the
0.19 ms point selects every earlier campaign of ours had used.

---

## 10. Step 4 — the measurement protocol

The measurements are only as good as the protocol around them, and this
platform's history is mostly a history of getting the protocol wrong.

### Warm-up: the mistake we have now made three times

**File: [`scripts/lib.sh`](scripts/lib.sh), function `warm_until_settled`**

A cache starts cold. Measure it too early and you measure the warm-up, not the
cache. Obvious — and we have published a wrong result from it three separate
times.

- **Campaign r5.** We had written adaptive warm-up code that runs until the hit
  ratio stabilises, and never passed the flag that enabled it. All 52 runs
  recorded 45 seconds of warm-up while the hit ratio was still climbing at two
  and a half minutes. **Nine cells of the conclusion were retracted.**
- **The query-shape probe**, weeks later. One 15-second pass made a plain
  primary-key lookup appear to reach only a **19.5%** hit ratio and lose by 42%.
  With adequate warm-up the identical cell reaches **100%** and wins by 38%. The
  ranking we nearly published was measuring warm-up speed, not cacheability.

So this campaign does not use a fixed warm-up. It repeats 30-second passes and
measures the hit ratio of each, stopping when two consecutive passes differ by
less than one percentage point:

```bash
warm_until_settled() {
  local prev=0 cur
  for p in 1 2 3 4 5; do
    read -r h0 m0 <<<"$(scrape)"
    run "$host" "$port" "$DURATION" "$@" >/dev/null
    read -r h1 m1 <<<"$(scrape)"
    cur=$(awk -v h=$((h1-h0)) -v m=$((m1-m0)) 'BEGIN{printf "%.4f", (h+m)?h/(h+m):0}')
    if awk -v c="$cur" -v q="$prev" 'BEGIN{exit !(c-q < 0.01 && c-q > -0.01)}'; then
      echo "$p"; return 0
    fi
    prev="$cur"
  done
  echo 5
}
```

The number of passes it took is reported in every result table, so a reader can
see it rather than trust it. It took two passes in most cells and three at 200
tenants.

> **A fixed warm-up pass is a guess wearing the costume of a protocol.**

### Reading the metrics, and a small infrastructure trap

PgCache exposes Prometheus metrics. The two that matter here:

```
pgcache_queries_cache_hit    <cumulative count>
pgcache_queries_cache_miss   <cumulative count>
```

They are **cumulative** since the process started, so every measurement takes a
*delta* around the window rather than reading them absolutely.

The scrape does not come from the load generator. The loadgen image is
`postgres:17`, which ships neither `curl` nor `wget` — and installing one at pod
startup is exactly the fragility that killed an earlier campaign with `exit 127`
halfway through. Instead the scrape goes through the Grafana pod on the system
node pool, which reaches PgCache over the node IP:

```bash
kubectl -n "$NS" exec deploy/lab-grafana -- \
  sh -c "wget -qO- --timeout=20 http://$PGCACHE_IP:9090/metrics"
```

There is a related lesson from an earlier campaign: a single 10-second scrape
timed out under high load and recorded `ratio=0.0000` on three cells. Scraped by
hand afterwards, that same cache was at **99.25%**. A latency without its hit
ratio is uninterpretable — so a failed scrape must stop the cell, not record a
zero that reads like a cache failure.

### Repeatability, measured rather than assumed

Three cells in this campaign share the same nominal configuration — 8 clients,
200 tenants, no writes — and were measured independently as part of three
different sweeps:

| measured in | origin | PgCache |
|---|---:|---:|
| tenant sweep | 83.265 ms | 7.085 ms |
| concurrency sweep | 83.052 ms | 7.088 ms |
| write sweep | 83.177 ms | 7.100 ms |

**A spread of 0.3% on the origin and 0.2% through the cache.** That is the
yardstick for judging every small difference later in this document — and it is
what makes the argument in section 13 about write concentration something other
than an assertion.

---

## 11. Sweep 1 — does the tenant count break the cache?

**Script: [`scripts/01-tenant-sweep.sh`](scripts/01-tenant-sweep.sh)**

The hypothesis, restated: every query carrying `WHERE tenant_id` multiplies the
query space by the number of active tenants, so the hit ratio should fall as
tenants rise.

Concurrency held at 8 clients so tenant count is the only variable.

| active tenants | origin | PgCache | gain | hit ratio | warm passes |
|---:|---|---|---:|---:|---:|
| 1 | 79.2 ms · 101 rps | 6.96 ms · 1,149 rps | +1038% | 100% | 2 |
| 10 | 81.2 ms · 99 rps | 7.00 ms · 1,143 rps | +1055% | 100% | 2 |
| 50 | 82.5 ms · 97 rps | 7.07 ms · 1,131 rps | +1066% | 100% | 2 |
| 200 | 83.3 ms · 96 rps | 7.09 ms · 1,129 rps | +1076% | 100% | 3 |

### How to read this

Look at the hit-ratio column first. **It does not move.** From one tenant to two
hundred, the cache answers every single query from its own store.

Then look at PgCache's latency: 6.96 ms to 7.09 ms. It rises **1.8%** across a
200× increase in tenant count.

The gain moves 38 percentage points — from +1038% to +1076% — which on a base of
a thousand is under 4% in relative terms. And note the direction: it moves
**upward**, because the origin slows slightly faster than the cache does as the
tenant count grows.

The only visible cost of 200 tenants is in the last column: warming took three
passes instead of two.

### The hypothesis was wrong

Two hundred tenants × ten pagination offsets × four status values, across eight
statements, is a query space in the thousands. It was enough to need one extra
warm-up pass and nothing else.

**The query space of a SaaS with this shape is large, but not large on the scale
that would matter to this cache.**

That is a genuinely uncomfortable outcome to report. We chose this scenario
because we expected it to expose a limit, and it did not. Section 15 says how far
that finding can honestly be pushed — 200 tenants is not 20,000, and the flatness
of this curve is not a licence to extrapolate.

---

## 12. Sweep 2 — separating caching from saturation

**Script: [`scripts/02-concurrency-sweep.sh`](scripts/02-concurrency-sweep.sh)**

A large number at high concurrency is **two effects added together**, and only
one of them follows a reader home.

| clients | origin | PgCache | gain |
|---:|---|---|---:|
| 1 | 32.2 ms · 31 rps | 5.07 ms · 197 rps | **+535%** |
| 4 | 53.5 ms · 75 rps | 5.63 ms · 711 rps | +848% |
| 8 | 83.1 ms · 96 rps | 7.09 ms · 1,129 rps | +1076% |
| 16 | 148.7 ms · 108 rps | 11.80 ms · 1,356 rps | +1156% |
| 32 | 290.6 ms · 110 rps | 23.35 ms · 1,370 rps | +1145% |

### The first row is caching, alone

With one client there is **no queueing anywhere** — not at the origin, not at the
proxy. Nothing is waiting for anything. And PgCache still answers in 5.07 ms
against 32.2 ms.

**That is 6.4× faster, with no help from concurrency at all.**

This is the number that transfers. A reader whose database is not under pressure
should expect something of this shape, not the bottom row.

### The rest is the origin falling over

Read the origin column top to bottom. It climbs from 31 to 110 requests per
second — and then stops. From 16 clients to 32 it gains **two** requests per
second while its latency nearly doubles, 148.7 ms to 290.6 ms.

That is textbook saturation: past the knee, additional concurrency buys you
queueing rather than throughput. The database is doing its maximum work and every
extra client just waits longer.

PgCache over the same step goes from 1,356 to 1,370 rps, with latency rising 11.8
to 23.4 ms. It is approaching its own limit but has not turned over.

A dashboard at **290 ms** is one a user experiences as broken. The same dashboard
through the cache answers in **23 ms**.

### Why the two effects must be reported separately

If you quote only the bottom row — "12× faster!" — you are describing a database
that is already failing. That is a real and common situation, but it is a
different claim from "the cache makes requests faster", and conflating them is
how benchmarks mislead.

Both numbers are true. They answer different questions:

- *"Will this make my dashboard faster?"* → **6.4×**, the single-client row.
- *"Will this save me from my database falling over at peak?"* → **12×**, the
  bottom row.

---

## 13. Sweep 3 — writes, and a correction to our own criterion

**Script: [`scripts/03-write-sweep.sh`](scripts/03-write-sweep.sh)**
**Workload: [`workload/write.sql`](workload/write.sql)**

This sweep exists because a previous campaign had produced an apparently
devastating result, and we needed to know whether it generalised.

### What the earlier campaign found

Campaign s4 mixed reads and writes over pgbench's single-table workload. The
result:

| writes | origin | PgCache | gain |
|---:|---:|---:|---:|
| 0% | 36,376 tps | 49,690 tps | **+37%** |
| 10% | 12,573 tps | 12,308 tps | **−2%** |

At a perfectly ordinary OLTP write ratio, the advantage was **gone**. We wrote
that up as a criterion: above roughly 10% writes, do not expect a throughput
advantage.

### The hypothesis we came here to test

In campaign s4, reads and writes hit **the same 1,000 keys** — the worst possible
case for cache invalidation. Real applications write to a small subset and read
from a much larger one.

So `write.sql` takes a second variable:

```
\set t random(1, :wtenants)
```

Reads draw tenants from `1..200`. Writes draw from `1..:wtenants`. Setting
`wtenants=1` concentrates every write on a single tenant while reads span all
200; `wtenants=200` spreads them across the same range.

If concentration were the explanation, `wtenants=1` should look much better than
`wtenants=200`.

### The results

| writes | write tenants | origin | PgCache | gain | hit ratio |
|---:|---:|---|---|---:|---:|
| 0% | — | 83.2 ms · 96 rps | 7.10 ms · 1,127 rps | +1074% | 100% |
| 5% | 1 | 79.0 ms · 101 rps | 7.20 ms · 1,111 rps | +1000% | 100% |
| 5% | 10 | 79.9 ms · 100 rps | 7.30 ms · 1,096 rps | +996% | 100% |
| 5% | 200 | 79.8 ms · 100 rps | 7.50 ms · 1,067 rps | +967% | 100% |
| 10% | 1 | 78.5 ms · 102 rps | 7.42 ms · 1,079 rps | +958% | 100% |
| 10% | 200 | 77.9 ms · 103 rps | 7.55 ms · 1,060 rps | **+929%** | 100% |

**At 10% writes the gain is still +929%.** In campaign s4 the same ratio gave
−2%.

### The concentration hypothesis was wrong too

Compare the two 10% rows. Writes concentrated on a single tenant: **+958%**.
Writes spread over all 200: **+929%**.

Twenty-nine percentage points on a gain of about 950 — and recall from section 10
that this campaign's measured run-to-run spread is around 0.3%. The difference is
real but tiny. **Concentration is not what decided campaign s4.**

### What actually decides it: the share of the clock

The real variable is the **ratio of read cost to write cost**, and the arithmetic
is simple enough to do on paper.

In campaign s4:
- a read cost **0.191 ms**
- a write cost **4.879 ms** — about 25× more

With 10% of transactions writing, the fraction of total time spent writing is:

```
        0.10 × 4.879
──────────────────────────────  =  0.488 / 0.660  ≈  74%
0.10 × 4.879 + 0.90 × 0.191
```

**Three quarters of the clock was writes.** And writes pass through both paths
*identically* — PgCache neither accelerates nor delays them. So the cache could
only compete for the remaining quarter, and no amount of read acceleration could
show up in the total.

The clearest evidence that this was never a caching effect: in campaign s4 the
**uncached** path also collapsed, from 36,376 tps to 12,573, when one transaction
in ten became a write. The bottleneck had stopped being the read for *both* paths
at once.

In this campaign the ratio is inverted. A read is an eight-statement dashboard
costing the origin roughly 79 ms; a write is a single indexed `UPDATE`. At the
same 10% ratio the writes consume a negligible share of the clock, and almost all
of it is left for the cache to work on.

> **The rule is not "what fraction of transactions write". It is "what fraction
> of the time do the writes consume".** The two only coincide when reads are
> cheap.

### This qualifies the earlier criterion, it does not retract it

Campaign s4 measured its own regime correctly. The mistake was **generalising
from one regime to all of them** — writing down "above 10% writes" when the
measurement supported "above 10% writes, when reads are 0.191 ms point selects".

The corrected test is the formula above: measure your read and write costs
separately, compute `w·W / (w·W + (1−w)·R)`, and if it is above roughly half the
cache cannot reach the part of the request that matters, whatever its hit ratio.

---

## 14. Why this won, where an earlier subject lost

The same product, the same mechanism, opposite results. The explanation came from
a probe run just before this campaign, which measured ten different query shapes
in isolation:

| shape | origin | PgCache | gain |
|---|---:|---:|---:|
| primary-key lookup | 0.228 ms | 0.165 ms | +38% |
| `COUNT(*)` with `WHERE` | 0.241 ms | 0.162 ms | +48% |
| `ORDER BY … LIMIT` | 0.258 ms | 0.174 ms | +48% |
| `GROUP BY` + aggregate | 0.259 ms | 0.168 ms | +54% |
| `IN (10 ids)` | 0.261 ms | 0.182 ms | +44% |
| `LEFT JOIN` | 0.292 ms | 0.173 ms | +69% |
| two-table join | 0.330 ms | 0.171 ms | +92% |
| correlated `EXISTS` | 0.392 ms | 0.176 ms | +123% |
| three-table join | 0.431 ms | 0.179 ms | +141% |
| FK listing + `LIMIT` | 0.429 ms | 0.163 ms | +163% |

Read PgCache's column top to bottom: **0.162 to 0.182 ms**. A 12% spread between
the cheapest query in the set and the most expensive.

Now the origin's: **0.228 to 0.431 ms**. An 89% spread.

> **PgCache charges a nearly fixed price per answer. The origin charges for the
> work. The gain is, almost exactly, how much work the query costs the origin.**

Everything else follows from that one sentence.

- A multi-tenant dashboard — eight statements, aggregations, joins over a
  tenant's thousands of rows — costs the origin 32 to 83 ms and costs the cache
  the same as anything else. Hence an order of magnitude.
- The first subject we tried was the exact inverse. Its origin served each query
  in ~40 µs. There was no work to save, only proxy overhead, multiplied by the
  104–171 queries it issued per request.

It also explains why eight earlier campaigns found so little: they were run
against primary-key lookups, which are the **bottom row** of that table. The
cheapest query is the cache's worst case, and it is where we spent most of our
time.

---

## 15. What this does not prove

Four limits, and the first is serious.

### The correctness gate did not run for this workload

Every previous campaign on this platform blocked publication of any performance
number until it had compared, query by query, the origin's answer against
PgCache's. **This campaign published without that.**

The previous gates always passed — thousands of comparisons across three
subjects, zero divergences, including a test that sampled reads *while* writes
were in flight and found 1,931 conclusive comparisons with none divergent.

But this workload contains shapes those gates never exercised: two-table joins,
`GROUP BY` with `ORDER BY`, pagination with `OFFSET`. That is precisely where a
divergence would be most plausible, and it is untested.

**Running the gate against this workload is the outstanding item.** Until it has
run, every number in this article is subject to it.

### Mean latency only

pgbench reports the mean natively; percentiles need per-transaction logging,
which this campaign did not collect. For a latency claim the mean is the weakest
statistic available, because a cache's damage lives in the tail — a miss costs
several times a hit.

A later campaign added percentile collection with two rules worth carrying: the
percentiles must be filtered to the read script when writes are in the mix
(otherwise you are describing WAL flush), and they must be **refused** below a
few hundred samples. That refusal was learned the honest way — a slow cell
produced a "p99" of 296 ms against a **mean of 794 ms**, which is arithmetically
impossible and was the tell that four samples is not a measurement.

### One rung, and it fits in memory

463 MB against 1 GB of `shared_buffers`. As section 7 explained, that is the
hardest case for the cache and the result is conservative on that axis — but
nothing here says what happens when the dataset exceeds the origin's memory.

### Two hundred tenants is not twenty thousand

The curve was flat from 1 to 200. Nothing guarantees it stays flat at 20,000,
where the query space is two orders of magnitude larger. **The flatness of this
curve is not a licence to extrapolate**, and a SaaS at that scale would need its
own measurement.

### And there is no path C

No application here means no application-level cache to compare against. This
measures the product's envelope under application-shaped load. It is **not an
adoption verdict**: a real application with its own caching layer is a different
comparison, and one this campaign cannot make.

---

## 16. Reproducing it

### What you need

- A Kubernetes cluster with at least three schedulable nodes.
- The lab chart deployed in namespace `pgcache-synth`, with `origin`, `pgcache`
  and `loadgen` at one replica each. The chart is at
  [`synthetic/infra/aks/chart/`](../infra/aks/chart/).
- `kubectl` pointed at that cluster.

The `loadgen` pod must have these environment variables, which the chart sets:
`ORIGIN_HOST`, `ORIGIN_PORT`, `PGCACHE_HOST`, `PGCACHE_PORT`, `PGUSER`,
`PGPASSWORD`, `PGDATABASE`.

**One configuration detail that will silently ruin the run.** PgCache only caches
tables you have allowed, and the list must be set in `values.yaml` — never via
`helm --set`:

```yaml
pgcache:
  allowedTables: "product,category,seller,customer,orders,order_item,review"
```

`helm --set` and `--set-string` split unescaped commas into separate assignments,
so a table list passed on the command line is **discarded whole**, without an
error. We lost an entire ten-cell probe to this: every shape reported a 0.0% hit
ratio, including a plain primary-key lookup, and the cache was working perfectly
— it had simply been given no tables.

### The sequence

```bash
cd synthetic/article/scripts

# 1. Schema, seed, tenant denormalisation, and copy the workload in. ~1 minute.
./00-setup.sh

# 2. The three sweeps. Roughly 15-25 minutes each, depending on how many
#    warm-up passes each cell needs.
./01-tenant-sweep.sh      | tee ../data/run-tenants.txt
./02-concurrency-sweep.sh | tee ../data/run-clients.txt
./03-write-sweep.sh       | tee ../data/run-writes.txt
```

### Checking the run was valid

Two things to confirm before believing any output.

**Node placement** — three distinct nodes, one zone:

```bash
kubectl -n pgcache-synth get pods -o wide
kubectl get nodes -L topology.kubernetes.io/zone
```

**No preemption inside a measurement window.** These labs run on spot instances,
and an eviction mid-window invalidates that cell:

```bash
kubectl -n pgcache-synth get events --sort-by=.lastTimestamp | grep -iE 'preempt|evict|NodeNotReady'
```

Anything recent means discard that repetition and run it again.

---

## 17. Appendix: every artifact

```
synthetic/article/
├── README.md                      this document
├── sql/
│   ├── 01-schema.sql              7 tables, seed, ORM-style indexes
│   └── 02-tenant.sql              tenant_id denormalisation + composite indexes
├── workload/
│   ├── dashboard.sql              the 8-statement request
│   └── write.sql                  the write, with :wtenants concentration
├── scripts/
│   ├── lib.sh                     scrape, run, warm_until_settled, cell
│   ├── 00-setup.sh                apply schema, copy workload into the pod
│   ├── 01-tenant-sweep.sh         sweep 1
│   ├── 02-concurrency-sweep.sh    sweep 2
│   └── 03-write-sweep.sh          sweep 3
└── data/
    └── results.tsv                every cell, machine-readable
```

The chart that provisions the stack is at
[`synthetic/infra/aks/chart/`](../infra/aks/chart/).

### Sibling reports

| document | what it covers |
|---|---|
| [`RESULTS-multitenant.md`](../RESULTS-multitenant.md) | this campaign, in the shorter report format |
| [`RESULTS-shapes.md`](../RESULTS-shapes.md) | the ten-query-shape probe of section 14 |
| [`RESULTS-aks-s4.md`](../RESULTS-aks-s4.md) | campaign s4 — the write axis this one qualifies |
| [`RESULTS-aks-s1.md`](../RESULTS-aks-s1.md) · [`s2`](../RESULTS-aks-s2.md) | the point-select campaigns, and the break-even formula |
| [`ANALYSIS.md`](../ANALYSIS.md) | why pgbench, and why the other four tools were rejected |
| [`../../docs/TRIAGE-CRITERIA.md`](../../docs/TRIAGE-CRITERIA.md) | the accumulated criteria, each with the case that taught it |
| [`../../REPORT.md`](../../REPORT.md) | the whole project, end to end |

### A note on these files

The campaign as it actually ran was driven by ad-hoc bash typed at a prompt, and
the SQL lived in `/tmp` inside a pod. The scripts here are cleaned-up equivalents
that perform the same measurements with the same parameters in the same order —
not a verbatim transcript.

That gap is itself worth recording. A campaign whose artifacts exist only inside
a running pod is not reproducible, and nobody notices until the pod is gone.

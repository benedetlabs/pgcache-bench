# NetBox — test scenarios

Derived from `STUDY.md` and from what the openFGA campaigns taught us. Written
before any measurement, so the campaign cannot be steered by its own early
results.

## Why this subject, in one paragraph

openFGA failed criterion C9: its origin served each query in ~40 µs, below
PgCache's ~45 µs serving floor, so the experiment compared two memory caches and
the proxy's overhead was all that was left to measure. NetBox is the opposite
shape on the three things that decided that outcome. Its reads are **not**
wrapped in transactions (C2 pass, unlike Strapi). Django 6 + psycopg3 default to
**client-side binding**, so queries arrive at PgCache as complete literal SQL
without anyone forcing `simple_protocol` — which means path B is honestly "path A
plus a cache", and openFGA's central confound is gone. And it has **no
cross-request application cache**, so there is no path C to lose to.

The remaining unknown is C9 itself, and it is the first thing we measure.

---

## Probe 0 — the C9 gate (run this before anything else)

**Question.** Is NetBox's origin cost per query meaningfully above ~45 µs?

**Method.** Seed S0 (1,000 devices). Warm the process. Drive
`GET /api/dcim/devices/?limit=50` and `GET /ipam/prefixes/?per_page=50` against
path A only. Divide end-to-end latency by the measured query count.

**Decision rule, fixed in advance:**

| Result | Verdict |
|---|---|
| < 60 µs/query | **Stop.** Same failure mode as openFGA. Do not spend cluster time. |
| 60–150 µs/query | Continue, but expect a narrow margin and say so up front. |
| > 150 µs/query | Proceed to the full ladder. |

No configuration tuning happens before this passes. Tuning a subject that cannot
win is how we produced nine cells of a conclusion we later had to retract.

---

## The three paths

| Path | Name | What it isolates |
|---|---|---|
| **A** | `baseline` | Real datastore cost, no cache |
| **B** | `pgcache` | Gain attributable only to PgCache |
| ~~C~~ | — | **Does not exist.** NetBox removed its object cache years ago; its Redis holds only the config revision and the plugin catalogue, and `context_managers.py` is per-request memoization. This is an A-vs-B study. |

Losing path C costs us the honest competitor, and every report must say so:
"PgCache helps or it doesn't" is a weaker claim than "PgCache beats the
alternative you already have".

---

## Workloads

Chosen for what they do to *shape space*, which is what actually limits the
cache. Amplification figures are measured, from `STUDY.md` §2.

| ID | Endpoint | Queries/req | Shape structure | What it tests |
|---|---|---:|---|---|
| **N1** | `GET /api/dcim/devices/?limit=50` | 28 | 22 distinct, 20 prefetch round trips | The primary API path. 100% cacheable under token auth. |
| **N2** | `GET /ipam/prefixes/?per_page=50` (UI) | 120 | **100 of 2 repeated shapes** | The cache's best case in the whole subject. |
| **N3** | `GET /search/?q=…` | 360 | **350 of 7 repeated shapes** | Extreme amplification over a tiny shape set. |
| **N4** | `GET /api/dcim/devices/<id>/` | 27 | 21 distinct | Detail read; nearly as expensive as a 50-row list. |
| **N5** | `GET /api/dcim/devices/?limit=50&brief=true` | 9 | 9 distinct | Control: the prefetch list collapses. If PgCache wins here too, the gain is not about prefetch amplification. |

**Key distributions**, applied to the object id / filter value:

- `zipf` (α=1.1) over 1% of keys — the primary workload. Its working set at S3
  is ~95 MB and stays resident *even at S3*; the ladder alone does not produce a
  knee under skew.
- `wide` — hot fraction widened to 20% (~1.9 GB hot set at S3).
- `uniform` — no locality. The control, and the workload the S3 acceptance test
  uses to prove the buffer pool was actually exceeded.

Every report must say which distribution produced the number it shows. That is
D-23, learned the hard way: r5 reported W1/W2 under `zipf` and W3 under
`uniform` in one table, with nothing distinguishing them.

---

## Correctness gate

Runs before any timing, and **aborts the campaign** on divergence — not prints
and continues, which is what the openFGA runner did until we fixed it.

1. **Differential.** Same request to path A and path B, compare normalised JSON
   bodies. NetBox's API responses are deterministic given a fixed dataset and
   ordering, which makes this the primary check here — on openFGA the oracle was
   primary because the answer is a single boolean.
2. **Analytical invariants.** Seed-derived facts that must hold in any response:
   device counts per site, interface counts per device, prefix containment.
   Catches a seed that loaded wrong, which no differential check can see.
3. **Staleness probe.** Write through path A, poll path B, record the lag until
   the change is visible. PgCache is CDC-coherent, so this should be
   sub-second — and that number is the whole argument for it over a TTL cache.

---

## Scale ladder

Per `STUDY.md` §6.4. Sizes are estimates; the acceptance test decides.

| Rung | Devices | Est. on-disk | vs `shared_buffers` (1 GB) |
|---|---:|---:|---|
| S0 | 1,000 | ~10 MB | smoke only |
| S1 | 20,000 | ~190 MB | ~0.2× — fits |
| S2 | 200,000 | ~1.9 GB | ~2× — the crossing point |
| S3 | 1,000,000 | ~9.5 GB | ~10× — the knee |

**The acceptance test is not optional.** A rung counts as "exceeded the buffer
pool" only when, after `pg_stat_reset()` and ten minutes of `uniform` load
against path A, `blks_hit / (blks_hit + blks_read)` comes in **below 0.95**. On
openFGA we reported a 556 MB dataset that was 23 MB of data and 525 MB of index
bloat left behind by `DELETE`-and-reload, and never noticed the origin was
serving at a 99.93% buffer hit rate. Size queries lie; the miss ratio does not.

The seed uses `TRUNCATE`, never `DELETE`, for exactly that reason.

**The OS page cache is a confound and must be bounded.** Without a memory ceiling
on the origin container, Linux serves the whole dataset from page cache and the
acceptance test passes for the wrong reason. But note the boundary we set for
ourselves after r8: the ceiling exists to make a rung *honest*, not to
manufacture I/O until the cache looks good. If NetBox needs a strangled origin to
show a win, NetBox is the wrong subject too — and we stop, rather than tune.

---

## What would make us abandon this subject

Stated now, so it cannot be rationalised away later:

- The C9 probe returns < 60 µs/query.
- Cache hit ratio stays below ~50% at S1 under `zipf` — the shape space is too
  wide for the cache to hold it, and no rung fixes that.
- The differential gate diverges. A cache that returns a wrong device list is not
  a performance question.

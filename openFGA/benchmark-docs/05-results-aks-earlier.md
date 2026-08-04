# What We Found Running PgCache on AKS

> **Provenance — read this first.**
> This document covers the **earlier AKS campaigns** (the r2/r3 partials and the
> phase 3-4 run), which stopped before completing. It is kept for the material
> that does not depend on a full campaign: the captured SQL, the `bare` vs `cni`
> network measurement, and the record of what we believed at the time.
>
> **Do not compare its tables with the current results.** Those campaigns ran
> with defects that were open at the time — notably D-15, D-17, the incomplete
> cold-restart of the path-B server, and D-18 — and several conclusions here have
> since been retracted. In particular, the claim that PgCache's penalty is
> *bounded* on dedicated hardware is **wrong**: it holds only at 40 rps, and at
> 160 rps the penalty is 9.9×.
>
> The current result is [05-results-aks-r5.md](05-results-aks-r5.md) — one
> campaign, run start to finish, one document.

We spent two days benchmarking PgCache in front of OpenFGA's PostgreSQL — first
on a laptop, then on a Kubernetes cluster. This is what came out, what we got
wrong along the way, and what we still don't know.

The short version: **PgCache never returned a wrong answer, and it never won on
latency.** Both halves matter, and the second is less damning than it sounds once
you see why.

---

## Why we moved to Kubernetes at all

The laptop results were embarrassing for PgCache — a 9.4× worse p99, and total
collapse at the larger dataset. We didn't trust them, for a good reason: the
laptop was a bad place to run this. Eight vCPUs shared between Postgres, the
cache, three copies of OpenFGA, the load generator, and whatever else the machine
was doing. When we finally looked, we found a Keycloak and a crash-looping CDC
connector eating CPU during our measurement windows.

So the question we brought to AKS was narrow: *does PgCache still lose when
nothing is competing with it?*

Yes — but by much less. On dedicated nodes the penalty dropped from 9.4× to about
2–3×, and the cache hit ratio climbed from the 70s into the low 90s. Same code,
same data, same workloads. That gap between environments is the most portable
lesson here: **if you benchmark a proxy on a busy machine, you are measuring the
machine.**

---

## Was it correct?

Yes, without qualification.

Before timing anything, every campaign runs a differential check: fire the same
authorization question at the baseline and at the PgCache path, compare the
answers to each other *and* to an oracle that derives the right answer from first
principles without touching the database. Any disagreement aborts the run.

Across every seeding, both network modes, and both dataset sizes: **4,000 pairs
per gate, 100.000000% agreement, zero divergences, zero transport errors.**

For an authorization system this is the result that actually matters. A cache
that returns a stale "allowed" isn't a slow system — it's a security incident.
PgCache is semantically transparent here, and that finding is independent of
every performance number below.

---

## The numbers

All at 40 requests/second against a 40 rps target, 60-second windows,
Zipf-distributed access (α=1.1), on dedicated `Standard_D8as_v5` nodes — one pod
per node, nothing sharing.

### Small dataset (E1, ~85k tuples)

| | p50 | p95 | p99 | queries/req | cache hits |
|---|---:|---:|---:|---:|---:|
| **A** baseline | 4.09 ms | 8.92 | **12.55** | 109 | — |
| **B** PgCache | 5.35 | 19.66 | **25.72** | 112 | 90.5% |
| **C** OpenFGA's own cache | 3.41 | 10.57 | **15.69** | 68 | — |

### Larger dataset (E2, ~1M tuples)

| | p50 | p95 | p99 | queries/req | cache hits |
|---|---:|---:|---:|---:|---:|
| **A** baseline | 4.27 | 11.48 | **26.62** | 137 | — |
| **B** PgCache | 6.03 | 33.66 | **52.20** | 133 | 86.3% |
| **C** OpenFGA's own cache | 3.92 | 14.69 | **37.46** | 98 | — |

Two things worth noticing.

**Nothing fell over.** On the laptop, the PgCache path at this dataset size was
dropping a third to half of the offered load — it simply couldn't keep up. Here
all three paths held 40 rps with zero errors. Dedicated hardware turned a
collapse into a bounded, measurable penalty. That's a real improvement, even
though the ranking didn't change.

**The trend still runs the wrong way for PgCache.** Going from 85k to 1M tuples,
the hit ratio slipped from 90% to 86% and the penalty on deny-heavy workloads
grew from roughly 3× to 4.6×. More data means more distinct query shapes, and
each new shape costs something to register. We'll come back to that.

---

## Does Kubernetes networking cost anything?

This is the one question only a cluster could answer, and we designed the chart
around it. A single value flips the whole lab between two modes:

- **`bare`** — pods use the host network, Services are headless, so a connection
  goes straight node to node. No kube-proxy, no NAT, no overlay. Effectively
  "what you'd get on VMs."
- **`cni`** — normal pod networking and ClusterIP Services. What your production
  cluster actually looks like.

Everything else is identical. The difference between the two modes *is* the cost
of the Kubernetes network layer, measured under a workload issuing 110–125
database queries per request — which should amplify any per-hop overhead
a hundredfold.

| Workload | Path | `bare` p99 | `cni` p99 | difference |
|---|---|---:|---:|---:|
| W1 (hot) | baseline | 12.55 ms | 13.29 ms | +0.74 |
| | PgCache | 25.72 | 24.02 | −1.70 |
| | app cache | 15.69 | 16.30 | +0.61 |
| W3 (deny) | baseline | 9.54 | 10.28 | +0.74 |
| | PgCache | 30.65 | 25.17 | −5.48 |
| | app cache | 10.43 | 10.95 | +0.52 |

**About three quarters of a millisecond.** On the two stable paths, in both
workloads, the CNI costs +0.5 to +0.8 ms at p99 — consistent enough to look like
signal rather than noise, and small enough not to matter for most people.

The PgCache row moves the *other* way, which we read as rep-to-rep variance
rather than a finding: that path restarts with a cold cache before every
repetition, so its spread is far wider than the others.

If you expected the overlay to compound with query amplification into something
dramatic — we did — it doesn't, at least not at 40 rps. It might at saturation.
We didn't get to measure that.

---

## The workload that surprised us

W2 uses uniform random access instead of Zipf — no locality at all. On the laptop
it was the cache's worst case by a mile: 0% hit ratio, multi-second p99, exactly
what you'd predict for a cache asked for something different every time.

On AKS, at the small dataset, W2 produced **the highest hit ratio of the entire
campaign: 94.1%.**

The explanation is mundane once you see it, and it changes how we'll read this
workload from now on. At 85k tuples the *entire working set fits in the cache*.
When everything fits, access pattern stops mattering — you hit no matter what you
ask for. W2 doesn't measure locality; it measures whether your cache is big
enough to hold the data. At this scale it wasn't testing what we thought it was.

We'd expect the old behaviour to return at E3, where the data is far larger than
the cache. Worth confirming.

---

## What the database actually saw

We captured SQL directly from the origin's `pg_stat_statements` — 40 `Check`
calls through each path, counters reset in between.

OpenFGA's read path is four query shapes, all boring in the best way:
single-table lookups with equality predicates. No joins, no CTEs, no views. This
is exactly why we picked OpenFGA as the first subject — it's the complement of
PgCache's "can't cache this" list.

```sql
-- ReadUserTuple · 218 executions · covers the primary key
SELECT object_type, object_id, relation, _user, condition_name, condition_context
  FROM tuple
 WHERE _user = $1 AND object_id = $2 AND object_type = $3
   AND relation = $4 AND store = $5 AND user_type = $6

-- ReadUsersetTuples · 201 executions · returns ~130 rows
SELECT store, object_type, object_id, relation, _user, condition_name,
       condition_context, ulid, inserted_at
  FROM tuple
 WHERE store = $1 AND user_type = $2 AND object_type = $3
   AND object_id = $4 AND relation = $5 AND (_user LIKE $6)

-- Read, the tuple-to-userset iterator · 109 executions
SELECT store, object_type, object_id, relation, _user, condition_name,
       condition_context, ulid, inserted_at
  FROM tuple
 WHERE store = $1 AND object_type = $2 AND object_id = $3 AND relation = $4
```

Roughly 580 statements for 40 authorization checks. That ratio — about 110 to 1 —
is what this whole lab is built around. OpenFGA resolves the permission graph in
Go, not SQL, and does no batching: every tuple it finds spawns another lookup.

### What PgCache adds

Run the same 40 checks through the cache and a second family of statements
appears at the origin. These are PgCache's own, issued to populate its cache:

```sql
-- PgCache shape-registration query · 217 executions
SELECT DISTINCT public.tuple.store, public.tuple.object_type,
       public.tuple.object_id, public.tuple.relation, public.tuple._user,
       public.tuple.user_type, public.tuple.ulid, public.tuple.inserted_at,
       public.tuple.condition_name, public.tuple.condition_context
  FROM public.tuple
 WHERE public.tuple._user = $1 AND public.tuple.object_id = $2
   AND public.tuple.object_type = $3 AND public.tuple.relation = $4 AND ...
```

Look at the pairing: 227 pass-through `ReadUserTuple` against 217 registration
queries. 192 `ReadUsersetTuples` against 172. Nearly one to one.

**On a miss, the origin does two queries instead of one** — the original, plus a
fully-qualified `SELECT DISTINCT` so the cache can learn the shape. That's the
same event we measure from the proxy side as ~20–33 ms of registration latency.
From the database it's simply double the work.

This is the mechanism behind everything else here. It also explains the tail: the
median request is fine, but any request touching a shape the cache hasn't seen
pays the registration cost — and with 110 queries per request, the odds of
touching at least one new shape are high.

### A trap in this data

The per-statement means look *better* through PgCache:

| Statement | Path A | Path B |
|---|---:|---:|
| `ReadUserTuple` | 0.659 ms | 0.142 ms |
| `ReadUsersetTuples` | 0.767 ms | 0.028 ms |
| `Read` (TTU iterator) | 0.952 ms | 0.049 ms |

Don't quote that. Path A ran first against a cold buffer cache — 122 to 201 pages
read from disk per shape. Path B ran minutes later, fully warm. Two captures at
different times are not a controlled comparison; the campaign's own A/B numbers,
measured in the same window at the same rate, are.

What the table *does* show honestly is the scale of what PgCache is trying to
beat: a warm origin serves these in **tens of microseconds**. The round trip
PgCache saves is smaller than the round trip it adds to its own embedded
PostgreSQL. That's the whole story — and it's why we now think the interesting
experiment is one where the origin is genuinely far away.

---

## The honest competitor

We always run a third path: OpenFGA's own in-process cache, which ships disabled.
Including it keeps the question meaningful — "does caching help?" is trivially
yes; "which cache" has a real answer.

It cuts query amplification roughly in half (109 → 68 on W1, 124 → 71 on W3).
Less work reaching the database, unambiguously.

And its p99 is still *worse* than doing nothing: 15.69 ms against the baseline's
12.55, and 37.46 against 26.62 at the larger dataset. Less work, longer tail.

At these dataset sizes, with the origin comfortably holding everything in
`shared_buffers`, **neither cache pays for itself.** The baseline is very hard to
beat when a database query costs 30 microseconds. That's not a criticism of
either cache — it's a statement about the regime we tested in.

---

## Laptop vs cluster, side by side

| | Laptop (shared) | AKS (dedicated) |
|---|---|---|
| PgCache p99 penalty, E1/W3 | 9.4× (43 → 301 ms) | 3.2× (9.5 → 31 ms) |
| PgCache at 1M tuples | collapsed, 33–53% dropped | sustained 40 rps |
| PgCache hit ratio, E1 | 76–82% | 90–94% |
| Correctness | 100% agreement | 100% agreement |

Identical software and data. The environment doesn't flip the conclusion, but it
moves the magnitude by 3×. Any number published from this lab has to carry its
environment with it — which is why every run writes a `versions.txt` with image
digests, VM SKUs, zone, disk IOPS and measured RTT.

---

## What we don't know

The gaps are the interesting part, so we're explicit about them.

**Where each path saturates.** The ramp phase never completed in any of the
campaigns below. This was later measured in campaign r5 — see
[05-results-aks-r5.md](05-results-aks-r5.md), which retracts the "bounded
penalty" claim these campaigns made.

**A remote origin.** Every measurement so far has the database one network hop
away, answering in microseconds. The condition where PgCache *should* win — the
one its own design implies — is an origin that's genuinely distant, where the
saved round trip is milliseconds and gets multiplied by 110 queries per request.
We haven't built that yet, and we think it's the next experiment worth running.

**E3 and beyond.** The shape-space problem should get worse with scale, and the
"W2 fits in cache" artifact should disappear. Both are predictions, not
measurements.

**One experimental flag we couldn't turn off.** OpenFGA ships with
`pipeline_list_objects` enabled and we could not disable it in Kubernetes — the
flag syntax that works in Docker Compose is ignored as a container argument, and
setting it via environment variable duplicated the value instead of clearing it.
It's identical across all three paths, so A/B/C comparisons hold. But it makes
cross-environment comparison with the laptop imprecise, and any future
`ListObjects` workload needs this caveat attached.

---

## Reading the fine print

**Repetition counts are uneven.** The protocol calls for three repetitions per
combination; some path-B cells have one or two, because that path restarts its
cache before every repetition and was the one running when nodes disappeared. A
median over one sample is just that sample.

**Some raw data was lost.** The load generator originally stored results in
ephemeral pod storage, and twice a Spot node deallocation took a finished phase
with it — 27 runs the first time. We recovered the summary numbers from the
campaign log, which records each run as it completes, but the per-request latency
files are gone for those runs. They're reported here with that provenance noted.
The fix — persistent volume, generator as a Deployment — is in place now and
verified: the volume survived the next pod recreation intact.

**`pg_stat_statements` wasn't installed.** The Helm chart loaded the library but
never created the extension — a step the Docker Compose setup did in an init
script we didn't port. We created it by hand for the SQL capture above. It
doesn't affect any campaign number, because query amplification comes from
OpenFGA's own metrics, but it's fixed in the chart now.

Every defect we found in our own benchmark — including the ones that invalidated
earlier published numbers — is written up in [06-defect-log.md](06-defect-log.md).
There are twenty-three. Publishing them is the only thing that makes the rest of
this trustworthy.

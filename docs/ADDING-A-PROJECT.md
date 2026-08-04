# Adding a Project

How to bring a new open source application onto the platform. The order is
deliberate: **the API study comes before any code**, because it can (and
should) disqualify bad subjects cheaply.

Time budget from the openFGA experience: study ~1 day, integration ~2-4 days,
first valid campaign ~1 day.

---

## Phase 1 — the API study (`STUDY.md`)

Copy `_template/STUDY.md` into the new project folder and answer every section.
This is the document that made the openFGA lab work: its `PLAN.md` §1 audited
the storage adapter *before* a single container ran.

### 1.1 Audit the SQL the app actually emits

Read the app's storage/persistence layer — not the docs, the code. For each
read path, catalog the SQL. You are classifying against PgCache's
non-cacheable list:

| Not cacheable | Why it matters |
|---|---|
| Views, RLS | proxy can't track invalidation |
| Tables without a PK | CDC can't identify rows |
| `LATERAL`, recursive CTEs | shape complexity |
| `FULL`/`CROSS JOIN` | ditto |
| Locking clauses (`FOR UPDATE`) | write path, by design |
| Volatile functions (`NOW()`, `random()`) in predicates | same SQL, different answer |

Two verdicts to extract:

- **Cacheable fraction of the read path.** OpenFGA: 100% of reads are
  single-table equality SELECTs — ideal. An app built on views and window
  functions is a poor subject; better to know in an afternoon.
- **The volatile exceptions.** OpenFGA had exactly one (`ReadChanges`
  interpolates `NOW()`), excluded via `ALLOWED_TABLES`. Find yours; every one
  needs an exclusion and a note.

Practical shortcut: run the app locally against Postgres with
`pg_stat_statements` on, drive it by hand, and read the statements table. Code
tells you intent; `pg_stat_statements` tells you truth.

### 1.2 Measure query amplification

How many datastore queries does one user-facing operation cost? This is the
lever of the whole benchmark: at 200 queries/request, 0.3 ms of per-query
overhead is 60 ms per request. OpenFGA: 100-480. A CRUD app at 1-3
queries/request will barely register a signal either way — that is a finding,
not a failure, but know it going in.

Prefer the app's own metric (OpenFGA exposes `datastore_query_count`).
Otherwise: `pg_stat_statements` delta divided by request count.

### 1.3 Find the native cache — path C

Does the app ship its own caching? What coherence model (TTL? invalidation?)?
Is it on by default? OpenFGA ships three cache flags, all off by default,
TTL-coherent — which made the platform question sharp: CDC coherence vs TTL
coherence. If there is no native cache, the comparison is A vs B only; say so.

### 1.4 Design the correctness check

In order of preference:

1. **Analytical oracle** — data generated structurally so any answer is
   derivable without querying. Needs a data model you can synthesize.
2. **Differential** — same requests to paths A and B, byte-compare answers.
   Always possible; weaker (both could be wrong the same way).
3. **None** — then the lab measures only speed, and every report says so.

Also identify the *staleness probe*: a write followed by polling reads through
path B, measuring how long until the new value appears. That is CDC lag versus
the native cache's TTL — the platform's central trade-off, measured.

### 1.5 Inventory the knobs that break comparability

Every app has defaults that silently differ between paths. OpenFGA had four:
an experimental-features flag that is NOT empty by default, a 3 s request
timeout that turns saturation into timeouts, a 30-connection pool ceiling, and
statement caching in the driver (`pgx`) that makes PgCache classify 99.97% of
traffic as non-cacheable unless switched to `simple_protocol`. Find the
equivalents. Each must be pinned identically across paths A, B and C.

### 1.6 Seeding at scale

The API will be too slow for realistic mass (OpenFGA's API caps writes at 100
tuples/call; 10M tuples would be ~100k calls). Find the direct-SQL path:
schema, `COPY` targets, and what post-load steps the app needs (`ANALYZE`,
migrations, cache resets). Define a scale ladder — small smoke rung up to
"does not fit in `shared_buffers`", because the interesting knee is where the
origin stops serving everything from RAM.

## Phase 2 — integration

> Run `/integrate <subject>` to have the integration agent team do this. It
> gates on the R1/R1b protocol spike first, freezes `CONTRACT.md`, then builds
> the four artifacts below in parallel against it. See `docs/AGENT-TEAM.md`.


1. **Copy `_template/`** to `<project>/`, keep `STUDY.md` at its root.
2. **Chart.** Start from `openFGA/infra/aks/chart/`. Reusable nearly verbatim:
   `origin` (StatefulSet + Premium SSD v2 StorageClass), `pgcache`, `loadgen`,
   `monitoring`, placement helpers, the `bare|cni` network switch. What you
   rewrite: the app Deployments (one per path, generated from a single
   template so paths cannot drift apart — see `templates/openfga.yaml` for the
   pattern), the migrate Job, the ConfigMap.
   With `hostNetwork`, every port must be distinct per path — including ones
   you forgot exist (gRPC bit openFGA).
3. **Workload generator.** Open-loop, Zipf keys, drop accounting, adaptive
   warm-up, per-run artifacts (`summary.json`, `latencies.csv`,
   before/after `/metrics` dumps). `openFGA/tools/cmd/fgabench` is the
   reference; the HTTP client and workload mix are what change.
4. **Campaign runner.** Adapt `openFGA/scripts/campaign-aks.sh`: seed →
   validate → differential gate → matrix of (workload × path × repetition),
   cold PgCache per path-B rep, contamination check per run, **snapshot per
   phase**.

## Phase 3 — first campaign

> Run `/smoke <subject>` for the first three steps below — the agent team runs
> them and stops at the correctness gate. Everything past that costs real
> cluster time and is invoked by hand.

Run in this order, each step gating the next:

```
smoke rung, path A only        does the app even run against the origin?
smoke rung, path B             does it run through PgCache? (protocol issues show here)
correctness gate               100.000000% or stop
smallest real rung, 3 reps     first comparable numbers
scale ladder                   until a path stops sustaining the rate
staleness probe                CDC lag vs native cache TTL
```

Publish in `<project>/benchmark-docs/` per the METHODOLOGY deliverables — and
keep the defect log honest. The openFGA lab found 16 defects in itself; yours
will have its own.

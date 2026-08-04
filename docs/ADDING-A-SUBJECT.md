# Adding a Subject — the API study

Before writing any code, produce a study document (`<subject>/PLAN.md`) that
answers everything below. The OpenFGA study (`openFGA/PLAN.md` §1) is the
reference: it was accurate enough that the benchmark's defects were all in the
measurement harness, never in the subject analysis.

## 1. Storage adapter audit

Read the subject's Postgres adapter source. Produce the table:

| Storage method | SQL emitted | Cacheable? |
|---|---|---|
| ... | full shape with placeholders | ✅ / ❌ + reason |

You are looking for the **PgCache non-cacheable list**: views, RLS, tables
without PK, `LATERAL`, recursive CTEs, `FULL`/`CROSS JOIN`, lock clauses,
volatile functions. A subject whose hot read path emits none of these is a good
subject. One that emits them everywhere dilutes the signal.

Flag every **non-deterministic read** — `NOW()` interpolated as text, random
ordering, session state. These must be excluded via `ALLOWED_TABLES`, and the
exclusion documented, or the correctness gate becomes unauditable.

## 2. Driver and protocol

Which driver, which protocol mode? `pgx` in extended mode with statement cache
makes ~99.97% of queries invisible to PgCache; `simple_protocol` fixes that but
interpolates literals, exploding the shape space with dataset size — and it is a
**confound** (path B becomes "path A without prepared statements, plus a
cache"). Record the mode chosen and the confound explicitly.

## 3. Query amplification

How many datastore queries does one API request cost? OpenFGA: 100–480 per
`Check`, because graph resolution happens in Go with no batching. Amplification
is the lever that turns small per-query overhead into collapse — a subject with
amplification ~1 will show almost no signal either way. Find the subject's own
metric for it (`datastore_query_count` histogram or equivalent); if none exists,
plan to derive it from `pg_stat_statements` deltas.

## 4. The honest competitor (path C)

What caching does the subject already ship? Flags, TTLs, coverage, defaults.
OpenFGA: check/iterator cache, TTL 10 s, disabled by default. If the subject has
no cache of its own, path C can be a standard sidecar (e.g. app-level look-aside
cache) — but say so explicitly, because the comparison changes meaning.

## 5. Seed strategy and the oracle

- Can the dataset be bulk-loaded via `COPY` behind the API's back? What
  invariants must be preserved (changelog tables, ULIDs, checksums)?
- **Can correctness be verified without asking the system under test?** Design
  the dataset structurally so the right answer is derivable from parameters
  alone. This is the hardest requirement and the one that makes the benchmark
  mean something. Test the oracle against brute force before trusting it.
- Post-seed: `VACUUM ANALYZE`, restart PgCache (it must never serve the previous
  dataset), verify a sample through the API.

## 6. Workloads

Define at minimum: a hot workload (Zipf locality on **every** axis the cache
keys on — the reference subject shipped with one axis accidentally uniform, see
defect D-01), a cold/uniform control, an adversarial worst case, a mixed
read/write workload for CDC-lag measurement, and the differential correctness
workload.

## 7. Metrics endpoints

Where do the subject and PgCache expose Prometheus metrics? The generator
snapshots both before and after the measurement window; the delta is the source
of truth, not the live scrape.

## 8. Deliverables checklist

```
<subject>/
├── PLAN.md               the study above + hypotheses + threats to validity
├── README.md             quick tour
├── docs/RUNBOOK.md       operational: run, read, invalidate
├── model/ or schema/     the dataset definition
├── tools/                generator + oracle + seeder (following openFGA/tools)
├── scripts/              campaign runner emitting the standard CSV schema
└── infra/aks/chart/      Helm chart: 3 paths, placement, networkMode, monitoring
```

Standard CSV columns (append-only; header auto-rewrites on schema change):

```
run_id,path,path_name,rung,workload,rate_target,rate_achieved,requests,errors,
dropped,allowed,denied,p50_ms,p90_ms,p95_ms,p99_ms,p999_ms,max_ms,mean_ms,
queries_per_request,pgcache_hit_ratio,consistency,started_at,ended_at,
doc_dist,warmup_s
```

Dates RFC3339 UTC. `report.py` and the validity rules consume these columns and
need no per-subject changes.

## 9. Before publishing anything

Run the validity checklist in `openFGA/docs/RUNBOOK.md` §8. Keep a defect log
from day one (`benchmark-docs/06-defect-log.md` format) — the first subject
accumulated 16, and the ones that hurt were the ones found *after* publishing.

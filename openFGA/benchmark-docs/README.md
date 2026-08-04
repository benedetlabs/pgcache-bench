# PgCache × OpenFGA — Benchmark Documentation

**BenedetLabs** · lab `pgcache/openFGA`

This folder is the record of a benchmark campaign measuring whether a
**database-level read cache coherent via CDC** (PgCache) beats an
**application-level cache coherent via TTL** (OpenFGA's built-in in-process
cache) — in latency *and* in correctness.

Everything here is in English for external review. The source repository and its
inline comments are in Portuguese.

---

## Index

| Document | Contents |
|---|---|
| [01-test-architecture.md](01-test-architecture.md) | What is under test, the three paths, the authorization model, the analytical oracle, the scale ladder, the workloads |
| [02-infrastructure.md](02-infrastructure.md) | Both environments (local Docker Compose and AKS), node placement, resource sizing, Azure quota constraints, cost controls |
| [03-methodology-and-validity.md](03-methodology-and-validity.md) | Measurement protocol, correctness gate, aggregation rules, known confounds, validity checklist |
| [04-prior-results-local.md](04-prior-results-local.md) | The two local campaigns, including the retracted one and why it was retracted |
| [05-results-aks-r5.md](05-results-aks-r5.md) | **Current AKS result** — campaign `r5`, 2026-08-03, the first one to run start to finish. One document, one campaign. |
| [05-results-aks-earlier.md](05-results-aks-earlier.md) | The earlier AKS campaigns, which stopped short. Kept for the captured SQL and the `bare` vs `cni` measurement; several of its conclusions are retracted by r5. |
| [06-defect-log.md](06-defect-log.md) | Every defect found in the benchmark itself, with evidence |
| [07-pgcache-configuration.md](07-pgcache-configuration.md) | **Configuration probe** — every campaign so far ran PgCache stock and under-warmed. Pinned tables and the hit-ratio knee, measured. |
| [08-results-aks-r6.md](08-results-aks-r6.md) | **Current result** — campaign `r6`, PgCache with pinned tables and warmed to the knee. 39/39 runs. Retracts r5's headline. |
| `runs/` | Raw campaign logs and Kubernetes event dumps |
| `results-aks/` | Raw per-run artifacts extracted from the cluster |

---

## The short version

The question is narrow on purpose:

> Does a cache **at the database level**, coherent by change data capture, beat a
> cache **at the application level**, coherent by TTL?

OpenFGA is the test subject because every one of its read paths is a single-table
`SELECT` with equality predicates — no CTEs, joins, `LATERAL`, or views — and
because it resolves the authorization graph in Go rather than in SQL. One `Check`
therefore becomes **100 to 480 small, repetitive datastore queries**. That is the
workload profile where a coherent read cache should shine, and where the effect
can be measured without ambiguity.

Three paths run against the same origin, with the same mass, in the same window,
at the same target rate:

| Path | Name | What it isolates |
|---|---|---|
| **A** | `baseline` | Real datastore cost, no cache at all |
| **B** | `pgcache` | Gain attributable **only** to PgCache |
| **C** | `appcache` | The honest competitor — OpenFGA's own cache, which ships disabled |

---

## Status

**One document per campaign.** Results files are named for the campaign that
produced them. We do not merge campaigns into a shared table: they run with
different code and different fixes, and a median across them corresponds to no
experiment that was ever performed. This is not a style preference — it is defect
D-18, which silently did exactly that and invalidated published aggregates.

The current result is **campaign r6** (2026-08-03): 39 of 39 runs, zero request
errors, PgCache configured with `PINNED_TABLES` and warmed to the hit-ratio knee.
Campaign r5 is the same lab with PgCache left stock and warmed for a fixed 45 s —
kept as the contrast, not as the headline.

**Correctness: PgCache is transparent.** Across every campaign, on every rung,
the differential gate (W7) has reported 100.000000% agreement with zero
divergences. In r5, rung E1: 4,000 pairs compared, zero divergences, zero
transport errors. For an authorization system this is the result that matters
most — a wrong cached answer is a security incident, not a performance metric.

**Performance: it depends almost entirely on configuration.** Stock and
under-warmed (r5), the p99 penalty over an uncached origin was 1.9×–2.8× at 40 rps
and 9.9× at 160 rps. With pinned tables and a warm cache (r6) the same conditions
give 1.09×–1.35× at 40 rps and **1.05× at 160 rps**, and PgCache beats OpenFGA's
own cache in four of seven comparable conditions. At 320 rps both PgCache and the
uncached origin collapse while the in-process cache holds.

The cost is cold start: the hit-ratio knee moves from 2m30s stock to 5–7+ minutes
with pinning. And note that the cache hit ratio moves *opposite* to latency here —
the mechanism is cost of miss × ~110 queries per request, not cost of hit.

---

## How to read the numbers

Three rules, each learned by getting it wrong first. They are detailed in
[03-methodology-and-validity.md](03-methodology-and-validity.md):

1. **Check query amplification before latency.** If `queries_per_request`
   differs between two paths, they did not perform the same amount of work.
2. **A run with >10% drop ratio has no valid percentiles.** Drops are not
   random — they remove exactly the requests that would have been slowest.
3. **Never aggregate runs from different target rates or different workload
   distributions.** The resulting "median" corresponds to no condition that was
   ever executed.

---

## Reproducing

The full operational procedure is in the source repository:

- `PLAN.md` — experimental design: hypotheses, threats to validity, protocol
- `docs/RUNBOOK.md` — how to run, what each metric means, when a number is invalid
- `docs/PLANO-EXECUCAO-AKS.md` — AKS-specific execution steps
- `scripts/campaign-aks.sh` — the autonomous campaign runner used here
- `infra/aks/chart/` — Helm chart for the cluster deployment

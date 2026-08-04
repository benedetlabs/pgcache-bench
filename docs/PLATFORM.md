# PgCache Test Platform

A standard for benchmarking [PgCache](https://pgcache.com) against real
open-source applications, on Kubernetes. Each application is a **subject**,
living in its own folder; this document is the contract every subject follows.

Written in English because results are shared with an external engineering team.

---

## The question every subject answers

> Does a cache **at the database level**, coherent via CDC, beat the caching
> strategy the application already ships with — in latency *and* correctness?

Correctness comes first. For most subjects a wrong cached answer is a bug; for
some (authorization, billing) it is an incident. A subject that cannot verify
correctness independently of the system under test is not a valid subject.

## The three-path topology

Every subject runs three paths against the **same origin, same data, same
window, same target rate**:

| Path | | What it isolates |
|---|---|---|
| **A** | baseline | Real datastore cost, no cache |
| **B** | pgcache | Gain attributable only to PgCache |
| **C** | app cache | The honest competitor — whatever the app itself offers |

Path C is **preferred, not mandatory** — it is scoping, not a gate. With it,
the question is *which* cache, which has the more interesting publishable
answer. Without it, the campaign asks the narrower "does PgCache beat a bare
origin", and every report must say so explicitly.

**Amended 2026-08-03.** This document previously called path C mandatory. The
first `/scout` sweep forced the issue: no candidate combined a strong native
cache with a cacheable read path. The one that did — Keycloak, with Infinispan
— was disqualified on C2, and the best remaining subject (NetBox) has no native
cache at all. Holding the mandate would have emptied the shortlist.

It is also no longer the sharper question. openFGA ran a full three-path
campaign and PgCache lost, so "which cache" already has one answer on record.
What is open is whether PgCache beats a bare origin *anywhere* — and that is an
A-vs-B question. `docs/TRIAGE-CRITERIA.md` C6 is the operative rule; this
paragraph exists so nobody re-derives the old one from a stale sentence.

## Measurement rules (non-negotiable)

Learned the hard way on the first subject; see
`openFGA/benchmark-docs/06-defect-log.md` for the 16 defects behind these rules.

1. **One path under load at a time.** Parallel measurement corrupts, it does not
   accelerate.
2. **Open-loop generator.** Latency measured from intended arrival time.
   Overflow is counted as drops; **a run with >10% drops has no valid
   percentiles** — drops remove exactly the slowest requests.
3. **Check work before latency.** If per-request datastore query counts differ
   between paths, they did not do the same work.
4. **Never aggregate across target rates, distributions, or network modes.**
   Group key = (rung, workload, rate, distribution, network mode, path).
5. **Correctness gate before any timing.** Differential comparison A vs B against
   an independent oracle. Divergence aborts the campaign.
6. **Cold cache per repetition.** Restart PgCache before every path-B rep — and
   after every re-seed (the cache must never serve a previous dataset).
7. **Requests only, no CPU limits** in pod specs. CPU limits are CFS quota;
   throttling shows up as p99 spikes indistinguishable from cache-miss cost.
8. **One pod per node** for origin, PgCache and the app, via anti-affinity on
   `lab.benedet/role` (remember: a pod *without* the label is repelled by all).
9. **Results are extracted before anything recreates a pod.** They live in
   emptyDir; a deleted pod is a deleted campaign.
10. **Every environment parameter travels with the numbers**: image digests,
    SKUs, zones, disk IOPS, measured RTT, network mode.

## Repository layout

```
pgcache/
├── docs/
│   ├── PLATFORM.md            <- this file
│   └── ADDING-A-SUBJECT.md    <- the API study checklist
├── openFGA/                   <- subject #1, the reference implementation
│   ├── PLAN.md                   experimental design (hypotheses, threats)
│   ├── docs/RUNBOOK.md           how to run, how to read, when numbers are invalid
│   ├── infra/aks/chart/          Helm chart (three paths + monitoring)
│   ├── scripts/campaign-aks.sh   autonomous campaign runner
│   ├── tools/                    generator + oracle + seeder (subject-specific)
│   └── benchmark-docs/           English results for external review
└── <next-subject>/            <- same shape
```

A subject folder is complete when it has: a PLAN (why this subject, hypotheses,
threats), a Helm chart following the three-path pattern, a load generator with
drop accounting, an independent correctness oracle, a seeder, and a campaign
runner producing the standard CSV schema.

## What you inherit from the reference implementation

Copy from `openFGA/`, adapting only what the subject demands:

- **Chart skeleton** — `_helpers.tpl` (placement, networkMode, labels),
  `monitoring.yaml` (Prometheus+Grafana on the system pool, never on lab nodes),
  `storageclass.yaml`, the migrate-job pattern.
- **`report.py`** — aggregation and validity rules operate on the standard CSV
  columns, not on subject specifics.
- **`campaign-aks.sh`** — phases, contamination detection, per-phase snapshots.
- **Documentation discipline** — the defect log format, the errata practice, the
  validity checklist.

## What each subject must build (the expensive part)

- The **oracle**: a way to know the correct answer without asking the system
  under test. For OpenFGA this was a structurally-defined dataset where any
  authorization decision is derivable in O(depth). Budget most of the effort
  here.
- The **client + workloads**: hot/cold/adversarial mixes with controlled
  locality (Zipf), speaking the subject's API.
- The **cacheability map**: which of the subject's queries PgCache can serve,
  which are volatile, which protocol quirks apply (see ADDING-A-SUBJECT.md).

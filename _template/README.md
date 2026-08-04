# <PROJECT NAME> — PgCache lab

Benchmark of PgCache in front of <app>'s PostgreSQL, following the platform
standard in [`../docs/METHODOLOGY.md`](../docs/METHODOLOGY.md).

**Status:** <study | integrating | first campaign | published>

## Layout (fill in as it materializes)

```
STUDY.md               the API study — done FIRST, see ../docs/ADDING-A-PROJECT.md
PLAN.md                experimental design: hypotheses, threats to validity
infra/aks/chart/       Helm chart (start from ../openFGA/infra/aks/chart/)
tools/                 workload generator + correctness oracle
scripts/               seed + campaign runner
benchmark-docs/        results in English + the defect log
```

## The three paths

| Path | Datastore | App cache | Isolates |
|---|---|---|---|
| A `baseline` | origin direct | off | real datastore cost |
| B `pgcache` | via PgCache | off | gain attributable only to PgCache |
| C `appcache` | origin direct | on | the honest competitor (if the app has one) |

## Quick start

```bash
# 1. study first — no code before it is answered
$EDITOR STUDY.md

# 2. deploy (after adapting the chart)
helm install lab ./infra/aks/chart -n pgcache-lab-<project> --create-namespace

# 3. campaign
./scripts/campaign-aks.sh all
```

## Non-negotiables (from the platform standard)

- Correctness gate before any performance number
- One path under load at a time; same origin/data/window/rate across paths
- Drop ratio > 10% voids percentiles
- Snapshot results after every phase — they live in an emptyDir
- Publish the benchmark's own defects in `benchmark-docs/`

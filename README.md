# PgCache Test Platform

A platform for benchmarking [PgCache](https://pgcache.com) against real open
source applications, on Kubernetes.

The question every project here answers, with its own workload:

> Does a **database-level read cache, coherent via CDC**, beat the application's
> own caching (or no caching) — in latency *and* in correctness?

**Start with [REPORT.md](REPORT.md)** — the narrative account of every campaign so
far, written to be read end to end. The per-campaign reports linked from it hold
the raw numbers.

Runs on Kubernetes deliberately: local runs starve for resources and produce
numbers that blame the machine, not the cache. Every lab here learned that the
hard way — see the openFGA defect log.

---

## Structure

```
pgcache/
├── README.md                  this file
├── docs/
│   ├── METHODOLOGY.md         the standard every project MUST follow
│   ├── ADDING-A-PROJECT.md    the API study + integration steps
│   ├── AGENT-TEAM.md          the agent teams — scout (/scout), discovery
│   │                          (/discover), integration (/integrate, /smoke,
│   │                          /stack-review)
│   ├── TRIAGE-CRITERIA.md     learned disqualifiers, each with the case that
│   │                          taught it — read by the scout
│   └── CANDIDATES.md          ranked shortlist with verdicts and evidence
├── _template/                 skeleton for a new project
│   ├── README.md
│   └── STUDY.md               the API study template (fill this in FIRST)
└── <project>/                 one folder per subject application
    ├── STUDY.md               why this app, its SQL profile, cacheability
    ├── PLAN.md                experimental design for this app
    ├── infra/<k8s>/chart/     Helm chart: origin + pgcache + app paths + loadgen
    ├── tools/                 workload generator + correctness oracle (app-specific)
    ├── scripts/               seed / campaign runners
    └── benchmark-docs/        results, in English, self-contained
```

## Projects

| Project | Status | What it exercises |
|---|---|---|
| [openFGA](openFGA/) | **reference implementation** | 100-480 single-table equality SELECTs per request; graph resolution in Go; the ideal-on-paper profile for a read cache |

The openFGA lab is the template in practice: its chart, campaign runner,
methodology and defect log are what `docs/` distills. When this README and the
openFGA lab disagree, the openFGA lab is what actually ran.

## Adding a project

Short version — the full procedure is
[docs/ADDING-A-PROJECT.md](docs/ADDING-A-PROJECT.md):

1. Copy `_template/` to `<project>/` and **fill in `STUDY.md` first**. The API
   study decides whether the project is even a valid subject — before any code.
2. Adapt the Helm chart from `openFGA/infra/aks/chart/` (origin, PgCache and
   loadgen carry over nearly unchanged; the app paths are what you rewrite).
3. Build the workload generator and, if feasible, a correctness oracle.
4. Run the campaign following `docs/METHODOLOGY.md`. Correctness gate before any
   performance number.
5. Publish results in `<project>/benchmark-docs/`, in English, including a
   defect log for the benchmark itself.

## The two results that matter, per project

1. **Correctness.** Does PgCache ever return a different answer than the origin?
   For openFGA: 100.000000% agreement, zero divergences, across every campaign.
2. **Latency, honestly measured.** Same origin, same data, same window, same
   target rate across all paths; percentiles refused when drop ratio exceeds
   10%; amplification checked before latency.

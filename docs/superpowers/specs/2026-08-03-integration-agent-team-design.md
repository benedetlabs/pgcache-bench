# Design — Integration Agent Team

**Date:** 2026-08-03
**Status:** approved (design), pending implementation plan
**Scope:** a second local agent team for the PgCache benchmark platform, covering
`docs/ADDING-A-PROJECT.md` Phase 2 (integration) and the non-paid part of Phase 3
(smoke + correctness gate). Complements the existing discovery team.

---

## 1. Problem

The platform has a discovery team (`/discover`) that produces `<subject>/STUDY.md`
and a viability verdict. Everything after that — chart, workload generator,
seeder, oracle, campaign runner — is hand-built. The openFGA lab is the only
worked example, and reproducing it for a new subject is weeks of work that
currently has no tooling and no gates.

This design adds a team that builds that stack from an approved `STUDY.md`, and
proves the stack works before any money is spent on cluster time.

## 2. Boundary: what agents do and do not do

Agents **build the stack and prove it runs**: they write the chart, generator,
seeder, oracle, scenarios and runner, then execute the S0 smoke rung and the
differential correctness gate.

Agents **do not run the paid campaign**. The scale ladder, the repetitions, and
the hours of cluster time stay a human decision, invoked by hand.

This supersedes the current text in `docs/AGENT-TEAM.md:52-57`, which says
integration is not agent work at all. The reasoning behind that text still holds
— the openFGA lab logged 16 defects, most of them at integration and measurement
time — but the conclusion changes: defects at integration are exactly why the
agent that builds the stack must also be forced to run it. A builder that never
executes its own artifact hands the defects to the human instead of finding
them. What stays protected is the expensive, long-running part.

`docs/AGENT-TEAM.md` must be rewritten to cover both teams and to state this
boundary explicitly.

## 3. Architecture: contract first, then parallel builders

Phase 2's four artifacts are tightly coupled. The chart's per-path ports must
match the generator's targets; scenario names must match the generator's flags;
`summary.json`'s fields must match what the report aggregator reads. Building
them in parallel without a fixed interface is how drift is manufactured — and
the openFGA lab's own defects (a forgotten gRPC port, a `DISK_LIMIT` that
silently produced 100% miss) are of exactly this class.

So: one serial step writes an **integration contract**, then four builders work
against it concurrently.

```
STUDY.md
   │
   ├─ step 0: R1/R1b protocol spike ──── R1 fails? STOP. Subject disqualified.
   │
   ├─ stack-contract ──► CONTRACT.md + scenarios/*.yaml
   │                        │
   │        ┌───────────────┼───────────────┬────────────────┐
   │        ▼               ▼               ▼                ▼
   │   chart-builder   bench-builder   seed-builder   runner-builder
   │        └───────────────┴───────────────┴────────────────┘
   │                        │
   │                  stack-reviewer  ──── REJECTED? one retry, then STOP.
   │                        │
   │                  smoke-operator
   │                        │
   └──────────────────► SMOKE-REPORT.md ──► human decides on the campaign
```

Discovery parallelizes because reading code contaminates nothing. Integration
parallelizes only *after* the interface is frozen, and measurement does not
parallelize at all.

## 4. Roster

**What this spec delivers is the team, not any subject's stack.** The
implementation output is seven agent definitions in `.claude/agents/`, three
command definitions in `.claude/commands/`, and the documentation changes in
§10. The Go generator, Helm chart, seeder and runner described below are what
those agents will *later* produce when `/integrate` is run against a subject —
they are requirements on the agent prompts, not files this work writes.

| Agent | Produces | Runs |
|---|---|---|
| `stack-contract` | `CONTRACT.md`, `scenarios/*.yaml`, the spike harness | serial, first |
| `stack-chart-builder` | Helm chart + local `docker-compose.yml` | parallel wave |
| `stack-bench-builder` | Go workload generator + its tests | parallel wave |
| `stack-seed-builder` | direct-SQL seeder, oracle, seed validation | parallel wave |
| `stack-runner-builder` | campaign runner + report aggregator | parallel wave |
| `stack-reviewer` | findings + APPROVED / APPROVED WITH NOTES / REJECTED | gate |
| `smoke-operator` | `SMOKE-REPORT.md` with evidence | last; only agent that executes load |

### 4.1 `stack-contract`

Reads `STUDY.md` §4.4 (workloads), §5 (knobs), §6 (seeding), §7 (ports). Emits:

- **`<subject>/CONTRACT.md`** — per-path ports and database endpoints; every
  pinned env var with its value; the generator's CLI flag set; the
  `scenarios/*.yaml` schema; the `summary.json` schema; the artifact paths each
  run writes. This is the interface the four builders compile against.
- **`<subject>/scenarios/*.yaml`** — one file per scenario derived from §4.4
  (for strapi: W1, W1L, W1a, W2, W2q, W3, W4). Each carries rung, path, target
  rate, key distribution, auth mode, abort criteria, and whether it may be
  pooled into aggregates.
- **the spike harness** — a minimal compose file (origin + PgCache) plus a
  node-postgres script that issues a parameterized query and a read inside
  `BEGIN…COMMIT`, resolving R1 and R1b.

### 4.2 `stack-chart-builder`

Starts from `openFGA/infra/aks/chart/`. Reusable near-verbatim: `origin`
StatefulSet, `pgcache`, `loadgen`, `monitoring`, placement helpers, the
`bare|cni` network switch. Rewrites: the app Deployments (one per path,
generated from a single template so paths cannot drift), the migrate Job, the
ConfigMap. Also produces the local `docker-compose.yml` — the cheap rung before
cluster time.

Every port distinct per path under `hostNetwork`, taken from `CONTRACT.md`.

### 4.3 `stack-bench-builder`

Go, following `openFGA/tools/cmd/fgabench`. Open-loop and target-rate driven;
Zipf key generation with a locality regression test; drop accounting; adaptive
warm-up to the hit-rate knee; per-run artifacts (`summary.json`,
`latencies.csv`, before/after `/metrics` dumps). Loads scenarios from
`scenarios/*.yaml` rather than hardcoding them. What changes per subject is the
HTTP client and the workload mix.

The Zipf locality regression test is mandatory, not optional: in the openFGA lab
a locality bug produced zero cache hits by design and went unnoticed.

### 4.4 `stack-seed-builder`

Direct SQL only — the boot-then-dump pattern from `STUDY.md` §6: boot the app
once against an empty origin, dump `information_schema`, derive COPY targets
from the dump rather than asserting them, then COPY. Also implements the
analytical oracle, the seed-validation sampler (path A only), and the invariant
assertions the study specifies.

### 4.5 `stack-runner-builder`

Adapts `openFGA/scripts/campaign-aks.sh`: seed → validate → differential gate →
matrix of (scenario × path × repetition), cold PgCache per path-B repetition,
contamination check per run, snapshot per phase. Plus the report aggregator
(`report.py` equivalent) reading the `summary.json` schema from the contract.

The runner is written by an agent but **invoked by a human** for anything beyond
S0.

### 4.6 `stack-reviewer`

Adversarial gate over the built stack, same discipline as `study-reviewer`:
spot-checks that the chart's ports match `CONTRACT.md`, that the generator
actually implements drop accounting and adaptive warm-up rather than claiming
to, that the runner enforces the abort rules, and that nothing contradicts
`docs/METHODOLOGY.md` §4 validity rules. Ends with an explicit verdict.

### 4.7 `smoke-operator`

The only agent permitted to execute load. Runs the Phase 3 ladder up to, and
stopping at, the correctness gate:

1. S0 rung, path A only — does the app run against the origin?
2. S0 rung, path B — does it run through PgCache? (protocol issues surface here)
3. differential correctness gate — 100.000000% or stop

Produces `<subject>/benchmark-docs/SMOKE-REPORT.md` with evidence, and hard-stops
before the scale ladder.

## 5. Command surface

| Command | Does |
|---|---|
| `/integrate <subject>` | spike → contract → builders → reviewer. Builds; does not run load. |
| `/smoke <subject>` | runs the S0 ladder and the correctness gate; produces the smoke report. |
| `/stack-review <subject>` | standalone re-review of an existing stack. |

There is deliberately **no `/campaign`**. The paid campaign is run by hand with
`<subject>/scripts/campaign-aks.sh`.

## 6. Gates

Every gate stops the pipeline. None of them warn and continue.

| Gate | Condition | Action |
|---|---|---|
| Protocol spike (R1) | PgCache cannot parse the extended query protocol | stop; report disqualification; build nothing |
| Transaction spike (R1b) | PgCache cannot serve reads inside `BEGIN…COMMIT` | do **not** stop automatically — record the cacheable ceiling this implies and require explicit human confirmation before building, since the subject may still be worth measuring |
| `stack-reviewer` | REJECTED | re-run only the rejected artifacts, one retry, then stop |
| Correctness | differential agreement < 100.000000% | stop; publish no numbers |
| Drop ratio | > 10% | report "did not sustain the rate"; never a p99 |
| Amplification | per-request statement count differs A vs B | comparison void regardless of latency |

## 7. Artifacts per subject

```
<subject>/
  STUDY.md            existing, from /discover
  CONTRACT.md         new — the integration source of truth
  scenarios/*.yaml    one per scenario
  docker-compose.yml  local rung
  infra/aks/chart/    kubernetes
  tools/cmd/*bench/   generator + Go tests
  tools/cmd/*seed/    seeder + oracle
  scripts/            runner + report aggregator
  benchmark-docs/     SMOKE-REPORT.md + defect log
```

## 8. Invariants

- Agents never modify `_sources/` — it stays scratch, read-only, uncommitted.
- Agents never run the paid campaign.
- Every builder works against `CONTRACT.md`; a builder that needs something not
  in the contract stops and asks rather than inventing it.
- The subject is never assumed: the team is subject-agnostic and reads
  `<subject>/STUDY.md`. Strapi is the first subject it will be exercised on.
- Existing files are never overwritten without asking, matching `/discover`.

## 9. Testing strategy

The smoke ladder is the integration test — a stack that cannot complete S0 on
both paths and close the correctness gate is not built, regardless of how good
the code looks.

Below that, the generator carries real Go unit tests: Zipf locality regression,
drop accounting arithmetic, scenario YAML loading, and warm-up knee detection.
These exist because each corresponds to a defect the openFGA lab actually
shipped.

## 10. Documentation changes

- `docs/AGENT-TEAM.md` — rewritten to cover both teams and to state the
  build-and-prove / humans-spend-money boundary, replacing the current
  "integration is not agent work" section.
- `docs/ADDING-A-PROJECT.md` — Phase 2 and Phase 3 gain pointers to
  `/integrate` and `/smoke`.
- `README.md` — command list updated.

## 11. Out of scope

- Running or reporting the paid campaign.
- Provisioning the AKS cluster (`infra/aks/cluster.sh` already exists and is
  human-run).
- Any change to the discovery team's agents or to `/discover`.
- Path C (native app cache) tooling — no current subject has one.

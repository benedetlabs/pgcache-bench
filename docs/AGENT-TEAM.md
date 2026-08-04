# The Agent Teams

The platform ships two teams of local Claude Code agents (defined in
`.claude/agents/`). Open a session at the platform root and they are available
automatically.

- **Scout** finds candidate subjects and triages them against learned
  disqualifiers.
- **Discovery** studies a candidate subject and produces a viability verdict.
- **Integration** builds that subject's test stack and proves it runs.

They are separate teams because they obey opposite rules: discovery
parallelizes freely, integration only after its interface is frozen, and
measurement does not parallelize at all.

## Usage

```
cd /Users/leonardo.benedet/BenedetLabs/pgcache
claude
> /scout headless CMS                                  # Phase 0 — find
> /discover strapi https://github.com/strapi/strapi   # Phase 1 — study
> /integrate strapi                                    # Phase 2 — build
> /smoke strapi                                        # Phase 3 — prove
```

Then, by hand, when you decide the numbers are worth paying for:

```bash
strapi/scripts/campaign-aks.sh
```

---

## Team 0 — Scout

Decides *which* app is worth a day of study. One agent, read-only.

| Agent | Produces | Runs |
|---|---|---|
| `subject-scout` | `docs/CANDIDATES.md` — ranked shortlist with verdicts and evidence | two phases, alone |

```
/scout ─ phase 1 metadata sweep (dozens) ─ phase 2 shallow clone (~5) ─► CANDIDATES.md
```

Command: `/scout [domain or hint]`.

**The scout does not know the disqualifiers — it reads them** from
`docs/TRIAGE-CRITERIA.md`. That indirection is the point: disqualifiers are
knowledge the platform accumulates from real results, and each criterion there
carries the case that taught it. When a spike kills a subject, the lesson
becomes a criterion, and every future sweep inherits it. Strapi taught C2
(plain reads wrapped in transactions) on 2026-08-03 and is now the scout's
known-answer regression case: a triage that passes Strapi is broken.

## Team 1 — Discovery

Produces `<subject>/STUDY.md` and a viability verdict. Read-only on the
subject's source.

| Agent | Produces | Runs |
|---|---|---|
| `subject-sql-auditor` | STUDY §1-§2 — SQL catalog, cacheability, amplification | parallel wave |
| `subject-cache-hunter` | STUDY §3, §5, §7 — native cache, knobs, ports | parallel wave |
| `subject-oracle-designer` | STUDY §4, §6 — correctness check, staleness probe, workloads, seeding | after the auditor |
| `study-reviewer` | findings + APPROVED/REJECTED verdict | last, as a gate |

```
            ┌─ subject-sql-auditor ──┐
prepare ────┤                        ├─ oracle-designer ─ assemble ─ study-reviewer
            └─ subject-cache-hunter ─┘
```

Command: `/discover <subject> [git-url]`.

## Team 2 — Integration

Builds the stack from an approved study, then proves it works. Produces
`CONTRACT.md`, `scenarios/*.yaml`, the chart, the generator, the seeder, the
runner, and `SMOKE-REPORT.md`.

| Agent | Produces | Runs |
|---|---|---|
| `stack-contract` | `CONTRACT.md`, `scenarios/*.yaml`, the spike harness | serial, first |
| `stack-chart-builder` | Helm chart + local `docker-compose.yml` | parallel wave |
| `stack-bench-builder` | Go workload generator + its tests | parallel wave |
| `stack-seed-builder` | direct-SQL seeder, oracle, seed validation | parallel wave |
| `stack-runner-builder` | campaign runner + report aggregator | parallel wave |
| `stack-reviewer` | findings + APPROVED/REJECTED verdict | gate |
| `smoke-operator` | `SMOKE-REPORT.md` with evidence | last; only agent that runs load |

```
STUDY.md ─ spike (R1/R1b) ─ stack-contract ─┬─ chart-builder ──┐
                                            ├─ bench-builder ──┤
                                            ├─ seed-builder ───┼─ stack-reviewer ─ smoke-operator
                                            └─ runner-builder ─┘
```

Commands: `/integrate <subject>`, `/smoke <subject>`, `/stack-review <subject>`.

**Why a contract first.** Phase 2's artifacts are tightly coupled: the chart's
per-path ports must match the generator's targets, scenario names must match
the generator's flags, and `summary.json`'s fields must match what the report
aggregator reads. Building them in parallel without a frozen interface is how
drift is manufactured — and the reference lab's own defects (a forgotten gRPC
port, a `DISK_LIMIT` that silently produced 100% miss) are exactly of that
class.

---

## Design principles

- **Discovery parallelizes; integration parallelizes late; measurement never.**
  Reading code concurrently contaminates nothing. Building coupled artifacts
  concurrently is safe only after the contract exists. Load runs one path at a
  time, always.
- **Evidence or it did not happen.** Every claim carries `file:line`. Both
  reviewers spot-check citations and fail wrong ones. This rule exists because
  this platform's own agents caught four wrong numbers in its own
  documentation by auditing artifacts instead of trusting prose.
- **Disqualification is success.** The study exists to kill bad subjects in a
  day; the spike gate exists to kill them in an hour. An app whose driver
  speaks a protocol PgCache cannot parse is a fine thing to discover *before*
  writing a chart and a load generator.
- **Correctness precedes performance.** No latency number is published from a
  run whose differential gate did not reach 100.000000% agreement. This is a
  refusal, not a warning.
- **Read-only on the subject.** Clones live in `_sources/` (scratch, never
  committed) and are never modified.

## The boundary: agents build and prove, humans spend money

Agents write the stack **and run it** — the S0 smoke rung and the correctness
gate. `smoke-operator` is the only agent permitted to execute load, and it
hard-stops at the correctness gate.

Agents do **not** run the paid campaign: no scale ladder, no publication
repetitions, no staleness campaign. Those are hours of cluster time and stay a
human decision, invoked by hand.

This boundary was moved deliberately. An earlier version of this document said
integration was not agent work at all, reasoning that the reference lab logged
16 defects and most appeared at integration and measurement time. That fact
still holds, but it argues the opposite conclusion: defects concentrate at
integration precisely because the person building the stack was not the one
running it. A builder that never executes its own artifact hands the defects
downstream. What genuinely needs protecting is the expensive, long-running
part — and that is what stayed protected.

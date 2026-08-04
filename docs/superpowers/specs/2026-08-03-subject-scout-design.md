# Design — Subject Scout

**Date:** 2026-08-03
**Status:** approved (design), pending implementation
**Scope:** an agent that *finds* candidate subjects for the PgCache platform,
upstream of `/discover`. Complements the discovery and integration teams.

---

## 1. Problem

The platform can study a subject (`/discover`) and build its stack
(`/integrate`), but choosing *which* subject to study has always been manual.
That choice is the highest-leverage decision in the whole pipeline: a bad
subject costs a day of discovery at best, and — before the spike gate existed —
weeks at worst.

The Strapi run on 2026-08-03 changed the economics. It produced a *cheap triage
test*: Strapi was disqualified because its ORM wraps every plain read in a
transaction and PgCache passes through in-transaction reads. That is a
`grep`-able property. Knowing it, most unsuitable candidates can now be killed
in minutes instead of days — but only if something remembers the rule and
applies it.

## 2. The core idea: disqualifiers are knowledge that accumulates

Strapi taught the platform one disqualifier. Future spikes will teach more.

If those rules live inside an agent's prompt, learning costs a prompt edit and
the knowledge is invisible. If they live in a versioned criteria file the agent
*reads*, the platform accumulates institutional memory and every rule carries
the case that taught it.

So the scout does not know the rules. It consults them.

This closes a loop: `/scout` → `/discover` → `/integrate` (spike) → whatever
the spike teaches goes back into the criteria file.

## 3. Components

| Component | Role |
|---|---|
| `docs/TRIAGE-CRITERIA.md` | learned disqualifiers, each with the case that taught it and its cheap test |
| `.claude/agents/subject-scout.md` | runs both triage phases, produces the shortlist |
| `.claude/commands/scout.md` | `/scout [domain or hint]` |
| `docs/CANDIDATES.md` | the living, ranked shortlist with a verdict per candidate |

## 4. Flow — hybrid triage

```
/scout [optional hint]
   │
   ├─ phase 1 — metadata only (cheap, dozens of candidates)
   │    Postgres as primary store? project alive? licence compatible?
   │    plausible scale? identifiable ORM or query layer?
   │    └─► ~5 survive
   │
   ├─ phase 2 — shallow clone, finalists only
   │    apply docs/TRIAGE-CRITERIA.md with file:line evidence
   │    the question that kills: are plain reads wrapped in transactions?
   │
   └─► docs/CANDIDATES.md — ranked, with verdict and evidence
```

Phase 1 is deliberately cheap because most candidates die there and cloning
them would be waste. Phase 2 is deliberately expensive because the question it
answers cannot be answered from metadata — and Strapi proved that guessing at
that question costs a month.

## 5. Output — three verdicts, not two

Per candidate:

- **Promising** — worth spending `/discover` on, with the reasons.
- **Disqualified** — with the exact criterion that killed it and the
  `file:line` evidence.
- **Uncertain** — what remains unknown, and what it would cost to find out.

The third state is load-bearing. A two-state verdict pushes the agent to fake
certainty; "uncertain, and here is the cheapest way to resolve it" is a useful
answer and an honest one.

**Ranking is by cost-to-resolve, not by attractiveness.** A good candidate
whose decisive question is a five-minute grep outranks an excellent one whose
decisive question needs a day — because the platform's scarce resource is
verification effort, not candidates.

## 6. Criteria file format

Each criterion carries:

- **The test** — concretely how to check it, ideally a grep or a manifest read.
- **Cost** — metadata-only, or requires a clone.
- **Severity** — fatal (disqualifies alone) or demoting (worsens the ranking).
- **Origin** — the case that taught it, with a date and a pointer to the
  evidence.

The first entry already exists and is real:

> **Plain reads wrapped in transactions** — fatal, requires clone. Taught by
> Strapi on 2026-08-03 (`strapi/spike/out/result.json`): PgCache passes through
> every read inside `BEGIN…COMMIT`, so an ORM that wraps plain reads makes the
> app's whole read body uncacheable. Test: find where the ORM's read methods
> are defined and check for a transaction wrapper around them.

A second entry, also real and now *cleared*: extended query protocol support
was a blocking unknown until the same spike proved PgCache handles it. That is
recorded as resolved rather than deleted, so nobody re-litigates it.

## 7. Rules

- **Read-only.** Clones go in `_sources/` and are never modified. The scout
  does not write a `STUDY.md`, does not run a spike, and builds nothing.
- **Disqualification is success** — the same principle as the rest of the
  platform.
- **Evidence where it decides.** `file:line` is mandatory in phase 2; phase 1
  claims carry a link and a quotation from the source.
- **Never invent a candidate.** If the sweep found nothing that passes, say so.
  An empty shortlist is a valid result.
- **Never overwrite `docs/CANDIDATES.md` silently** — a candidate already
  marked disqualified stays disqualified with its evidence unless a criterion
  changed, in which case say which one.

## 8. Testing strategy

The scout's output is a judgement, so it cannot be unit-tested. What can be
checked:

- Run it once and verify every phase-2 claim's `file:line` resolves to what it
  says. This is the same spot-check discipline the two reviewers apply.
- Verify the Strapi criterion actually fires: given Strapi as a candidate, the
  scout must disqualify it on the transaction criterion, citing
  `document-service/common.ts`. Strapi is now a known-answer regression case —
  a scout that passes Strapi is broken.

## 9. Documentation changes

- `docs/AGENT-TEAM.md` — add the scout upstream of the discovery team and show
  the full `/scout → /discover → /integrate → /smoke` chain.
- `README.md` — command list.

## 10. Out of scope

- Automating the criteria file's updates. A spike result becomes a criterion by
  human judgement; the loop is deliberate, not automatic.
- Ranking by business value, popularity, or strategic fit. The scout ranks by
  verification cost and cacheability evidence only.
- Any change to `/discover`, `/integrate` or `/smoke`.

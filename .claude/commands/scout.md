Find candidate subjects for the platform and triage them. Optional argument:
`$ARGUMENTS` — a domain, a hint, or an explicit candidate list (e.g.
`CMS and headless commerce`, or `medusa, cal.com, plausible`).

This runs **upstream of `/discover`**. It answers "which app is worth a day of
study", not "what does this app do".

## Pipeline

1. **Launch `subject-scout`.** Pass it the hint if there was one. It runs two
   phases:
   - **Phase 1, metadata** — wide sweep, cheap. Kills candidates on Postgres
     primacy, project viability, licence. Cuts to ~5 finalists.
   - **Phase 2, shallow clone** — finalists only. Applies the clone-cost
     criteria from `docs/TRIAGE-CRITERIA.md` with `file:line` evidence,
     starting with the one that most often decides it: does the data layer
     wrap plain reads in a transaction?

2. **Report to the user**: the shortlist ranked by cost-to-resolve, each entry
   with its verdict (promising / disqualified / uncertain), the criterion that
   decided it, and the evidence. Then the recommended next command, which is
   normally `/discover <winner> <url>`. Portuguese for the chat summary;
   `docs/CANDIDATES.md` stays in English.

## Rules

- Never modify `_sources/`. Clones are scratch.
- **This command discovers and triages. It does not study.** No `STUDY.md`, no
  spike, no build. Those are `/discover` and `/integrate`.
- **An empty shortlist is a valid result.** If nothing survives triage, say so
  rather than promoting the least-bad candidate.
- **Never silently overwrite a verdict already in `docs/CANDIDATES.md`.** If a
  criterion changed and a past verdict no longer holds, name the criterion and
  explain the reversal.
- If web search is unavailable, report that plainly and offer phase 2 alone
  against a user-supplied list. Do not present a triage as a sweep.
- When the scout reports that a criterion seems missing or wrong, surface it to
  the user — `docs/TRIAGE-CRITERIA.md` is updated by human judgement, not by
  the agent.

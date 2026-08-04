---
description: Run the discovery agent team on a new candidate subject (e.g. /discover strapi https://github.com/strapi/strapi)
---

Run the PgCache platform discovery pipeline for the subject given in the
arguments: `$ARGUMENTS` (subject name, optionally followed by a git URL).

## Pipeline

Discovery is read-only analysis, so the independent parts run in PARALLEL —
unlike measurement campaigns, which are strictly sequential. Do not conflate
the two.

1. **Prepare.**
   - `mkdir -p <subject>/` from `_template/` (copy `STUDY.md` and `README.md`,
     substitute the subject name). Skip files that already exist — never
     overwrite a study in progress.
   - Shallow-clone the source into `_sources/<subject>/` if a URL was given
     and the clone does not exist (`git clone --depth 1`). `_sources/` is
     scratch: never committed, never modified.

2. **Parallel wave** — launch BOTH at once as subagents, background:
   - `subject-sql-auditor`: audit `_sources/<subject>/`, produce STUDY §1-§2.
   - `subject-cache-hunter`: same source, produce STUDY §3, §5, §7.

3. **Dependent step** — when the auditor returns, launch
   `subject-oracle-designer` with the auditor's findings included in its
   prompt. Produces §4, §6 and the workload sketch.

4. **Assemble.** Merge the three outputs into `<subject>/STUDY.md`, keeping
   the template's section order. Where agents disagree, keep both claims and
   mark the conflict — do not silently pick one.

5. **Review gate.** Launch `study-reviewer` on the assembled study. If
   REJECTED, re-run only the rejected sections with the reviewer's findings in
   the prompt (one retry; after that, stop and report).

6. **Report to the user**, in this order: the reviewer's verdict, the
   subject's viability verdict (good/marginal/disqualified), the three most
   load-bearing findings with their evidence, and what Phase 2 (integration)
   would cost if approved. Portuguese for the chat summary; the STUDY.md
   itself stays in English.

## Rules

- Never modify `_sources/`. Never overwrite an existing non-template
  `STUDY.md` without asking.
- A disqualification is a valid, successful outcome — say it plainly.
- If the subject does not use Postgres as primary store, stop at step 1 and
  report that: it is out of scope for the platform.

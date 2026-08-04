Prove the built stack works for the subject named in the arguments:
`$ARGUMENTS` (a subject name, e.g. `strapi`). This is the non-paid part of
`docs/ADDING-A-PROJECT.md` Phase 3.

Requires `/integrate <subject>` to have completed and `stack-reviewer` to have
passed the stack. If `<subject>/CONTRACT.md` or the built artifacts are
missing, stop and say so.

## Pipeline

1. **Launch `smoke-operator`** with the subject. It runs the ladder, in order,
   each step gating the next:

   ```
   S0 rung, path A only     does the app run against the origin?
   S0 rung, path B          does it run through PgCache?
   seed validation          oracle vs path A, byte-compared
   differential gate        path A vs path B, 100.000000% or stop
   ```

   Local `docker-compose` first — the cheap rung, where config mistakes should
   surface. Then the same S0 pass on the Kubernetes chart, so the chart itself
   is exercised before a campaign depends on it.

2. **Report to the user**, in this order: whether each rung passed, the
   correctness figure to full precision, per-path amplification and whether it
   matched, every defect found and how it was diagnosed, and an explicit
   readiness statement. Portuguese for the chat summary;
   `benchmark-docs/SMOKE-REPORT.md` stays in English.

## Rules

- **This command stops at the correctness gate.** No scale ladder, no
  publication repetitions, no staleness campaign. Those cost real cluster time
  and are the user's decision, run by hand with `<subject>/scripts/`.
- **A failed smoke is a successful outcome.** It found something before the
  campaign did. Report it plainly, with the evidence, and do not soften it.
- **Never publish a latency number from a run whose correctness gate did not
  pass.** Correctness precedes performance, always.
- Do not patch the stack to make a rung pass. Report the defect; fixing is
  the builders' job on a re-run of `/integrate`.
- Tear down whatever was started.

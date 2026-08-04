Build the test stack for the subject named in the arguments: `$ARGUMENTS`
(a subject name, e.g. `strapi`). This is `docs/ADDING-A-PROJECT.md` Phase 2.

Requires `<subject>/STUDY.md` to exist and to carry a viability verdict that is
not "disqualified". If it does not exist, stop and tell the user to run
`/discover <subject>` first.

## Pipeline

Integration is NOT like discovery. Discovery parallelizes freely because
reading code contaminates nothing. Here, the artifacts are coupled, so a
contract is frozen first and only then do builders run concurrently.

1. **Spike gate.** Have `stack-contract` write the R1/R1b spike harness first,
   then run it.
   - **R1 — extended query protocol.** If PgCache cannot parse it, STOP.
     Report the subject as disqualified and build nothing. Most drivers offer
     no simple-protocol fallback, so this is fatal.
   - **R1b — reads inside `BEGIN…COMMIT`.** If PgCache cannot serve them, do
     NOT stop automatically. Compute the cacheable ceiling this implies from
     STUDY §1, present it to the user, and **ask whether to continue** before
     building anything. The subject may still be worth measuring, but that is
     their call, not yours.

2. **Contract.** `stack-contract` produces `<subject>/CONTRACT.md` and
   `<subject>/scenarios/*.yaml`. Serial — nothing else starts until it lands.
   If it reports contract gaps, surface them to the user before continuing;
   a gap filled by a guessing builder is a lost campaign.

3. **Builder wave** — launch all four at once, in the background:
   - `stack-chart-builder` → Helm chart + `docker-compose.yml`
   - `stack-bench-builder` → Go generator + tests
   - `stack-seed-builder` → seeder + oracle + validation sampler
   - `stack-runner-builder` → campaign runner + report aggregator

   Each gets the contract path in its prompt and is told the contract is
   binding. If any builder reports a contract gap, collect it — do not let it
   invent a value.

4. **Review gate.** Launch `stack-reviewer` on the assembled stack. If
   REJECTED, re-run only the rejected builders with the reviewer's findings in
   their prompts (one retry; after that, stop and report).

5. **Report to the user**, in this order: the spike result, the reviewer's
   verdict, any contract gaps still open, what was built and where, and the
   exact next command (`/smoke <subject>`). Portuguese for the chat summary;
   the artifacts themselves stay in English.

## Rules

- Never modify `_sources/`.
- Never overwrite an existing `CONTRACT.md` or built artifacts without asking.
- **This command builds. It does not run load.** Not even a smoke rung —
  that is `/smoke`. Builders may compile, lint and unit-test their own code.
- A disqualification at the spike gate is a successful outcome. Say it plainly:
  it cost an hour instead of a month.
- If the study's own open questions include unresolved blocking items beyond
  R1/R1b, surface them before building rather than after.

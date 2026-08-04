Re-review the built integration stack for the subject named in the arguments:
`$ARGUMENTS` (a subject name, e.g. `strapi`).

`/integrate` already runs `stack-reviewer` as its final gate. This command
exists for the cases where you want the gate again without rebuilding: after
hand-editing the chart or the runner, after a PgCache version bump, before
committing cluster time to a campaign, or when a campaign produced a result
you do not trust.

## Pipeline

1. Launch `stack-reviewer` on `<subject>/`. Give it the contract, the study,
   and the built artifacts.
2. **Report to the user**: findings ranked by severity with their `file:line`
   evidence, then the verdict. Portuguese for the chat summary.

## Rules

- **Read-only.** This command reviews; it does not fix and does not rebuild.
  If the verdict is REJECTED, the user decides whether to re-run `/integrate`.
- Do not run load.
- If the reviewer's findings contradict something in `CONTRACT.md`, say so
  explicitly rather than picking a side — a contract that disagrees with a
  reviewed artifact is itself a finding.

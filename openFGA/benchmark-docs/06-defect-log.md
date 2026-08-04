# Defect Log

Every defect found **in the benchmark itself**, with the evidence that exposed it
and the fix. Recorded because a benchmark whose own bugs are not published is not
auditable — and because four of these silently invalidated published numbers.

Ordered by discovery. Severity reflects impact on published results, not effort
to fix.

---

## D-01 · Document axis had no locality

**Severity: critical — invalidated the workload it was supposed to test.**

`PLAN.md` §6 specifies Zipf(α=1.1) over users **and objects**. The generator
seeded document selection with the request sequence number:

```go
d = p.PositiveDocFor(u, int(seq))   // w1
d = p.NegativeDocFor(u, int(seq))   // w3
```

Both functions map that parameter through modular arithmetic
(`universe.go:296`: `m := (r*2654435761 + t*97) % p.Docs`). A monotonic sequence
through multiplication by a prime modulo `Docs` is a **permutation**.

Measured over 1,600 requests:

| Workload | Distinct documents | |
|---|---:|---|
| W3, rung E1 | 1,591 / 1,600 | **99.4% unique** |
| W3, rung E2 | 1,600 / 1,600 | 100.0% unique |
| W3, rung E3 | 1,600 / 1,600 | 100.0% unique |
| W3 under true Zipf(1.1) | 600 / 1,600 | 37.5% unique |

Without a repeated `object_id` there is no repeated `WHERE` clause — precisely
the condition a read cache exists to exploit. **The W3 tables did not test the
hypothesis they claimed to test.**

W1 was less affected (~37% distinct) because `PositiveDocFor` already constrains
the codomain to documents the user can actually view.

**Fix.** New `-doc-dist zipf|uniform` flag; `docSeed()` feeds the Zipf index
instead of the sequence number. `uniform` preserves the old behaviour as a
control. Regression locked by `TestDocLocality` in
`tools/cmd/fgabench/generator_test.go`.

**Effect after fix:** W3 hit ratio rose to 82%, with a proper warm-up plateau
(77.0 → 87.8 → 89.4 → 90.6 → 91.4%). PgCache still lost.

---

## D-02 · Headline CPU claim had no supporting artifact

**Severity: critical — the finding that reoriented the lab was unsupported.**

The report's central claim — "the bottleneck was not the database", citing
`fga-a 198.2% / fga-origin 4.8% / fga-pgcache 0.2%` — appears in **no committed
file**. `scripts/run.sh` called `docker stats --no-stream` once, *after* the
generator exited, measuring an idle system.

Maximum `fga-a` value across the **non-quarantined** `docker-stats.csv` files:
**2.84%**. The only reading above 100% sits in `results/_contaminado/A-lad1/`
(105.01%, with the origin at 31.31% — not 4.8%), a directory that had already
been quarantined and therefore cannot support a published claim.

**Fix.** Background sampler at 2 s intervals with RFC3339 timestamps, running for
the duration of the measurement window, killed via `trap`.

---

## D-03 · Report aggregated runs from different target rates

**Severity: high — produced comparisons between conditions that never coexisted.**

`report.py` grouped by `(rung, workload, path)` with no rate in the key. Rung E1
/ W1 / path A became the median of six runs spanning five different target rates,
including a ramp probe at rate 100 that achieved 1.9 rps with a p99 of 29 s.

Result: path A reported at 15 rps against paths B and C at 40 rps. The published
`dados-consolidados.csv` and HTML dashboard carried this; the Markdown report
escaped only because the numbers had been hand-picked.

**Fix.** `rate_target` and `doc_dist` both entered the group key. Per
`(rung, workload)` the tool selects the condition with the widest path coverage,
ties broken by most recent run. Excluded runs are printed to stdout rather than
dropped silently.

---

## D-04 · Percentiles computed over survivors

**Severity: high — published numbers with no referent.**

Under saturation the open-loop generator's client queue fills and the overflow is
dropped. Drops are **not random**: they remove exactly the requests that would
have been slowest.

Rung E3, path B: **26 requests completed, 1,575 dropped.** The published "p99 of
39,455 ms" was computed from 26 samples.

**Fix.** `DROP_LIMIT = 0.10` in `report.py`. Anything above is flagged ⚠, and the
gain comparison is refused with an explicit message rather than a number.

---

## D-05 · PgCache container was CPU-capped below the origin

**Severity: high — confounded the E3 collapse.**

`docker-compose.yml` gave the origin `cpus: 3.0` and PgCache `cpus: 2.0` — while
PgCache hosts **proxy plus an embedded PostgreSQL 18** on those two cores.

At rung E3 it hit the ceiling:

```
fga-pgcache,202.07%,5.245GiB / 6GiB   <- 202% of a 2.0-cpu limit = saturated
fga-b,0.94%,81.35MiB / 2GiB           <- OpenFGA idle, waiting
fga-origin,118.95%,976.7MiB / 4GiB
```

The path-B collapse at E3 could not be attributed to the caching mechanism while
the container itself was the binding constraint.

**Fix.** CPU raised to 4.0. After the fix PgCache was no longer pinned at its
ceiling and still lost — the rescue hypothesis was eliminated and the result did
not change. Peak figures depend on the window: 140.49% is the whole-file peak of
one run (`E1/w3/B-r3`); the campaign-wide in-file maximum is 298.43% and the
in-measurement-window maximum is 192.32%, both against a 400% ceiling.

---

## D-06 · Memory limit set above physical host RAM

**Severity: high — self-inflicted while correcting D-05.**

Raising `mem_limit` to `8g` on a host with **7.75 GiB total RAM** produces a
cgroup that can never cut. PgCache grew until the VM was starved and restarted
mid-window.

Evidence: counters went *backwards* between the before/after snapshots
(`pgcache_queries_cache_hit` 655,992 → 181,460), and E1/w3/B logged 197 errors in
2,400 requests.

Peak observed at rung E1 — with only 85K tuples — was **5.12 GiB of 7.75 GiB**.

**The 5g ceiling was still not enough.** Under the corrected limit, PgCache
reached **4.791 GiB of 5 GiB** and restarted again during E1/W2 rep 3
(`pgcache_hits: -392433`, 406 errors). Of three W2 path-B repetitions only one
produced a usable hit ratio: rep 1 never wrote `pgcache-metrics-after.txt` at
all. **The W2 path-B hit ratio in Campaign B is therefore not measured.**

**Fix.** `mem_limit` reduced to `5g`, a real ceiling below physical RAM. Affected
runs quarantined in `results/_contaminado-mem8g/`.

**This also qualifies D-05:** the original `6g` was *also* above what the host
could spare, so the 2026-07-25 run effectively had no memory ceiling either. The
CPU cap was a genuine defect; the memory cap was a machine limitation.

---

## D-07 · Stale CSV header silently discarded new columns

**Severity: medium — caused two experiments to be aggregated as one.**

`appendMasterCSV` only wrote a header when the file did not exist. After
`doc_dist` and `warmup_s` were added, rows carried 26 fields under a 24-column
header, and Python's `DictReader` discarded the extras. `doc_dist` arrived empty,
so `zipf` and `uniform` runs were aggregated together — exactly the defect D-03
had just fixed, in a different dimension.

**Fix.** `appendMasterCSV` now rewrites a stale header in place. New columns are
appended at the end only, so older files stay valid.

---

## D-08 · Fixed warm-up instead of hit-rate knee

**Severity: medium — measured cold cache and labelled it PgCache.**

`PLAN.md` §8.2 requires waiting for the hit-rate knee. `suite.sh` passed a fixed
30 s. At rung E3 the cache was still registering new shapes after six minutes and
never converged.

**Fix.** Adaptive warm-up: the generator warms in slices, measures the hit ratio
*of the slice*, and stops when it stabilises within `-warmup-knee-eps`. Prints an
explicit warning if it reaches the ceiling without stabilising.

---

## D-09 · `pg_stat_statements` reset in the wrong place

**Severity: low — server and client windows did not coincide.**

`run.sh` reset the counters before warm-up, so the server-side window covered
warm-up plus measurement while the client's covered measurement only.

**Fix.** New `-post-warmup-exec` hook runs the reset between warm-up and
measurement.

---

## Kubernetes-specific defects

These surfaced when porting the lab to AKS. None affects previously published
numbers.

### D-10 · StorageClass existed only in the node-pool script

`origin-premiumv2` was created by `cluster.sh pools`, a path abandoned when
placement moved to a shared node pool. The PVC never bound, `origin-0` stayed
Pending, and everything cascaded: PgCache could not resolve the origin's DNS and
exited; OpenFGA looped on "waiting for database".

**Fix.** StorageClass moved into the Helm chart (`templates/storageclass.yaml`).

### D-11 · gRPC port collision under `hostNetwork`

Three OpenFGA pods share a node with `hostNetwork: true`. Only HTTP and metrics
ports were made distinct; all three tried to bind gRPC 8081.

```
panic: failed to listen: listen tcp 0.0.0.0:8081: bind: address already in use
```

**Fix.** Per-path `grpcPort` (8081 / 8091 / 8101) and an explicit `--grpc-addr`.

### D-12 · Schema migration was never ported

`docker-compose.yml` had a `migrate` service the chart omitted. All three OpenFGA
pods looped on `datastore requires migrations: at revision '0', but requires '4'`
and never passed readiness.

**Fix.** `templates/migrate-job.yaml`, a Helm `post-install,post-upgrade` hook.

### D-13 · Anti-affinity repels pods that lack the role label

**The subtlest one.** The lab's pod anti-affinity uses `operator: NotIn` on
`lab.benedet/role`. In Kubernetes label selectors a **missing key satisfies
`NotIn`** — so any pod without that label is repelled by every lab pod, on every
node.

The migration Job hung Pending indefinitely:

```
0/7 nodes are available: 2 node(s) didn't match Pod's node affinity/selector,
5 node(s) didn't satisfy existing pods anti-affinity rules.
```

**Fix.** The Job's pod template carries `lab.benedet/role: openfga`. Any future
pod added to this namespace must carry a role label or it will never schedule.

### D-14 · `--experimentals=` cannot be cleared in Kubernetes — UNRESOLVED

OpenFGA ships with `experimentals: ["pipeline_list_objects"]` by default.
`PLAN.md` §9 requires zeroing it so the three paths are comparable. In Docker
Compose, `--experimentals=""` worked because the shell collapses it to an empty
string.

As a Kubernetes arg, `--experimentals=` is ignored:

```
🧪 experimental features enabled: [pipeline_list_objects]
```

Setting `OPENFGA_EXPERIMENTALS=""` as an env var made it **worse** — the value
duplicated:

```
"Experimentals":["pipeline_list_objects","pipeline_list_objects"]
```

**Status: open.** The flag is identical across all three paths, so A/B/C remain
mutually comparable, but AKS results are **not** directly comparable to the local
lab on this dimension. `pipeline_list_objects` affects `ListObjects` only, so W1
and W3 (pure `Check`) are unaffected. **W4 results would be affected and must
carry this caveat.**

---

## Summary

| ID | Severity | Status |
|---|---|---|
| D-01 document locality | critical | fixed, regression-locked |
| D-02 unsupported CPU claim | critical | fixed |
| D-03 rate aggregation | high | fixed |
| D-04 percentiles over survivors | high | fixed |
| D-05 PgCache CPU cap | high | fixed |
| D-06 memory limit above host RAM | high | fixed |
| D-07 stale CSV header | medium | fixed |
| D-08 fixed warm-up | medium | fixed for real in r6 — campaign now passes `-warmup-max` |
| D-09 stat reset placement | low | fixed |
| D-10 missing StorageClass | high (AKS) | fixed |
| D-11 gRPC port collision | high (AKS) | fixed |
| D-12 missing migration | high (AKS) | fixed |
| D-13 anti-affinity role label | medium (AKS) | fixed |
| D-14 experimentals flag | medium | **open** |
| D-15 campaign missed pre-seed PgCache stop | high (AKS) | fixed |
| D-16 kubelet CFS config not active on deployed pool | informational | documented |
| D-17 Spot deallocation destroyed phase-1 artifacts | high (AKS) | recovered from log; fixes applied |
| D-18 run_id collides across campaigns | critical | fixed, verified in r5 |
| D-19 RollingUpdate deadlocks one-pod-per-node | blocker (AKS) | fixed |
| D-20 leaking `reg_gate_loading` gauge / cold-start stampede | high | fixed |
| D-21 contamination alarm fired on every run | medium | fixed in source, **not yet applied to the cluster** |
| D-22 five path-B runs lost to stale DNS | medium | fixed in source, **not yet applied to the cluster** |
| D-23 report mixed two distributions in one table | high | fixed |
| D-24 PgCache measured stock and under-warmed | **critical** | re-measured in r6; **r2–r5 performance conclusions retracted** |

**What survived all of it:** the correctness result. Across every campaign the
differential gate reported 100.000000% agreement with zero divergences. That
result depends on none of the defects above.

---

## D-15 · Campaign runner omitted the pre-seed PgCache stop

**Severity: high (AKS) — deadlocked the autonomous campaign on its first gate.**

`scripts/seed.sh:15` stops PgCache **before** the bulk `COPY`, with the stated
reason that the logical replication consumer has no business digesting the WAL of
a mass load, and that the cache must start empty anyway.

`campaign-aks.sh` was written without that step. After the seed truncated and
reloaded the mass, PgCache continued serving the **previous** dataset. Every
`Check` through path B exhausted the 60 s request timeout:

```
"raw_response": {"code":"deadline_exceeded","message":"Request Deadline Exceeded"},
"query_duration_ms": "60000", "datastore_query_count": 9
```

`openfga-b` never reached readiness and the W7 correctness gate hung
indefinitely — the campaign made no progress for roughly ten minutes before the
condition was noticed.

**Fix.** `seed_rung()` now performs a cold restart of both `pgcache` and
`openfga-b` after seeding and before the oracle validation, mirroring what
`seed.sh` did with `docker compose stop`.

---

## D-16 · Kubelet CPU-quota config is not active on the deployed pool

**Severity: informational — the guarantee holds, but not for the stated reason.**

`cluster.sh` writes a kubelet configuration disabling CFS quota
(`cpuCfsQuota: false`, `cpuManagerPolicy: static`) when it creates dedicated node
pools. Placement later moved to `mode: sharedPool`, reusing the operator's
pre-existing `userpool` — so that script never ran, and both deployed pools
report `kubeletConfig: null`.

The no-throttling property still holds, because the Helm chart declares
`requests` only and never `limits`: with no CPU limit there is no CFS quota to
enforce, regardless of kubelet configuration.

**Recorded because the safety margin is thinner than intended.** If anyone adds a
CPU `limit` to the chart, throttling returns silently and will present as a p99
spike indistinguishable from PgCache shape-registration latency.

---

## D-17 · Spot deallocation destroyed phase-1 artifacts; bare Pod never rescheduled

**Severity: high (AKS) — lost the per-request data of a completed 27-run phase.**

Three compounding causes:

1. The `loadgen` pod is a **bare Pod, not a Deployment**: when its Spot node was
   deallocated, nothing recreated it, and its `emptyDir` — holding every phase-1
   artifact — died with the node. The old `vmss000000-000005` instances were
   replaced by `vmss000008/9/a`; all controller-managed pods rescheduled,
   `loadgen` did not.
2. The campaign runner executed on an operator laptop via `nohup`. The process
   died overnight (host sleep), so the per-phase `snapshot` added after the
   previous data loss (see the phase-3 incident in `05-results-aks-earlier.md`) never
   executed — phase 1 finished its last run at 01:19:50Z and the log stops there.
3. The phase-5 ramp was separately contaminated by a `helm upgrade` performed
   mid-campaign to deploy monitoring (operator error, logged at 00:25:42Z as
   `!! 1 pod(s) fora de 1/1 Running apos E1/w1/B/ramp160`).

**Recovered:** the campaign log records each run's summary (p50/p95/p99,
amplification, hit ratio, errors, drops) at completion — 22 of 27 runs parsed
back out of the log and published in `05-results-aks-earlier.md` with a provenance
caveat. `latencies.csv` (per-request rows) is unrecoverable.

**Fixes required before the next campaign:**
- `loadgen` becomes a Deployment (or writes to a PVC that survives it).
- The campaign runner moves inside the cluster (a Job on the system pool) or to
  an always-on host — never a laptop that sleeps.
- `snapshot` runs after **every run**, not every phase, for path-B artifacts at
  minimum.
- No `helm upgrade` while a campaign is live; the runner should hold a lockfile
  the operator can check.

---

## D-18 · `run_id` collides across campaigns; CSV and artifacts diverge

**Severity: critical — the master CSV contains rows whose artifacts belong to a
different campaign, and there is no column that distinguishes them.**

The campaign runner labels repetitions `bare-r1`, `bare-r2`, `unif-r1`, … The
labels restart at `r1` on every campaign, and the results volume is not cleared
between them. Two consequences:

1. **Artifacts are overwritten.** `results/E1/w1/A-bare-r1/` is rewritten by each
   campaign that runs that combination. The directory always holds the *last*
   campaign's data.
2. **`all-runs.csv` only appends.** Rows from earlier campaigns survive under
   identical `run_id` values.

Measured on the preserved snapshots:

| Snapshot | artifact dirs | CSV rows |
|---|---:|---:|
| `results-aks-fase3-4` | 33 | 33 |
| `results-aks-r2-parcial` | 24 | **36** |
| `results-aks-r3` | 35 | **46** |

In the r3 CSV, `bare-r1` appears **15 times** — it should appear at most once per
(rung, workload, path), so at most 9. The extra rows are r2's, indistinguishable
from r3's.

`report.py` groups by `(rung, workload, rate_target, doc_dist, path)` and takes
the median. **It is therefore silently computing medians across campaigns that
ran under different code and different infrastructure** — the exact failure mode
D-03 and D-07 were supposed to have closed, arriving through a third door.

The same corrupted row is demonstrably present in two snapshots:

```
results-aks-r2-parcial/all-runs.csv:  bare-r3 E1 w3 A err=2400/2400
results-aks-r3/all-runs.csv:          bare-r3 E1 w3 A err=2400/2400
```

**Required fix (not yet applied):**
- `run_id` must be globally unique — prefix with the campaign id and a UTC
  timestamp, e.g. `r4-20260803T1530Z-bare-r1`.
- A `campaign_id` column joins the standard CSV schema and the `report.py`
  group key.
- The runner clears (or rotates) `results/` at campaign start, or writes each
  campaign to `results/<campaign_id>/`.
- `report.py` refuses to aggregate rows whose `campaign_id` differs, the same way
  it already refuses to mix rate targets.

**This invalidates any aggregate computed from an `all-runs.csv` that spans more
than one campaign.** Per-run `summary.json` files remain trustworthy; the
per-campaign tables published in `05-results-aks-earlier.md` were parsed from campaign
logs, not from the master CSV, and are unaffected.

---

## D-19 — Rolling updates deadlock a one-pod-per-node lab

**Found:** 2026-08-03, campaign r4, first attempt.
**Severity:** blocker. Campaign hung at the first gate and produced nothing.

`cold_pgcache` restarts PgCache and the path-B OpenFGA between repetitions. Both
were `Deployment`s, and only PgCache declared `strategy: Recreate`; the three
OpenFGA deployments used the default `RollingUpdate`.

RollingUpdate creates the replacement pod *before* terminating the old one. In a
lab whose entire premise is one pod per node, there is no spare node for that
replacement — and there was no spare node in Azure either, since the Spot quota
sat at 40/40. What actually happened is worse than a simple stall: PgCache (being
`Recreate`) released its node first, the surging `openfga-b` replica grabbed that
freshly-vacated node, and the new PgCache pod was then left `Pending` with
nowhere to go. The old `openfga-b` never terminated, so the rollout never
completed. Deadlock.

```
pgcache-ff75c6764-zjn2c     0/1  Pending            8m38s   <none>
openfga-b-74bc6b79c8-7qcf5  0/1  CrashLoopBackOff   10m     aks-userpool-...h
```

RollingUpdate is also semantically wrong here. The point of the restart is a
*cold* cache; briefly running two PgCache instances, or two path-B servers
against different cache states, is the opposite of what the step is for.

**Fix:** `strategy: {type: Recreate}` on all five lab deployments, and restart
PgCache *first*, wait for it, and only then restart `openfga-b` — restarting both
at once brings the path-B server up against a datastore that does not exist yet,
and it burns its restart budget in CrashLoopBackOff.

Note for anyone applying this to a live cluster: server-side apply rejects the
in-place switch (`spec.strategy.rollingUpdate: Forbidden: may not be specified
when strategy 'type' is 'Recreate'`). The deployments have to be deleted and
recreated by the chart.

## D-20 — `pgcache_cache_reg_gate_loading` is a leaking gauge

**Found:** 2026-08-03, campaign r4, second attempt.
**Severity:** high — it invalidates the obvious readiness signal, and cost us a
wrong fix before we caught it.

After a cold restart PgCache registers each new query *shape*, which means
scanning the origin table into its local cache store. Queries on a shape being
registered are held until registration finishes. The W7 correctness gate walks
4,000 distinct pairs — 4,000 distinct shapes — and it is the only step in the
campaign that does so against a just-restarted PgCache. At 32 workers that is up
to 32 concurrent registrations; PgCache saturates and **every** path-B Check
returns `deadline_exceeded` at the 60 s OpenFGA request timeout:

```
"raw_response": {"code":"deadline_exceeded","message":"Request Deadline Exceeded"},
"query_duration_ms": "60000", "datastore_query_count": 9
```

Campaigns r2 and r3 survived this by accident. Their `cold_pgcache` restarted
both deployments in parallel and then polled `healthz` for up to 150 s; that
incidental wait was long enough for the snapshot to finish. Making the restart
sequential and fast removed the accidental cushion and exposed the real
dependency.

The trap is the metric. `pgcache_cache_reg_gate_loading` looks like exactly the
signal you want, and it is not:

```
pgcache_cache_tables_tracked      2
pgcache_cache_reg_gate_loading   25     <- with the stack completely idle
pgcache_cache_queries_loading     0
```

It sat at 25 for minutes with no load at all. When OpenFGA cancels at the
deadline the registration is abandoned and the gauge never decrements. Meanwhile
PgCache itself was perfectly healthy — `select count(*) from tuple` through port
6432 returned the correct 84,598 in **57 ms**. A readiness gate keyed on that
gauge returning to zero would block forever, on a component that was fine.

The component that was actually broken was `openfga-b`: its connection pool
(`maxOpenConns: 30`) stayed wedged on the timed-out Checks, so it never became
Ready again on its own. Under `networkMode: bare` a non-Ready pod leaves the
headless Service with no endpoint, so `curl` did not even fail on connect — DNS
stopped resolving (`exit 6`), and CoreDNS then cached that negative answer.

**Fix:** gate on observable end-to-end behaviour instead of on an internal
counter — PgCache must return the origin's exact tuple count through its own SQL
port, *and* `openfga-b` must answer `healthz`, with the path-B server recycled if
it has not recovered on its own. Separately, the W7 gate runs at 8 workers rather
than 32; it compares decisions and never reports timing, so lowering concurrency
costs the verdict nothing. The measured workloads are unaffected because their
zipf concentration reuses a small set of shapes.

## D-21 — The contamination alarm fired on every run

**Found:** 2026-08-03, campaign r5.
**Severity:** medium. Nothing was measured wrong, but the one alarm meant to
catch a Spot eviction inside a measurement window was crying wolf after every
single run.

`check_health` grepped `preempt|evict|oomkill|NodeNotReady` across the whole
`kubectl get events` line, with no time bound. Two independent problems:

- Every Kubernetes `FailedScheduling` event carries the string *"Preemption is
  not helpful for scheduling"* in its **message**. The filter was matching that
  text, not the event's reason.
- With no time window, events from the deploy — 50 minutes before the first
  measurement — were re-reported after each run.

```
[16:20:53Z] !! CONTAMINACAO POSSIVEL apos E1/w1/C/bare-r1:
51m  Warning  FailedScheduling  pod/openfga-a-...  ... Preemption is not helpful ...
```

**Fix:** match on the REASON column, and only consider events younger than five
minutes.

## D-22 — Five path-B runs lost to a stale DNS answer

**Found:** 2026-08-03, campaign r5.
**Severity:** medium. Nothing was mismeasured; five of fifty-seven runs simply did
not happen, and they were all path B, so four conditions lost their comparison.

Every failure carries the same signature, always at the generator's own preflight,
never mid-window:

```
[17:50:35Z] --- E1/w1/B run=r5-ramp20 dist=zipf rate=20
erro: alvo http://openfga-b...:8090 nao responde: dial tcp: lookup
openfga-b.pgcache-lab.svc.cluster.local on 10.0.0.10:53: no such host
```

The `wait_path_b_ready` gate had passed seconds earlier, and it requires
`healthz` to return 200. So a single successful probe does not guarantee the
*next* process resolves the name. Under `networkMode: bare` the Service is
headless: each `openfga-b` restart removes the endpoint, CoreDNS caches the
negative answer, and there is a window during which the name fails again. A
one-shot probe crosses that window by luck.

Coverage after the campaign — the gaps are exactly where path B is missing:

| condition | runs | paths |
|---|---|---|
| w1 zipf 40 rps | 9 | A B C |
| w1 uniform 40 rps | 8 | A B C (one B rep missing) |
| w2 zipf 40 rps | 8 | A B C (one B rep missing) |
| w3 zipf 40 rps | 9 | A B C |
| w3 uniform 40 rps | 9 | A B C |
| w1 zipf 20 rps | 2 | A C — **B missing** |
| w1 zipf 80 rps | 2 | A C — **B missing** |
| w1 zipf 160 rps | 3 | A B C |
| w1 zipf 320 rps | 2 | A C — **B missing** |

**Fix:** the readiness gate now requires three consecutive successful probes,
spaced five seconds apart, each in a fresh process. And `one_run` retries once —
an infrastructure stumble *before* the window opens measures nothing and
contaminates nothing, so losing the whole matrix cell to it is pure waste. A
genuinely broken target still fails twice and still shows up as a gap.

## D-23 — The report silently mixed two distributions in one table

**Found:** 2026-08-03, analysing campaign r5.
**Severity:** high for anyone reading the generated report. The numbers are each
individually correct; the table invites an invalid comparison.

`agg()` elects one condition per `(rung, workload)` — the one covering the most
paths, breaking ties by repetition count and then by most recent run. Electing a
single condition is right: it is the only slice in which A, B and C are
comparable. The defect is that the report never said *which* condition won.

In r5, `w1` and `w2` were reported under `zipf` while `w3` was reported under
`uniform`, in the same table, with nothing to distinguish them. Phase 2 had run
`w1` and `w3` with `doc-dist=uniform`; for `w3` both distributions had three
paths and three repetitions, so the tie fell to the most recent run — `uniform`.
A reader comparing rows across that table would be comparing two different
experiments.

The old warning made it worse by misdescribing the excluded runs as *"fora da
taxa alvo comparavel"* (outside the comparable target rate) when almost all of
them had hit their target rate exactly — they were excluded for belonging to a
condition that was not elected.

**Fix:** `main()` now prints the elected `(doc-dist, rate, campaign)` for every
`(rung, workload)` and warns explicitly when they are not all the same, and the
exclusion message says the real reason.

## D-24 — Every campaign measured PgCache stock, and under-warmed

**Found:** 2026-08-03, after campaign r5, by reading the vendor documentation.
**Severity:** high. It does not make any published number *wrong*, but it makes
the headline claim narrower than we stated it.

Two independent halves.

**We never enabled the adaptive warm-up we built.** D-08 replaced the fixed
warm-up with knee detection, gated behind `-warmup-max`. The campaign never
passes that flag, so `warmup()` falls straight through to the fixed path. All 52
runs of r5 recorded `warmup_s = 45`. Measured for the first time, the knee is at
**2m30s**:

```
30s: 76.0%   1m0s: 88.9%   1m30s: 90.3%   2m0s: 91.7%   2m30s: 92.7%
joelho em 2m30s (delta 0.0093 < 0.0100)
```

At 45 seconds the cache is still climbing steeply. Warming to the knee and
changing nothing else moves W1 p99 from 25.16 ms to 22.57 ms and the hit ratio
from 90.2% to 93.0%. D-08's status in this log said *fixed*; the fix was
unreachable code on the campaign path.

**We never used `PINNED_TABLES`.** The chart passed six environment variables and
left every other documented knob at its default — including `PINNED_TABLES`,
`PINNED_QUERIES`, `ADMISSION_THRESHOLD`, `CACHE_POLICY` and `MV_SIZE_RATIO`. The
documentation states that single-table pinned queries with no joins "are always
kept up to date via CDC — they are updated in place and never invalidated", which
describes our workload exactly: every OpenFGA read is a single-table `SELECT`
with equality predicates.

Probing it (W1, 40 rps, same seed, only the config changing) inverts the tail:

| config | p95 | p99 | max | hit ratio |
|---|---:|---:|---:|---:|
| stock | 16.31 | 22.57 | 40.91 | 93.0% |
| stock (duplicate control) | 16.76 | 22.27 | 54.55 | 93.1% |
| **pinned** | **10.89** | **15.81** | **20.55** | **78.2%** |

The hit ratio *drops* while the tail improves — pinning does not create more
hits, it makes misses cheap, which is the term our whole diagnosis said dominates
(cost of miss × ~100 queries per request).

**Status:** the chart now exposes `pgcache.pinnedTables` and
`pgcache.admissionThreshold`, both defaulting to off. The probe is n=1 on the
treatment and its warm-up never converged, so it is a signal, not a result. Full
write-up in [07-pgcache-configuration.md](07-pgcache-configuration.md).

**What this means for published claims:** r5's PgCache figures describe a stock,
under-warmed PgCache. That is a legitimate measurement — it is what someone gets
by deploying the container and pointing it at their database — but it must be
labelled as such, and it is not the same claim as "PgCache is 1.9× slower than no
cache".

---

## Retraction notice — campaigns r2 through r5

D-24 is not only a defect in the harness; it invalidates a published conclusion.
Campaign r6 re-ran the same conditions with `PINNED_TABLES` set and the cache
warmed to its hit-ratio knee, changing nothing else:

| condition | published (stock, 45 s) | corrected (pinned, knee) |
|---|---:|---:|
| W1 40 rps, p99 | 25.16 ms (1.93× baseline) | 15.25 ms (1.20×) |
| W2 40 rps, p99 | 21.94 ms (1.86×) | 12.92 ms (1.09×) |
| W3 40 rps, p99 | 28.68 ms (2.81×) | 13.12 ms (1.35×) |
| W1 160 rps, p99 | 159.92 ms (9.9×) | **16.74 ms (1.05×)** |

**The claim "PgCache's penalty scales with load" is withdrawn.** It described the
stock configuration's warm-up deficit, not a property of the product. Baseline and
appcache figures moved by less than 5% between the two campaigns, so the cluster
is not the explanation.

Every campaign before r6 should be read as measuring *a stock, under-warmed
PgCache*. That remains a legitimate measurement — it is what you get by starting
the container and pointing it at a database — but it must be labelled as such.
Full comparison in [08-results-aks-r6.md](08-results-aks-r6.md).

# 03 — Methodology and Validity

This document exists so you can decide whether to believe the numbers in this
repository. It states what the measurement protocol actually is, which numbers we
refuse to publish and why, and every confound we know about and have not yet
eliminated.

Nothing here is aspirational. Every rule described below is enforced by code —
`scripts/run.sh`, `scripts/suite.sh`, `scripts/campaign-aks.sh`,
`scripts/report.py` — or by a test that fails the build. Where a rule is enforced
only by a human reading a checklist, that is said explicitly.

Related documents:

| Document | Answers |
|---|---|
| `PLAN.md` | Why. Hypotheses, choice of subject, threats to validity, protocol. |
| `docs/RUNBOOK.md` | How. Execution, collection, interpretation, and when a number is void. |
| `docs/DESCOBERTAS-INTEGRACAO.md` | Integration gotchas found the hard way. |
| **this document** | Whether the numbers can be trusted, and under what conditions. |

---

## 1. Measurement protocol

### 1.1 The golden rule

Three paths are measured:

| Path | Name | Datastore | OpenFGA internal caches | What it isolates |
|---|---|---|---|---|
| **A** | `baseline` | `origin` directly | all OFF | true datastore cost |
| **B** | `pgcache` | `pgcache` → `origin` | all OFF | gain attributable **only** to PgCache |
| **C** | `appcache` | `origin` directly | check + iterator + controller ON, TTL 10 s | the honest competitor |

All three must run against the **same origin**, with the **same dataset**, in the
**same window**, driven by the **same generator**, at the **same target rate**.
Numbers from different runs are never compared. This is not a convention — the
group key in `scripts/report.py` makes it structurally impossible to compare
across target rates or across document distributions (see §4).

Path C exists because without it the question degenerates into "does caching
help?", whose answer is trivially yes. With it the question is *which* cache — a
database-level cache invalidated by CDC, or an application-level cache
invalidated by TTL — and that has an actual answer.

In Kubernetes all three OpenFGA deployments are rendered from a single Helm
template (`infra/aks/chart/templates/openfga.yaml`) iterating
`.Values.openfga.paths`. That is not line-count economy: separate manifests let a
flag drift on one path without anyone noticing, at which point the comparison
stops measuring what it claims to measure.

### 1.2 Open-loop generation

`fgabench` fires at a fixed rate from an arrival scheduler and does **not** wait
for the previous response. Latency is measured from the **intended arrival
instant**, not from the moment a worker became free.

This avoids *coordinated omission* — the classic error where a saturated system
looks fast because the client stopped offering load. The price is that under
saturation the client-side queue fills and the overflow is counted as a **drop**.
That price is paid deliberately, and §3 is about how to read the result.

### 1.3 One path under load at a time

Every runner is sequential by construction. From `scripts/campaign-aks.sh`:

> Measurements are sequential by construction. The lab's golden rule is one path
> under load at a time (`PLAN.md` §8): two paths measured simultaneously compete
> for CPU and neither number is valid. Parallelizing here does not speed anything
> up — it corrupts.

In the AKS topology A, B and C share the same node. That is acceptable *only*
because the other two are idle while one is under load.

### 1.4 Warm-up excluded from the analysis window

Measuring a cold cache and calling it "PgCache" is fraud. Two mechanisms:

- **Adaptive warm-up.** With `WARMUP_MAX` set, the generator warms in slices and
  measures the hit ratio **of the slice**, stopping when it stabilizes (delta
  < 0.01). If it emits `AVISO: teto de aquecimento atingido sem estabilizar`, the
  measurement window caught a cache still forming, and the run is either redone
  with a higher ceiling or published with the limitation stated.
- **Cold cache per repetition.** Before each path-B repetition the PgCache
  deployment is restarted (`cold_pgcache()` in `scripts/campaign-aks.sh`;
  `docker compose restart pgcache` locally). Without this, repetition 2 inherits
  the shapes registered in repetition 1 and silently measures a warm-up that is
  not inside the window.

`pg_stat_statements` on the origin is reset **between** warm-up and measurement
(via `-post-warmup-exec`), so the server-side window coincides with the client's.

### 1.5 Repetitions and central tendency

**Three repetitions per (rung × workload × path).** The reported value is the
**median** across repetitions, with min–max dispersion for `p50_ms`, `p95_ms`,
`p99_ms` and `rate_achieved`. A single run is never reported as a result.

`REPS=3` is the default in `scripts/campaign-aks.sh` and `make suite`.

### 1.6 The scale ladder

One measurement point proves nothing; the curve proves it. The ladder exists to
find the knee — the point where the working set stops fitting in the origin's
`shared_buffers`.

| Rung | Users | Groups | Folders | Docs | Tuples |
|---|---:|---:|---:|---:|---:|
| E0 | 200 | 50 | 100 | 500 | 3,453 |
| E1 | 2,000 | 500 | 1,000 | 10,000 | 84,598 |
| E2 | 10,000 | 2,500 | 5,000 | 100,000 | 1,023,498 |
| E3 | 50,000 | 12,500 | 20,000 | 800,000 | 9,710,498 |
| E4 | 100,000 | 25,000 | 50,000 | 2,500,000 | 30,249,998 |

Deliberate control: the origin's `shared_buffers` is **fixed at 1 GB on every
rung**. Without that control the ladder measures "how much RAM I gave Postgres",
not scalability.

---

## 2. Correctness gate before performance

For an authorization system, **a wrong cached answer is a security incident, not
a performance metric.** The protocol therefore refuses to produce a latency
number until correctness has been established, in two stages.

### 2.1 Stage 1 — the dataset against the analytical oracle

`tools/internal/universe` defines the dataset **purely structurally**: given the
counts, the authorization of any (user, document) pair is derivable in
O(depth) without querying anything. That is what makes 10 M+ tuples seedable via
`COPY` (the OpenFGA API caps at 100 tuples per call — ~97k requests otherwise)
while still knowing the right answer.

After every seed, `fgaseed verify` samples 300 positive and 300 negative pairs
and compares OpenFGA's answers against the oracle. Real output from the AKS
campaign log (`benchmark-docs/runs/campaign-20260802T212444Z.log`, E1):

```
verificacao do degrau E1 (http://openfga-a.pgcache-lab.svc.cluster.local:8080)
  positivos confirmados: 300   divergentes: 0
  negativos confirmados: 300   divergentes: 0
  erros de transporte:   0
  OK — oraculo e OpenFGA concordam em 100% da amostra.
```

If this diverges, **stop**. The dataset is wrong and no timing measurement
matters.

### 2.2 Stage 2 — W7, the differential gate

W7 issues **identical Check pairs against A and B** and compares three things:
A vs B agreement, A vs oracle, B vs oracle. It measures agreement, not time.

```
./bin/fgabench -workload w7 -manifest results/E1/manifest.json \
  -target http://localhost:18080 -diff-target http://localhost:18090 \
  -consistency MINIMIZE_LATENCY -workers 32
```

Expected output shape:

```
  amostras comparadas : 3969
  concordancia A==B   : 3969 (100.000000%)
  divergencias        : 0
  A bate com oraculo  : 3969 (100.000000%)
  B bate com oraculo  : 3969 (100.000000%)
```

The target is `100.000000%` — six decimal places, not "high nineties". W7 runs
**before any timing measurement**, on every rung, in both the local suite
(`scripts/suite.sh`) and the AKS campaign (`seed_rung()` in
`scripts/campaign-aks.sh`).

**If A and B disagree, the campaign becomes a bug report, not a performance
report.** The suite aborts. There is no configuration under which a divergent
cache produces a published latency number.

The reason this is structurally checkable at all: PgCache is configured with
`ALLOWED_TABLES=tuple,authorization_model`, which excludes `changelog` — the only
read query in OpenFGA's Postgres adapter whose result changes with wall-clock time
(`... WHERE inserted_at < NOW() - interval '0ms'`, with `NOW()` interpolated as a
literal). Every query admitted to the cache is deterministic.

Status of hypothesis H4 (correctness): **confirmed** and unaffected by the errata
that invalidated the first campaign's performance conclusions — 3,657 decisions
verified, zero divergences.

---

## 3. Why drop ratio invalidates percentiles

```
drop_ratio = dropped / (requests + dropped)
```

Under saturation the open-loop client's queue fills and the overflow is dropped
rather than sent. **The drops are not random.** The queue overflows precisely
when the system is slow, and the requests it sheds are exactly the ones that
would have been the slowest. A percentile computed over the survivors is a number
without a referent: it is not an estimate of the offered load's latency
distribution, because the sample is censored by the very variable being measured.

`scripts/report.py` sets `DROP_LIMIT = 0.10`, marks any combination above it with
⚠, and **refuses the A/B comparison**:

> **PgCache vs baseline:** not comparable — at least one of the paths collapsed
> and its percentiles do not describe the offered load.

### The real example — rung E3, W1, target 40 rps

From `results/all-runs.csv` (run `v3`):

| Path | completed | dropped | drop ratio | rps achieved | p50 ms | p99 ms | queries/req |
|---|---:|---:|---:|---:|---:|---:|---:|
| A · baseline | 1,046 | 555 | 34.7% | 26.15 | 71.20 | 13,366.60 | 218.9 |
| B · pgcache | **26** | **1,575** | **98.4%** | 0.65 | 30,503.64 | **39,454.78** | 102.4 |
| C · appcache | 491 | 1,110 | 69.3% | 12.27 | 10,642.89 | 28,185.72 | 480.2 |

Path B's "p99 of 39,455 ms" is computed from **26 samples**, out of 1,601
requests the generator intended to send. It is not a latency measurement. The
correct reading is: *at 40 rps on E3, path B did not sustain the offered rate* —
it sustained 0.65 rps.

Note also that path B's amplification on that row (102.4 queries/request) is
likewise computed over the same 26 completed requests, so it cannot be compared
with path A's 218.9. When a path collapses, **every** per-request metric derived
from that window collapses with it.

And note the honest consequence: on this row *all three paths* are above the 10%
gate. The E3/W1 cell produces no publishable latency comparison at all. That is
the correct outcome, not a gap in the data.

A drop ratio above 10% is a **throughput result**, and should be reported as one:
"path X did not sustain N rps at rung E". It is never a latency result.

---

## 4. Aggregation rules

Implemented in `agg()` in `scripts/report.py`.

### 4.1 The group key

```
(rung, workload, rate_target, doc_dist, path)
```

`rate_target` and `doc_dist` are in the key for concrete, already-observed
reasons:

- **`rate_target`.** Mixing a 5 rps ramp probe with the 40 rps measurement puts
  them in the same bucket, and the resulting "median" corresponds to no condition
  that was ever executed. This is not hypothetical: in the 2026-07-25 run it
  produced an E1/W1 path A reported at 15 rps against B and C at 40 rps. The
  `results/all-runs.csv` in this repository still contains the probe rows
  (`probe5`, `probe10`, `probe20`, `probe40`) that caused it.
- **`doc_dist`.** `zipf` and `uniform` are different workloads — one has object
  locality, the other has none. Averaging them is averaging two experiments. See
  §8 for why that distinction is load-bearing.

### 4.2 Choosing the comparable rate

For each `(rung, workload)`, exactly one `(rate_target, doc_dist)` pair is
selected and everything else is excluded. The selection score, in order:

1. **Number of paths covered** — only a condition where all three paths ran is
   worth comparing.
2. **Number of repetitions.**
3. **Most recent `started_at`** — tie-break.

The third criterion matters more than it looks. When a re-run repeats the same
rate and the same path coverage as an earlier one, **the newer one wins**.
Without that tie-break the report freezes on the first measurement and silently
ignores the round you just finished.

Excluded runs are printed to stdout rather than dropped in silence:

```
aviso: 4 execucoes fora da taxa alvo comparavel, nao agregadas:
  E1/w1/A run=probe5 alvo=5 rps alcancado=5.0 rps
```

### 4.3 Statistic reported

Median across repetitions for every numeric field; min–max dispersion carried for
`p50_ms`, `p95_ms`, `p99_ms`, `rate_achieved`. `drop_ratio` is computed from the
**sum** of requests and drops across repetitions, not from the median, so a
single collapsed repetition cannot be hidden by two healthy ones.

### 4.4 Runs discarded before aggregation

A run with `errors / requests > 0.5` is dropped. A wholly failed run is an
incident, not a repetition. This rule was added after a pipeline-validation run
errored 1,200 of 1,200 requests (the rung's store had been wiped by a later
seed) and, aggregated in, dragged the median down with zeroed latencies.

### 4.5 What the aggregation does **not** separate

`report.py` groups by rung, workload, target rate, `doc_dist` and path. It does
**not** know about the AKS `networkMode` (`bare` vs `cni`). Results from the two
network modes must be kept in separate `results/` trees and separate
`all-runs.csv` files. Merging them is averaging two experiments, exactly like
merging `zipf` and `uniform`.

---

## 5. The key metric: query amplification

`queries_per_request` is derived from `datastore_query_count`, a counter exposed
by **OpenFGA itself** (`--datastore-metrics-enabled`), read as a delta between
`openfga-metrics-before.txt` and `openfga-metrics-after.txt` bracketing the
window. It is a measurement, not an estimate.

### 5.1 Why the number is large

OpenFGA resolves the authorization graph **in Go**, not in SQL
(`internal/graph/check.go`). A `Check` is a BFS over the model's rewrite tree, and
every returned tuple becomes a child dispatch. There is no batching and no `IN`
coalescing between sibling sub-problems:

- trivial direct Check: 1 query
- typed relation `[user, user:*, group#member]`: 3 concurrent queries at the node
- userset with fan-out F: `1 + F` sub-resolutions
- TTU (`viewer from parent`) with P parents: `1 + P`

And there is a decisive asymmetry: `union` short-circuits on the first
`Allowed: true` and cancels its siblings. **A positive answer finishes in few
queries; a negative answer must exhaust the entire fan-out.** That is why W3
(100% deny) is the maximum-amplification workload.

### 5.2 Observed values

Documented range: **124 to 480** datastore queries per request. Observed span
across `results/all-runs.csv` is wider at the low end because path C's in-process
cache absorbs datastore reads — which is precisely the point of this section.

| Rung / workload | A · baseline | B · pgcache | C · appcache |
|---|---:|---:|---:|
| E1 / W3 (rep r2) | 124.0 | 124.1 | 64.3 |
| E1 / W1 (rep r2) | 131.2 | 138.8 | 64.5 |
| E2 / W1 | 195.2 | 170.6 | 114.0 |
| E2 / W3 | 198.2 | 194.0 | 117.0 |
| E3 / W1 ⚠ collapsed | 218.9 | 102.4 | 480.2 |

### 5.3 The rule

**Always check amplification before latency. If it changed, you are not comparing
the same amount of work.**

Amplification is the lever that turns a small per-query overhead into collapse:
0.3 ms of added cost per query becomes 60 ms per request at 200 queries. Two
consequences:

- A path whose amplification *dropped* (path C, ~64 vs ~124) got faster partly by
  doing less work. That is a legitimate result, but it is a different claim from
  "the same work, served faster".
- A path whose amplification is *equal* gives a clean comparison. E1/W3 rep r2 is
  the useful worked example: A and B did essentially identical work (124.0 vs
  124.1 queries/request), both sustained the full 40 rps with 1 drop, and the
  medians nearly tie — yet the tails separate by ~7×.

| E1 / W3, rep r2 | queries/req | drop | p50 ms | p99 ms | PgCache hit ratio |
|---|---:|---:|---:|---:|---:|
| A · baseline | 124.0 | 1 | 10.07 | 43.42 | — |
| B · pgcache | 124.1 | 1 | 12.25 | **301.32** | 81.9% |

Same work, same offered rate, both healthy on the drop gate. Median essentially
tied, tail exploding. That signature is **miss cost, not hit cost** — and it is
why a high hit ratio is not, by itself, a gain. Confirm against the PgCache
counters: `pgcache_cache_queries_registered`,
`pgcache_query_registration_latency_seconds_sum`,
`pgcache_cache_registration_throttled_total`. In E1 that worked out to ~19.7 ms
per shape registration; at 124 queries per Check the odds of hitting at least one
new shape are high, and each one pays the registration.

---

## 6. Known confounds, stated honestly

These are unresolved. They are listed here rather than in a footnote because each
one bounds what the numbers can be used to claim.

### 6.1 `simple_protocol` — path B is not "path A plus a cache"

For PgCache to see the queries at all, the datastore URI must carry
`default_query_exec_mode=simple_protocol`. With the default extended protocol,
measured on this lab:

```
pgcache_queries_total              615,572
pgcache_queries_uncacheable        615,370      <- 99.97%
pgcache_queries_cacheable                1
pgcache_protocol_extended_queries  615,371
```

— all of the proxy's cost, none of its benefit, plus a 16× slowdown versus the
direct origin (414 ms vs 26 ms per check). With `simple_protocol`, 99.1% of
queries become cacheable.

`simple_protocol` makes `pgx` interpolate parameters into the SQL text. Two
opposite effects:

- the query becomes legible to the proxy, which can now cache it;
- prepared statements on the Postgres side disappear, so the origin re-plans
  every query.

**Therefore path B is not "path A plus a cache". It is "path A without prepared
statements, plus a cache."** The confounded portion has not been isolated. The
isolating experiment is defined and the switch exists
(`PGX_EXEC_MODE=simple_protocol` in `.env`, `execMode` in
`infra/aks/chart/values.yaml`) — run path A in `simple_protocol` and the
difference against path A in `cache_statement` is exactly the protocol's share.
**It has not been run.** Until it is, any B-vs-A delta contains an unknown amount
of protocol cost.

A second-order effect from the same change: with literals interpolated into the
SQL text, the shape space grows with the cartesian product of the dataset, which
is what drives the shape-registration cost in §5.3.

### 6.2 `pipeline_list_objects` could not be disabled in Kubernetes

OpenFGA's `--experimentals` flag is **not empty by default** — it ships as
`["pipeline_list_objects"]`. The lab's intent is to disable it, and the local
Docker Compose lab does so successfully via `OPENFGA_EXPERIMENTALS: ""`
(`docker-compose.yml`).

In Kubernetes this could not be achieved:

- passing `--experimentals=` as a container arg is ignored;
- setting it via environment variable produced a duplicated value.

The Helm template that renders all three paths
(`infra/aks/chart/templates/openfga.yaml`) consequently does not carry the flag,
while the standalone manifest `infra/aks/lab.yaml` still does — a discrepancy
worth knowing if you diff the two.

Impact assessment:

- The experimental flag is **identical across A, B and C**, because all three are
  rendered from the same template. **A/B/C remain comparable with each other.**
- AKS results are **not** comparable with local-lab results on this axis.
- The flag only affects `ListObjects`, i.e. **workload W4 only**. W1, W2, W3 and
  W7 are unaffected.

W4 has not been executed on any rung, so no published number is currently
contaminated by this. If W4 is run on AKS, the flag state must be stated in the
result.

### 6.3 Spot instances — evictions invalidate windows

The AKS `userpool` is **Spot** with **Deallocate** eviction policy
(`infra/aks/chart/values.yaml`). An eviction in the middle of a measurement
window invalidates that run, the same way a mid-window PgCache restart zeroes the
cumulative counters in the local lab.

`scripts/campaign-aks.sh` therefore calls `check_health()` after **every** run:

```bash
ev=$(kubectl -n "$NS" get events --sort-by=.lastTimestamp 2>/dev/null \
      | grep -iE 'preempt|evict|oomkill|NodeNotReady' | tail -5)
```

plus a count of pods not in `1/1 Running`. Findings are written to the campaign
log with a `!! CONTAMINACAO POSSIVEL apos <tag>` marker; the script does not
auto-discard, by design — it records what happened and the analysis decides. The
full event stream is dumped to `benchmark-docs/runs/events-<stamp>.txt` at the
end of the campaign.

**A run whose window overlaps a Preempted/Evicted/OOMKilled/NodeNotReady event is
discarded and redone.** This is a manual step and is the weakest link in the AKS
protocol.

### 6.4 Others carried over from `PLAN.md` §9

| Confound | Why it is a risk | Current state |
|---|---|---|
| Single machine (local lab) | 8 vCPU shared between origin, pgcache, 3× OpenFGA and the generator | Mitigated by fixed `cpus:`/`mem_limit` and one-path-at-a-time; not eliminated. The real fix is one role per machine, which is what the AKS topology does. |
| Local origin | Between containers on one host, the round-trip a cache hit avoids is ~0.1 ms — the condition where PgCache has least to gain, by construction | Not fixable in that topology. A remote origin (1–3 ms × 124–480 queries per Check) is what could flip the sign. |
| `latest` image tags | `latest` does not identify a build | `versions.txt` records digests per run; the AKS chart still pins `latest` in `values.yaml`. **Pin digests before publishing any number.** |
| Cluster auto-upgrade | A node re-image mid-suite kills the measurement without warning | Must be suspended during campaign windows. Manual. |
| `docker stats` / node CPU sampling | The first campaign sampled CPU **after** the run, on an idle system, and the resulting CPU claim was retracted in the `PLAN.md` §12 errata | Local: `docker-stats.csv` now sampled during the window. AKS: the `node-stats.csv` sampler **is not yet written**. Until it is, no CPU-attribution claim can be made on AKS. |
| Coverage | W4, W5 and W6 have not been executed on any rung | Open. |

---

## 7. Validity checklist

Run through this before publishing any number. If any item fails, **the number is
void** — record the limitation instead of publishing.

### 7.1 Universal (translated from `docs/RUNBOOK.md` §8)

- [ ] `make test` passed — oracle and document-locality regression guards
- [ ] Dataset validated post-seed: 600/600 samples, zero divergences
- [ ] W7 gate passed: `100.000000%` agreement, A vs B and both vs the oracle
- [ ] PgCache memory limit below the host's physical RAM (a cgroup above physical
      RAM never cuts: the process grows until the machine is exhausted and
      restarts mid-window, zeroing the counters)
- [ ] No foreign containers/workloads during the measurement, or the
      contamination is recorded in the report
- [ ] Warm-up reached the knee — no `AVISO: teto ... sem estabilizar`
- [ ] `drop_ratio < 10%` on **every** path being compared
- [ ] PgCache counter deltas are positive (a negative delta means it restarted
      mid-window — contaminated run, discard)
- [ ] All three paths at the **same target rate** and the same `doc_dist`
- [ ] ≥ 3 repetitions per combination
- [ ] `versions.txt` carries image digests — `latest` does not identify a build
- [ ] Window CPU samples checked: nothing pinned at its own limit (`cpu_perc` is
      relative to **one** core: 140% in a `cpus: 4.0` container is 35%
      utilization; 202% in a `cpus: 2.0` container is total saturation)
- [ ] Amplification (`queries_per_request`) compared **before** latency

### 7.2 AKS-specific

- [ ] No `Preempted` / `Evicted` / `OOMKilled` / `NodeNotReady` event overlapping
      any measurement window (`check_health()` output in the campaign log;
      full dump in `benchmark-docs/runs/events-<stamp>.txt`)
- [ ] All pods `1/1 Running` after each run
- [ ] All six pods on the intended nodes, one pod per node — verify with
      `kubectl get pods -o wide`; the isolation requirement is a pod per node,
      except for A/B/C which share a node and are never loaded simultaneously
- [ ] `networkMode` recorded (`bare` vs `cni`), and results from the two modes
      kept in **separate** `all-runs.csv` files — `report.py` does not separate
      them (§4.5)
- [ ] Cluster auto-upgrade suspended for the duration of the campaign
- [ ] PgCache Deployment restarted before **each** path-B repetition
      (`cold_pgcache()`), and `rollout status` confirmed ready
- [ ] Image digests pinned in `values.yaml` (currently `latest` — open item)
- [ ] Origin disk parameters cited: Premium SSD v2, `iops: 8000`, `mbps: 400` —
      at E3 the origin becomes I/O bound by design, so the disk is an
      experimental variable
- [ ] Node RTT measured and recorded (`./cluster.sh rtt` → `lab-params.env`)
      before the campaign; without it, network cost cannot be separated from
      cache effect
- [ ] `experimentals` state stated explicitly if any W4/ListObjects number is
      published (§6.2)
- [ ] Per-window node CPU/memory sampling available — **currently not
      implemented**; if absent, no CPU-attribution claim may be made

---

## 8. Regression guard: document locality

`tools/cmd/fgabench/generator_test.go` exists to make one specific class of
invalid benchmark fail the build rather than fail silently. The bug it locks is
described in doc 06; the short form is that the generator's document axis was
effectively uniform when it was supposed to be Zipf, so there was no repeated
`object_id`, therefore no repeated `WHERE`, therefore the benchmark was not
exercising the only thing a read cache does.

Three tests:

| Test | Asserts | Why |
|---|---|---|
| `TestDocLocality` | With `-doc-dist=zipf`, distinct documents < 75% of 1,600 requests, for both W1 and W3 | Locks the regression. The 75% threshold is deliberately loose: it catches the bug without pinning the test to a specific Zipf implementation. |
| `TestUniformModeReproducesTheOldBug` | With `-doc-dist=uniform`, W3 yields > 90% distinct documents | Control. `uniform` is the legacy mode and must keep reproducing the old behavior; if the two modes ever converge, the before/after comparison loses meaning. |
| `TestNegativeDocIsActuallyNegative` | Over 5,000 draws, no W3 pair is authorized under the oracle (`p.CanView`) | The locality optimization in `negativeDoc` reuses the seed when it is already denied; this guarantees it never smuggles a positive pair into W3, which is 100% negative by definition. |

The test comments record the measured asymmetry, which is itself methodologically
useful: W1 suffered less than W3 because `PositiveDocFor` already restricts the
codomain to documents the user can see, so it repeated (~37% distinct) even when
seeded with the sequence number. W3 had nothing constraining it — **99.4%
distinct documents in 1,600 requests**, i.e. essentially no repeated `WHERE` at
all. W3 is the maximum-amplification workload and was therefore the one most
damaged.

`make test` is a mandatory pre-flight step in `docs/RUNBOOK.md` §5.1, not a
formality: it has already caught two real modeling errors (the root folder
granting access to the root group, and the existence of omniscient users).

---

## 9. Where the raw data is

If you want to check a percentile this report does not compute, it is in the raw
files.

| Artifact | Content |
|---|---|
| `results/<rung>/<workload>/<path>-<run_id>/latencies.csv` | **One line per request**: `latency_ms,ok,allowed` |
| `.../summary.json` | Structured run summary |
| `.../docker-stats.csv` | `ts,name,cpu_perc,mem_usage`, sampled **during** the window |
| `.../versions.txt` | service → image → digest |
| `.../pg_stat_statements.psv` | Most expensive SQL on the origin, `\x01`-separated |
| `.../openfga-metrics-{before,after}.txt` | Raw `/metrics`, bracketing the window |
| `.../pgcache-metrics-{before,after}.txt` | Same, path B only |
| `results/all-runs.csv` | One line per run, 26 columns (see `docs/RUNBOOK.md` §6) |
| `benchmark-docs/runs/campaign-<stamp>.log` | Full AKS campaign transcript, including health checks |
| `benchmark-docs/runs/events-<stamp>.txt` | Kubernetes event stream for the campaign window |

New columns are always appended at the **end** of `all-runs.csv`; `fgabench`
rewrites the header itself when it detects a stale one, because otherwise
`report.py`'s `DictReader` would discard the extra columns in silence.

---

## 10. Summary of what these numbers can and cannot support

**Can support**, when the checklist passes:

- Correctness claims about PgCache versus baseline under quiescent writes (H4) —
  this is the one result unaffected by the errata that invalidated the first
  campaign's performance conclusions.
- Relative latency between A, B and C **at the same rung, workload, target rate
  and `doc_dist`**, provided all compared paths are under the 10% drop gate and
  their amplification is comparable.
- Throughput statements of the form "path X did not sustain N rps at rung E",
  which is what a high drop ratio actually measures.

**Cannot support** as of this writing:

- Any decomposition of path B's cost into "protocol" versus "cache" — the
  `simple_protocol` confound (§6.1) has not been isolated.
- Any CPU-attribution claim on AKS — per-window node sampling is not implemented
  (§6.4).
- Any `ListObjects` (W4) claim comparing AKS to the local lab — the
  `pipeline_list_objects` flag differs between them (§6.2). W4 has not been run
  anyway.
- Any latency comparison at E3/W1 at 40 rps — all three paths collapsed (§3).
- Any cross-network-mode comparison unless the two `all-runs.csv` files were kept
  separate (§4.5).

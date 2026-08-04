# Campaign r5 — AKS, 2026-08-03

**One document, one campaign.** Every number below comes from campaign `r5` and
nothing else. The earlier AKS campaigns are in
[05-results-aks-earlier.md](05-results-aks-earlier.md), kept separate because
they ran with defects that were still open at the time — mixing them into one
table is how you end up comparing two different experiments.

| | |
|---|---|
| **Campaign id** | `r5` |
| **Ran** | 2026-08-03, 16:13–18:08 UTC (114 min) |
| **Environment** | AKS `eks-1`, `brazilsouth-1`, node pool `userpool` (`Standard_D8as_v5`, 8 vCPU / 32 GiB, Spot) |
| **Placement** | one pod per node — origin, PgCache, the three OpenFGA servers, generator |
| **Network mode** | `bare` (hostNetwork + headless Service) |
| **Rung** | E1 only, 84,598 tuples |
| **Phases** | 1 (W1/W2/W3, zipf, 3 reps), 2 (W1/W3, uniform control, 3 reps), 5 (rate ramp 20/80/160/320) |
| **Runs** | 52 recorded of 57 planned — the 5 missing are all path B, see *Gaps* |
| **Request errors** | **0**, in all 52 runs |
| **Raw artifacts** | [`results-aks-r5/`](results-aks-r5) |

The three paths, as always: **A** `baseline` (no cache), **B** `pgcache`,
**C** `appcache` (OpenFGA's own in-process cache, which ships disabled).

---

## The gates, before any timing

Nothing in this document was measured until these passed. In r5 they are real
abort conditions — earlier campaigns printed a failing gate into a pipe and
carried on measuring on top of it.

**Mass against the analytical oracle.** 300 positive and 300 negative samples,
zero divergences, zero transport errors. The dataset is defined structurally, so
any (user, document) authorization is derivable in O(depth) without querying
anything — the oracle is not a second implementation, it is arithmetic.

**Differential correctness, path A against path B.** 4,000 (user, document)
pairs:

```
amostras comparadas : 4000
concordancia A==B   : 4000 (100.000000%)
divergencias        : 0
A bate com oraculo  : 4000 (100.000000%)
B bate com oraculo  : 4000 (100.000000%)
erros de transporte : 0
```

**PgCache is semantically transparent.** That result has now held in every
campaign, in both environments, without a single exception. For an authorization
system it is the result that matters most — a wrong cached answer is a security
incident, not a performance regression. It is also no longer the open question.

---

## At 40 rps, the picture is stable and unflattering

Medians over three repetitions, E1, zipf distribution, 60-second windows,
milliseconds:

| workload | path | p50 | p95 | p99 | queries/req | hit ratio |
|---|---|---:|---:|---:|---:|---:|
| W1 | baseline | 4.24 | 9.44 | **13.06** | 104.4 | — |
| W1 | pgcache | 5.46 | 18.77 | **25.16** | 109.3 | 90.2% |
| W1 | appcache | 3.53 | 11.11 | **16.37** | 67.0 | — |
| W2 | baseline | 6.51 | 9.56 | **11.81** | 171.1 | — |
| W2 | pgcache | 8.75 | 17.71 | **21.94** | 174.6 | 76.9% |
| W2 | appcache | 7.35 | 10.83 | **15.05** | 146.2 | — |
| W3 | baseline | 5.44 | 8.73 | **10.20** | 124.5 | — |
| W3 | pgcache | 9.93 | 22.92 | **28.68** | 124.5 | 89.9% |
| W3 | appcache | 4.75 | 9.41 | **11.00** | 71.2 | — |

PgCache costs between 1.7× and 2.8× the baseline p99 while hitting its cache 77
to 90% of the time.

The column that explains it is `queries/req`. PgCache serves **the same number of
datastore queries as the baseline** — 109 against 104, 175 against 171, 124.5
against 124.5. It does not remove work from the request path; it relocates where
the work is answered. And its own store is a full PostgreSQL instance, so a
"hit" is a complete SQL round trip to a local database, not a memory read.

OpenFGA's own cache removes the work outright. Its query count drops to 67, 146
and 71 — between a third and a half less work reaching any database at all. That
is the entire reason it wins.

The W2 pgcache row is a median over two repetitions; one was lost to D-22.

---

## The uniform control: document locality does not drive the hit ratio

Phase 2 repeats W1 and W3 with `doc-dist=uniform` instead of `zipf`. Under zipf
roughly a third of the documents touched in a run are distinct; under uniform in
W3 it is 99.4%. If PgCache's cached shapes were keyed largely on the document,
tripling the document spread should shred the hit ratio.

It does not move:

| workload | distribution | pgcache hit ratio | pgcache p99 | baseline p99 |
|---|---|---:|---:|---:|
| W1 | zipf | 90.2% | 25.16 | 13.06 |
| W1 | uniform | 90.4% | 26.31 | 15.31 |
| W3 | zipf | 89.9% | 28.68 | 10.20 |
| W3 | uniform | 90.0% | 23.87 | 11.61 |

Two tenths of a percentage point in one case, one tenth in the other.

This inverts something we had assumed. The hundred-odd queries behind a single
`Check` are dominated by lookups that do **not** vary with the document — group
membership for the user, folder hierarchy walks, the model itself. Document-keyed
queries are a minority of the amplification, so widening their spread barely
touches what gets cached.

It also means D-01, the document-locality bug we fixed in the workload generator
and treated as a correctness issue, does not change the PgCache verdict. Worth
stating plainly, because we had expected it might.

The W1 uniform pgcache row is a median over two repetitions; one was lost to
D-22.

---

## The rate ramp: the penalty is not bounded, it grows

This phase had failed twice before — once contaminated by a mid-campaign
deployment, once killed by a Spot node. W1, zipf, one run per rate except at 40
rps where phase 1 supplies three:

| rate | path | achieved | p50 | p99 | drops |
|---|---|---:|---:|---:|---:|
| 20 | baseline | 20.0 | 4.28 | 15.53 | 0.0% |
| 20 | appcache | 20.0 | 4.09 | 19.11 | 0.0% |
| 40 | baseline | 40.0 | 4.24 | 13.06 | 0.0% |
| 40 | pgcache | 40.0 | 5.46 | 25.16 | 0.0% |
| 40 | appcache | 40.0 | 3.53 | 16.37 | 0.0% |
| 80 | baseline | 80.0 | 4.09 | 13.31 | 0.0% |
| 80 | appcache | 80.0 | 3.00 | 17.49 | 0.0% |
| 160 | baseline | 160.0 | 3.82 | **16.17** | 0.0% |
| 160 | pgcache | 159.9 | 7.93 | **159.92** | 0.1% |
| 160 | appcache | 160.0 | 2.77 | **19.51** | 0.0% |
| 320 | baseline | **166.2** | 7665.76 | 9347.78 | **48.1%** |
| 320 | appcache | 320.0 | 2.69 | 29.11 | 0.0% |

Three things fall out.

**PgCache's penalty scales with load.** At 40 rps it is 1.9× the baseline p99. At
160 rps it is **9.9×** — 159.92 ms against 16.17 ms — with drops still under
0.1%, so these are honest percentiles and not survivor bias.

This retracts a claim published in the earlier campaigns. We wrote that the
penalty was "bounded" on dedicated hardware. It was bounded *at the rate we
happened to be measuring*, which is a different statement. Measuring one point on
a curve and describing it as a ceiling is the kind of error that stays invisible
until you measure a second point.

**The baseline saturates between 160 and 320 rps.** Asked for 320 it delivered
166.2 and dropped 48.1% of the offered load. Percentiles there are meaningless —
dropping half the requests removes precisely the ones that would have been
slowest — so the 7.6-second p50 should be read as *it fell over*, not as a
latency. The finding is the achieved rate, not the distribution.

**OpenFGA's own cache sails through it.** At 320 rps path C held the full rate
with three drops and a p99 of 29.11 ms, while the uncached origin collapsed. The
competitor that ships disabled is, on every axis we currently measure, winning.

---

## Gaps in this campaign

**Five runs missing, all path B.** 52 of 57. The cause is D-22: a stale DNS
answer at the generator's own preflight, never mid-window, so nothing that *was*
measured is affected — those runs simply did not happen.

| condition | runs | paths present |
|---|---|---|
| W1 zipf 40 rps | 9 | A B C |
| W1 uniform 40 rps | 8 | A B C — one B repetition missing |
| W2 zipf 40 rps | 8 | A B C — one B repetition missing |
| W3 zipf 40 rps | 9 | A B C |
| W3 uniform 40 rps | 9 | A B C |
| W1 zipf 20 rps | 2 | A C — **B missing** |
| W1 zipf 80 rps | 2 | A C — **B missing** |
| W1 zipf 160 rps | 3 | A B C |
| W1 zipf 320 rps | 2 | A C — **B missing** |

The 160 rps point — the one carrying the scaling claim — is intact.

**PgCache's own collapse point is unknown.** Its 320 rps run is one of the three
lost. Given the trend it is probably below the baseline's, but "probably" is not
a measurement.

**Only E1 was measured.** Phases 1, 2 and 5 all run against the 84,598-tuple
rung. E2 (≈1M tuples) was not re-run in r5, so this document says nothing about
scale.

**One experimental flag we cannot turn off.** OpenFGA ships with
`pipeline_list_objects` enabled and it cannot be disabled in Kubernetes — the
flag syntax that works in Docker Compose is ignored as a container argument, and
setting it via environment variable duplicates the value instead of clearing it.
It is identical across all three paths, so A/B/C comparisons hold, but
cross-environment comparison with the laptop is imprecise. Tracked as D-14, still
open.

---

## What had to be fixed for this campaign to finish at all

r5 is the first AKS campaign to run start to finish, and that is not luck. All of
the following are written up with evidence in
[06-defect-log.md](06-defect-log.md):

- **D-19** — `RollingUpdate` deadlocks a one-pod-per-node lab. All five lab
  deployments are now `Recreate`, and PgCache restarts *before* the path-B server
  rather than alongside it.
- **D-20** — `pgcache_cache_reg_gate_loading` is a gauge that leaks; it sat at 25
  with the stack completely idle while PgCache answered `select count(*)` in
  57 ms. Readiness is now gated on observable end-to-end behaviour instead.
- **D-21** — the contamination alarm matched the word *preemption* in the message
  of every Kubernetes `FailedScheduling` event, with no time bound, so it fired
  after every run and would never have flagged a real Spot eviction.
- **D-18** — `run_id` collided across campaigns, so `all-runs.csv` accumulated
  rows from different campaigns under identical labels. Every run now carries a
  `campaign_id` and each campaign writes to its own directory. Verified here: 52
  runs, 52 summary files, 53 CSV lines, one to one.
- **Reproducibility** — `psql` was previously installed by hand inside the
  generator pod and vanished on every pod recreation; it now comes from the
  chart. And the seed's correctness gates now actually abort the campaign instead
  of printing a failure into a pipe.

---

## Reading the fine print

**Images are `latest`.** `pgcache/pgcache:latest`, `openfga/openfga:latest`,
`postgres:17`. Digests are not pinned, so this campaign is not byte-reproducible
against a future pull.

**Spot nodes.** The lab runs on evictable VMs. No eviction occurred inside a
measurement window — but the alarm that was supposed to prove that is the one
broken by D-21, so the evidence is the absence of pod restarts and the zero error
count, not the alarm.

**No CPU limits, only requests.** A CPU limit becomes a CFS quota in 100 ms
windows; a pod that exceeds it freezes until the window turns, and that appears
as a p99 spike indistinguishable from cache-miss cost. With one pod per node the
isolation comes from the node. Note that `cpuCfsQuota: false` is *not* active on
the deployed pools — both report `kubeletConfig: null` — so the guarantee rests
entirely on the chart declaring requests without limits (D-16).

**Path B carries a known confound.** It runs with `execMode: simple_protocol`,
without which PgCache classifies ~99.97% of queries as non-cacheable. So path B
is not "path A plus a cache" — it is "path A without prepared statements, plus a
cache".

# Campaign r6 — PgCache configured the way its documentation says to

**This campaign retracts the headline of every previous one.** Run stock and
under-warmed, PgCache lost on latency in all nine cells we measured and appeared
to degrade badly under load. Run with pinned tables and warmed to the hit-ratio
knee, it sits within 5–35% of an uncached origin, beats OpenFGA's own cache in
four of the seven conditions where all three paths are comparable, and the
"penalty grows with load" finding from r5 does not survive at all.

| | |
|---|---|
| **Campaign id** | `r6` |
| **Ran** | 2026-08-03, 19:15–21:52 UTC (157 min) |
| **Environment** | identical to [r5](05-results-aks-r5.md) — AKS `eks-1`, `brazilsouth-1`, `userpool` Spot `Standard_D8as_v5`, one pod per node, `networkMode: bare` |
| **Rung** | E1, 84,598 tuples |
| **Phases** | 1 (W1/W2/W3, zipf, 3 reps) and 5 (rate ramp 20/80/160/320) |
| **Runs** | **39 of 39** — the first campaign in this lab with no missing runs |
| **Request errors** | **0** |
| **Raw artifacts** | [`results-aks-r6/`](results-aks-r6) |

**What changed relative to r5, and nothing else did:**

| | r5 | r6 |
|---|---|---|
| `PINNED_TABLES` | not set | `tuple,authorization_model` |
| warm-up | 45 s fixed | adaptive, to the hit-ratio knee, ceiling 7 min |

Same cluster, same seed, same workloads, same rates, same images, same three
paths. Both knobs come straight from the vendor documentation and neither had
ever been exercised — see [07-pgcache-configuration.md](07-pgcache-configuration.md)
for how we found that out, and D-24 for the write-up.

---

## Correctness is unaffected by pinning

The gate ran before any timing, as always: 300 positive and 300 negative oracle
samples with zero divergences, then 4,000 (user, document) pairs across paths A
and B — **100.000000% agreement, zero divergences**, both matching the oracle.

This mattered more than usual here. Pinned entries are, by the documentation,
"updated in place and never invalidated", which is exactly the shape of thing that
could go stale silently. It didn't.

---

## Steady state: 40 rps, medians of three repetitions

p99 in milliseconds, E1, zipf:

| workload | path | r5 (stock, 45 s) | **r6 (pinned, knee)** | change |
|---|---|---:|---:|---:|
| W1 | baseline | 13.06 | 12.76 | — |
| W1 | **pgcache** | 25.16 | **15.25** | **−39%** |
| W1 | appcache | 16.37 | 16.24 | — |
| W2 | baseline | 11.81 | 11.90 | — |
| W2 | **pgcache** | 21.94 | **12.92** | **−41%** |
| W2 | appcache | 15.05 | 13.83 | — |
| W3 | baseline | 10.20 | 9.72 | — |
| W3 | **pgcache** | 28.68 | **13.12** | **−54%** |
| W3 | appcache | 11.00 | 10.61 | — |

The baseline and appcache columns barely move, which is the control we needed: the
cluster did not get faster between campaigns. Only path B changed, because only
path B's configuration changed.

**Penalty over an uncached origin:** W1 from 1.93× to **1.20×**, W2 from 1.86× to
**1.09×**, W3 from 2.81× to **1.35×**.

**PgCache now beats OpenFGA's own cache in W1 (15.25 vs 16.24) and W2 (12.92 vs
13.83).** That had never happened in this lab. It still loses W3, the deny-heavy
workload, where the origin is fastest to begin with and there is least to gain.

### The hit ratio goes down while latency improves

| workload | r5 hit ratio | r6 hit ratio |
|---|---:|---:|
| W1 | 90.2% | 83.2% |
| W2 | 76.9% | 82.9% |
| W3 | 89.9% | 79.0% |

In two of three workloads the cache hits *less often* and yet the tail is
dramatically better. This is the third independent time the pattern has appeared,
and it is the clearest confirmation of the mechanism we have: PgCache's problem in
this workload was never the cost of a hit, it was **cost of miss × amplification**
— roughly 110 datastore queries per `Check`, so any per-miss penalty is multiplied
about a hundredfold. Pinning does not create more hits. It makes misses cheap.

Anyone tuning this should stop optimising for hit ratio. It is the wrong number.

---

## Under load: the r5 conclusion does not survive

W1, zipf, one run per rate.

| rate | path | achieved | p50 | p99 | drops | r5 p99 |
|---|---|---:|---:|---:|---:|---:|
| 20 | baseline | 20.0 | 4.08 | 13.46 | 0.1% | 15.53 |
| 20 | pgcache | 20.0 | 5.62 | 20.42 | 0.1% | — |
| 20 | appcache | 20.0 | 3.81 | 16.54 | 0.1% | 19.11 |
| 80 | baseline | 80.0 | 3.96 | 13.29 | 0.0% | 13.31 |
| 80 | **pgcache** | 80.0 | 4.75 | **14.05** | 0.0% | — |
| 80 | appcache | 80.0 | 2.95 | 17.08 | 0.0% | 17.49 |
| 160 | baseline | 160.0 | 3.89 | 15.88 | 0.0% | 16.17 |
| 160 | **pgcache** | 160.0 | 4.54 | **16.74** | 0.0% | **159.92** |
| 160 | appcache | 160.0 | 2.74 | 19.04 | 0.0% | 19.51 |
| 320 | baseline | **173.1** | 7539.02 | 8899.37 | **45.9%** | (166.2, 48.1%) |
| 320 | pgcache | **158.1** | 8009.75 | 9024.35 | **50.6%** | — |
| 320 | appcache | 320.0 | 2.86 | 29.69 | 0.0% | 29.11 |

**At 160 rps, stock PgCache was 159.92 ms. Configured, it is 16.74 ms.** Same
cluster, same workload, same rate — a 9.6× difference from two configuration
values. r5 concluded that "PgCache's penalty scales with load: 1.9× at 40 rps,
9.9× at 160". That conclusion was measuring the stock configuration's warm-up
deficit, not a property of PgCache. At 80 and 160 rps the configured penalty is
1.06× and 1.05× — effectively parity with no cache at all, and better than
appcache at both rates.

**At 320 rps everything except appcache falls over.** The baseline delivered 173.1
of 320 rps with 45.9% drops; PgCache delivered 158.1 with 50.6%. Percentiles above
a 10% drop ratio are meaningless — dropping half the load removes exactly the
requests that would have been slowest — so read those two rows as *collapsed*, not
as latencies. PgCache saturates slightly earlier than the uncached origin (158.1
against 173.1 achieved), which is a real if modest cost.

Path C held the full 320 rps with a 29.69 ms p99 and zero drops. Under saturation
the cache that removes queries outright still wins, and it is not close.

**The 20 rps row is the odd one.** PgCache is worse there (20.42) than at 80 or 160
(14.05, 16.74), which is backwards. The likeliest explanation is that a thinner
request stream keeps fewer entries hot inside the measurement window, but each ramp
point is a **single run** and this is well inside the range where one run proves
nothing. Flagged, not explained.

---

## The bill: warm-up time

This is the finding to carry into production, and it is not free.

| workload | knee, stock | knee, pinned |
|---|---:|---:|
| W1 | 2m30s | 6m00s |
| W2 | — | **7m30s, ceiling hit, never converged** |
| W3 | — | 5m15s |

Every campaign before this one warmed for **45 seconds**. The stock knee is at
2m30s; with pinning it is between five and seven-plus minutes. W2 — the
uniform-random workload with no locality — hit the 7-minute ceiling in all three
repetitions, and the generator said so each time:

```
[B/w2] AVISO: teto de aquecimento (7m0s) atingido sem estabilizar
       — a janela de medicao pega cache ainda em formacao
```

So W2's numbers above come from a cache that was **still forming**, and they are
still the second-best result in the table. That makes them conservative, but they
are not a converged measurement.

Practically: a pinned PgCache pod that restarts serves degraded traffic for five to
seven minutes, against two and a half for a stock one. If you run this behind a
rolling deploy, budget for it — and note that our own lab had to move every
deployment to `Recreate` (D-19) for unrelated reasons, which makes the cold window
unavoidable rather than maskable.

---

## What we still would not claim

**The ramp is n=1 per point.** The 160 rps result is a 9.6× improvement over r5 and
the direction is unambiguous, but three repetitions would make it a result rather
than a strong signal.

**W2 never converged.** Its knee is above our 7-minute ceiling. Re-running phase 1
with a 15-minute ceiling would close that.

**Only E1, only zipf.** E2 (≈1M tuples) has not been measured with pinning at all,
and the uniform control (phase 2) was skipped here to keep the runtime under three
hours. The shape-space argument predicts pinning matters *more* at scale, not less,
but that is a prediction.

**`ADMISSION_THRESHOLD` is still untested.** It remains the most obvious untried
knob, aimed squarely at W3 and the uniform control, where most admitted entries are
never reused.

**Path B still carries `simple_protocol`.** Without it PgCache classifies ~99.97%
of these queries as non-cacheable, so it is required — but it means path B is "path
A without prepared statements, plus a cache", not "path A plus a cache". That
confound is unchanged by anything here.

---

## Summary for someone deciding whether to deploy this

At the load levels we measured, with the documented configuration applied and the
cache actually warm:

- **Correctness: unconditional pass.** Twelve thousand differential comparisons
  across three campaigns, zero divergences.
- **Latency at 40–160 rps: parity to a 35% p99 penalty**, beating the
  application's own cache in four of seven comparable conditions.
- **Latency under saturation: it does not save you.** At 320 rps PgCache collapsed
  alongside the uncached origin while the in-process cache held the full rate.
- **Operational cost: five to seven minutes of cold start**, against two and a half
  without pinning.
- **Do not tune for hit ratio.** In this workload it moves opposite to latency.

The most important thing we learned is not about PgCache at all: we published nine
cells of "PgCache loses" measured against a configuration the vendor's own
documentation argues against, and we did it for three campaigns before reading that
documentation carefully.

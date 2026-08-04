# We had been benchmarking PgCache's default configuration

**Probe date:** 2026-08-03, immediately after campaign r5, same cluster, same
seeded dataset (E1, 84,598 tuples), same workload (W1, zipf, 40 rps, 60-second
window). The only thing that changes between runs is PgCache's own configuration.

Raw artifacts: [`results-config-probe/`](results-config-probe).

---

## The problem with every number we have published so far

Up to and including campaign r5, the chart passed PgCache exactly six environment
variables:

```
UPSTREAM_URL  LISTEN_PORT  METRICS_PORT  NUM_WORKERS  ALLOWED_TABLES  PGCACHE_TELEMETRY
```

Everything else ran at the product's defaults. The vendor documentation
(pgcache.com/docs) describes several knobs, and two of them target precisely the
weaknesses our workload exposes:

| Knob | Env var | Default | Were we using it? |
|---|---|---|---|
| Pinned tables | `PINNED_TABLES` | — | **No** |
| Pinned queries | `PINNED_QUERIES` | — | No |
| Admission threshold | `ADMISSION_THRESHOLD` | `1` | No (default) |
| Cache policy | `CACHE_POLICY` | `clock` | No (default) |
| Materialization size gate | `MV_SIZE_RATIO` | `10` | No (default) |
| Memory / disk limit | `MEMORY_LIMIT` / `DISK_LIMIT` | 80% RAM / auto | No (default) |

Publishing "PgCache loses" while running it stock is the same unfairness as
running path C with OpenFGA's cache disabled. We would not have accepted that
from someone else's benchmark.

### On "the result only comes from the second execution"

This is close to right, but the default is the opposite. The documentation states
that `admission_threshold` defaults to **1** — a cacheable query is admitted on
its **first** occurrence. Raising it is the documented remedy for cache
pollution:

> "higher values delay admission until the query has been seen that many times,
> which keeps one-off queries out of the cache at the cost of some initial cache
> misses."

So there is no built-in two-execution rule at our settings. What genuinely costs
on first occurrence is **shape registration** — PgCache determines eligibility
"by query shape at registration", and registering a shape means pulling the
relevant data in. That is the blocking behaviour we diagnosed independently as
D-20, arriving at the same mechanism from the failure side.

---

## Finding 1 — our warm-up was too short, and we already knew better

The generator has had adaptive warm-up since D-08: warm in slices until the
hit-ratio knee, rather than for a fixed duration. It is enabled by `-warmup-max`.
**The campaign never passes it.** With `-warmup-max` at its default of 0 the code
falls straight through to the fixed path, and all 52 runs of campaign r5 recorded
`warmup_s = 45`.

D-08 was marked *fixed* in the defect log. The fix existed as unreachable code on
the campaign path. Running the adaptive warm-up for the first time shows what
that cost us:

```
30s:   hit ratio da fatia 76.0%
1m0s:  hit ratio da fatia 88.9%
1m30s: hit ratio da fatia 90.3%
2m0s:  hit ratio da fatia 91.7%
2m30s: hit ratio da fatia 92.7%
joelho em 2m30s (delta 0.0093 < 0.0100)
```

**The knee is at 2m30s. We were warming for 45 seconds.** At 45 s the cache is
still climbing steeply — the 30-second slice sits at 76% and the 60-second slice
at 88.9%. Every PgCache number in every campaign so far was measured on a cache
that had not finished forming.

Correcting only this, with nothing else changed:

| | p95 | p99 | max | hit ratio |
|---|---:|---:|---:|---:|
| r5, 45 s fixed warm-up | 18.77 | 25.16 | — | 90.2% |
| knee warm-up (2m30s) | 16.31 | **22.57** | 40.91 | 93.0% |

About 10% off the p99 and nearly three points of hit ratio, purely from warming
properly. It does not change the ranking — the baseline is at 13.06 — but it
means our published PgCache numbers were pessimistic by roughly that margin.

---

## Finding 2 — pinned tables move the tail, and not in the direction the hit ratio suggests

With `PINNED_TABLES=tuple,authorization_model` and nothing else changed:

| run | config | p95 | p99 | max | hit ratio | queries/req |
|---|---|---:|---:|---:|---:|---:|
| `cfg-default / default-r1` | stock | 16.31 | 22.57 | 40.91 | 93.0% | 109.0 |
| `cfg-pinned / pinned-r1` | stock¹ | 16.76 | 22.27 | 54.55 | 93.1% | 110.8 |
| `cfg-pinned2 / pinned-r1` | **pinned** | **10.89** | **15.81** | **20.55** | **78.2%** | 109.2 |

¹ The `cfg-pinned` run is stock, not pinned: the `helm --set` meant to apply the
pinning failed on comma parsing and the upgrade aborted, which we only caught by
inspecting the pod's environment afterwards. It is reported because an accidental
duplicate of the control is worth having — the two stock runs land at p99 22.57
and 22.27 with hit ratios of 93.0% and 93.1%, about as tight a repeatability
check as this lab has produced.

Read the pinned row against **either** control and the tail collapses: p99 from
~22.4 to 15.81, p95 from ~16.5 to 10.89, and the worst single request from
40–55 ms down to 20.55 ms. Against the r5 baseline for the same workload (p99
13.06) the PgCache penalty falls from about 1.7× to **1.21×** — and it lands
below path C's 16.37, which would be the first time PgCache beats OpenFGA's own
cache on any tail metric in this lab.

**The hit ratio went down, not up: 78.2% against 93.0%.** That is the interesting
part. Pinning did not make more requests hit; it made the misses cheap. Our whole
diagnosis of PgCache in this workload has been *cost of miss × amplification* —
roughly a hundred datastore queries per `Check`, so any per-miss penalty gets
multiplied about a hundredfold. Pinning attacks exactly that term, and tail
metrics are where a per-miss penalty shows up. Mechanism and measurement agree.

### Why this is a signal and not yet a result

- **One pinned run.** n=1 against n=2 controls. The effect is large and the
  controls are tight, but a single treatment run is a single treatment run.
- **The pinned run never reached its knee.** It hit the 3m30s ceiling still
  climbing (34.8 → 55.9 → 63.9 → 69.0 → 73.3 → 74.3 → 77.6%), and the generator
  said so: *"teto de aquecimento atingido sem estabilizar — a janela de medicao
  pega cache ainda em formacao"*. So this is an **under-warmed** pinned cache
  beating a **fully-warmed** stock one. Warming it properly should only help,
  which makes the comparison conservative — but it is not the comparison we
  intended to run.
- **A replication attempt with a 10-minute ceiling was lost** to a `kubectl exec`
  stream timeout from the operator's laptop at the two-minute mark. The warm-up
  curve it had produced by then (37.1 → 56.6 → 64.7 → 69.6%) tracks the first
  pinned run closely, which is mildly reassuring and nothing more.
- **Only W1 was probed.** W2, W3, the uniform control and every rate above 40 rps
  are untested under pinning.

---

## What this changes

The chart now exposes both knobs, defaulting to off so existing results stay
reproducible:

```yaml
pgcache:
  allowedTables: "tuple,authorization_model"
  pinnedTables: ""          # e.g. "tuple,authorization_model"
  admissionThreshold: ""    # product default is 1
```

Note for anyone applying this: `helm --set` splits on commas, so the value needs
`--set-string 'pgcache.pinnedTables=tuple\,authorization_model'`. Ours failed
into a successful-looking run instead, which is how we ended up with an
accidental control.

**Open questions, in the order we think they matter:**

1. Replicate the pinned result with a warm-up ceiling high enough to reach the
   knee, three repetitions, across W1/W2/W3.
2. Does pinning hold at 160 rps, where stock PgCache is 9.9× the baseline? That
   is where a reduction in per-miss cost should matter most.
3. `ADMISSION_THRESHOLD > 1` is untested and is aimed squarely at the uniform
   control and W3, where 99.4% of documents are distinct and most admitted
   entries are never reused.
4. Re-run the campaign with `-warmup-max` actually enabled, so that "PgCache" in
   our tables means a warmed PgCache.

Until at least (1) and (4) are done, **campaign r5's PgCache figures should be
read as a stock, under-warmed configuration.** That is a legitimate thing to
measure, but it is not the same claim as "PgCache is 1.9× slower than no cache".

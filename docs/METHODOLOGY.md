# Methodology Standard

The rules every project on this platform must follow. Each one exists because
violating it silently produced a wrong published number in the openFGA lab —
the defect IDs refer to `openFGA/benchmark-docs/06-defect-log.md`.

This is the project-agnostic distillation. Per-project detail (what each metric
means for that app, how to run it) lives in the project's own runbook.

---

## 1. The path model

Every project measures at least:

| Path | What it isolates |
|---|---|
| **A** `baseline` | The app talking straight to its origin Postgres. Real datastore cost. |
| **B** `pgcache` | The app talking through PgCache. Gain attributable **only** to the proxy. |
| **C** `appcache` | The app's own caching enabled, if it has any. The honest competitor. |

Path C is what makes the result meaningful. Without it the question degrades to
"does caching help?" — whose answer is always yes. If the app has no native
cache, say so explicitly in the STUDY and run A vs B only.

**Golden rule:** all paths hit the same origin, with the same data, in the same
window, from the same generator, at the same target rate. Numbers from different
conditions are never compared. One path under load at a time — parallel
measurement is not faster, it is corrupt.

## 2. Correctness before performance

A wrong cached answer is an incident, not a performance metric. Before any
latency measurement:

1. **Seed validation** — if the project has an oracle (a way to compute the
   correct answer independently), validate the seeded mass against it.
2. **Differential gate** — the same requests against paths A and B, responses
   compared pairwise. Target: 100.000000% agreement. Any divergence aborts the
   campaign and becomes a bug report.

An oracle is the single most valuable thing a project can have. openFGA's
analytical oracle (answers derivable in O(depth) without querying) is the gold
standard; a stored-snapshot diff is an acceptable fallback. A project with no
correctness check at all measures only speed, and must say so.

## 3. Load generation

- **Open-loop.** Arrivals scheduled at a fixed rate; latency measured from the
  *intended* arrival instant, never from when a worker was free. A closed-loop
  generator under saturation stops sending and makes a saturated system look
  fast (coordinated omission).
- **Overflow is dropped and counted**, not queued past the deadline.
- **Realistic key distribution.** Real traffic is skewed (Zipf); uniform access
  is the cache's worst case and belongs in a *control* workload, not the main
  one. Verify locality empirically — the openFGA generator claimed Zipf and
  delivered 99.4% unique keys (D-01). Lock it with a regression test.
- **Warm-up until the hit-rate knee**, not for a fixed time (D-08). Measuring a
  cold cache and calling it PgCache is fraud.
- **Cold cache per repetition.** Restart PgCache before each path-B repetition,
  and after every re-seed (D-15) — otherwise it serves the previous dataset.

## 4. Validity rules for the numbers

- **Amplification before latency.** Record datastore queries per request from
  the app's own metrics. If it differs between paths, they did not do the same
  work, and the latency comparison is void.
- **Drop ratio > 10% voids percentiles** (D-04). Drops remove exactly the
  requests that would have been slowest. Report "did not sustain the rate", not
  a p99.
- **Aggregation keys**: rung/scale, workload, target rate, key distribution,
  network mode. Median across ≥3 repetitions. Mixing conditions produces a
  median of nothing that ever ran (D-03, D-07).
- **Counters that go backwards mean a restart mid-window.** Discard the run
  (D-06).
- **Every run records its environment**: image digests (never trust `latest`),
  node SKUs, zones, disk IOPS, measured RTT between nodes. A number without its
  parameters is uninterpretable later.

## 5. Kubernetes placement

- **One pod per node** for origin, PgCache and the app. Enforce with pod
  anti-affinity on a role label (`operator: NotIn` — beware: a pod *missing*
  the label is repelled by everything, D-13) or dedicated tainted pools.
- **CPU requests only, never limits.** A CPU limit is CFS quota in 100 ms
  windows; throttling shows up as p99 spikes indistinguishable from the cache
  behaviour under test.
- **Memory ceilings must be real** — below what the node/host can actually
  give, or the process grows until the machine dies mid-window (D-06).
- **No DaemonSet agents on lab nodes.** Observability scrapes over the network
  from a different pool. That is why the Azure Monitor agents were disabled.
- **Spot nodes are usable** for short windows, but check events for
  `Preempted/Evicted/OOMKilled/NodeNotReady` after every run and discard
  contaminated windows.
- **Results must not live only in an `emptyDir`.** Snapshot after every phase
  and before anything that recreates a pod. Forty runs died learning this.

## 6. Sizing PgCache

PgCache embeds a full PostgreSQL as its cache store. Consequences:

- A cache **hit is a SQL round-trip** to that embedded Postgres, not a memory
  read. Against an origin serving from `shared_buffers` in ~10 µs, a hit only
  pays for itself when the avoided network round-trip is large. Topology is
  therefore an experimental variable, not plumbing.
- It is the hungriest component: 4.8 GiB at 85K tuples in the openFGA lab. Give
  it at least the origin's resources, memory-optimized if available.
- For the proxy to see queries, the client may need `simple_protocol` — which
  interpolates literals and makes the query-shape space grow with the data.
  This is a **known confound**: path B is "path A without prepared statements,
  plus a cache". State it in every report.

## 7. Deliverables per campaign

In `<project>/benchmark-docs/`, in English:

1. Architecture of the test (design, not outcomes)
2. Infrastructure actually used, with live-captured values
3. Results with the validity rules applied
4. **A defect log for the benchmark itself.** A benchmark that does not publish
   its own bugs is not auditable.

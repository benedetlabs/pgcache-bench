# Campaign s1 — pgbench on AKS

**Ran:** 2026-08-04, AKS `eks-1`, namespace `pgcache-synth`, chart revision 2.
**Rung P0:** pgbench scale 10 — 1,000,000 rows, `pgbench_accounts` 150 MB against
a 1 GB `shared_buffers`, so the origin serves entirely from its buffer pool.
**Phases:** w7, w0, w1, w2 · 2 repetitions · 45 s per cell · 48 cells, all
completed.

**PgCache wins, and the margin grows with load.** At 256 clients it delivers
**2.8× the throughput at 36% of the latency**, while the origin is past its own
peak and losing ground.

**There is no path C.** pgbench has no application tier and no cache of its own,
so this measures PgCache's envelope against an uncached origin. It is not an
adoption verdict for any application.

---

## Topology

Three roles, three separate nodes, enforced by anti-affinity. This is not a
nicety: path B runs origin **and** PgCache, path A runs only origin, so
co-locating them would hand path B less CPU for the origin than path A had, at
exactly the concurrency where the comparison is decided.

| role | node | zone |
|---|---|---|
| loadgen | `aks-userpool-...vmss00000p` | brazilsouth-1 |
| pgcache | `aks-userpool-...vmss00000q` | brazilsouth-1 |
| origin | `aks-userpool-...vmss00000r` | brazilsouth-1 |

All in **one zone**, checked before any number was believed — a cross-zone split
would have added tens of microseconds to one path and explained the entire
result on its own.

Nodes came from the autoscaler reacting to Pending pods
(`TriggeredScaleUp aks-userpool 1->4`). The pool itself was not modified.

Paths differ in `-h` and `-p`. Nothing else: same pgbench binary, same script,
same data, same node class.

---

## w7 — correctness gate · GREEN

Three passes, 2,000 statements each, one `psql` invocation per path per pass,
outputs diffed line by line.

| pass | statements | rows | divergences |
|---|---:|---:|---:|
| cold cache | 2,000 | 2,000 | **0** |
| warm cache | 2,000 | 2,000 | **0** |
| after `UPDATE pgbench_accounts … WHERE aid <= 5000` | 2,000 | 2,000 | **0** |

6,000 statements, zero divergences, including immediately after CDC
invalidation. Consistent with every campaign this platform has run: PgCache has
never once returned a wrong answer.

---

## w0 — calibration · O, H and M

Single client, so this is the cost of the path with no queueing anywhere.

| quantity | measured | conditions |
|---|---:|---|
| **O** — origin per query | **167 µs** | path A, `hot=1000` |
| **H** — PgCache on a hit | **119 µs** | path B at a 98.5% hit ratio |
| **M** — PgCache on a miss | **614 µs** | path B at a 5.9% hit ratio |

Both C10 conditions hold, and this is the first subject in the platform for
which that is true:

1. **`H < O`** — 119 µs against 167 µs. On openFGA this failed outright
   (H≈45 µs, O≈40 µs), which is why no hit ratio could ever have rescued it.
2. **break-even `= (M−O)/(M−H) = (614−167)/(614−119) =` 90.3%** at one client.

**The laptop was wrong about the direction, and usefully so.** Probe 0 measured
O=77 µs and predicted that a faster origin on real hardware would raise
break-even and might erase the win. The origin here is *slower*, at 167 µs, not
faster — because on AKS every query crosses a real node-to-node hop, which the
laptop's container-to-container path did not have. A cache hit crosses the same
single hop; a miss crosses two, which is why M rose further (614 µs against
174 µs) and break-even still ended up higher than the laptop's 82.9%.

---

## w1 — hot-set sweep · the break-even curve

8 clients. Median of 2 repetitions.

| hot set | hit ratio | A latency | A tps | B latency | B tps | tps | latency |
|---|---:|---:|---:|---:|---:|---:|---:|
| 1,000 | 98.7% | 237 µs | 33,734 | 152 µs | 52,550 | **+56%** | **−36%** |
| 10,000 | 88.6% | 241 µs | 33,240 | 154 µs | 51,798 | **+56%** | **−36%** |
| 100,000 | 17.3% | 258 µs | 31,108 | 463 µs | 17,310 | −44% | +80% |
| 1,000,000 | 1.7% | 237 µs | 33,872 | 567 µs | 14,134 | −58% | +139% |

The last row is pgbench's **default** `select-only` keyspace. Anyone
benchmarking a read cache with stock `pgbench -S` measures a 1.7% hit ratio and
concludes the cache is harmful. That is a keyspace result, not a caching result.

**Break-even at 8 clients is lower than at one.** Solving the mix at this
concurrency gives H≈146 µs, M≈574 µs against O≈245 µs, so break-even is **77%** —
which is why 88.6% still wins here and 90.3% was the bar at a single client.

---

## w2 — saturation sweep · the offload curve

`hot=1000` throughout, so the hit ratio is held near 98.6% and **concurrency is
the only variable**. Median of 2 repetitions.

| clients | A latency | A tps | B latency | B tps | tps | latency |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 196 µs | 5,106 | 115 µs | 8,681 | +70% | −41% |
| 8 | 255 µs | 31,391 | 154 µs | 51,930 | +65% | −40% |
| 16 | 321 µs | 49,988 | 178 µs | 89,882 | +80% | −44% |
| 32 | 516 µs | **62,078** | 265 µs | 120,964 | +95% | −49% |
| 64 | 1,073 µs | 59,687 | 506 µs | 126,518 | +112% | −53% |
| 128 | 2,183 µs | 58,661 | 965 µs | 132,688 | +126% | −56% |
| 256 | 5,053 µs | 50,706 | 1,827 µs | **140,099** | **+176%** | **−64%** |

**The origin peaks at 32 clients and then loses ground** — 62,078 tps down to
50,706 at 256, an 18% decline, with latency rising 10×. PgCache never turns over:
it climbs to 140,099 tps and its latency rises 16× against the origin's 26×.

This is the measurement no real subject in this platform could produce. openFGA
ran at 40–160 rps against an origin at a 99.93% buffer hit rate and NetBox never
left its comfortable region, so every prior campaign measured PgCache's overhead
and none of them measured its offload.

It also means the *smallest* gain is at the lowest load. A single-client
comparison — the one most people run first — is PgCache's **worst** case here,
and it still wins by 70%.

---

## Defect found and fixed

**Three cells recorded `ratio=0.0000`** — w2 at 64, 128 and 256 clients. Scraped
by hand afterwards, the same cache was at **99.25%** (12,089,948 hits / 91,810
misses). A single 10-second `curl` had timed out; the cache was fine, the
measurement was not.

The latency and throughput for those cells are unaffected — they come from
pgbench, not from the scrape — and the hit ratio is bounded by the identical
`hot=1000` cells at lower concurrency, all of which measured 98.5–98.8%. They are
reported above from that evidence, not from the failed scrape.

Fixed for s2: three attempts, 30-second timeout, and **abort on total failure**.
A latency without its hit ratio is uninterpretable, which is precisely what made
openFGA's r5 retractable, so a missing scrape must stop the cell rather than
record a zero that reads like a cache failure.

---

## Health

23 `FailedScheduling`/`Preempted` events in the namespace, **all 46–50 minutes
old** — the initial scale-up waiting for nodes, plus Grafana and Prometheus being
preempted on the *system* pool. None fell inside a measurement window and none
touched origin, pgcache or loadgen. The lab pods were stable for the whole
campaign.

---

## What this does not settle

- **P0 only.** 150 MB against 1 GB of `shared_buffers`: the origin never touches
  disk. P1 (~1.5 GB) and P2 (~6 GB) raise O and would lower break-even further.
  They are scale, and must be reported as scale — not as the rung where the
  cache finally looks good.
- **One query shape.** A primary-key point select. w5 in campaign s2 tests the
  result-size axis, where probe 0 found PgCache losing by 65×.
- **No writes.** Static data throughout, apart from the w7 gate's update burst.
  CDC invalidation cost under sustained load is still unmeasured — that is
  sysbench's job, and it remains this platform's largest blind spot.
- **`-M simple` only.** w4 in s2 covers `extended` and `prepared`; the latter
  makes the *origin* faster, which raises break-even.
- **Still not an adoption verdict.** No path C exists here. What this measures is
  the envelope: PgCache pays when `H < O` and the hit ratio clears break-even,
  and the payment grows as the origin saturates.

---

## Reproduce

```bash
helm upgrade --install synth ./synthetic/infra/aks/chart -n pgcache-synth \
  --set campaign.enabled=true --set campaign.id=s1 \
  --set campaign.phases="w7 w0 w1 w2" \
  --set campaign.reps=2 --set campaign.duration=45s --set campaign.warmup=45s
kubectl -n pgcache-synth logs -f job/campaign-s1
kubectl -n pgcache-synth exec deploy/loadgen -- cat /lab/results/s1/cells.tsv
```

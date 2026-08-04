# Test Architecture — PgCache vs. OpenFGA

**Status:** design document. It describes what is measured and why. It contains no results.
Source material: `PLAN.md` (experimental design), `README.md`, `docs/RUNBOOK.md` (operations),
`tools/internal/universe/universe.go` (the oracle), `tools/cmd/fgabench/main.go` (load
generator), `model/model.json` (authorization model).

---

## 1. The question under test

> Does a cache at the **database level**, kept coherent by **change data capture**, beat a
> cache at the **application level**, kept coherent by **TTL** — in latency *and* in
> correctness at the same time?

The two competitors are:

| | PgCache (path B) | OpenFGA in-process cache (path C) |
|---|---|---|
| Placement | Wire proxy in front of PostgreSQL | Inside the OpenFGA process |
| Coherence mechanism | Change data capture on the origin's logical WAL | Time-to-live expiry |
| Staleness window | CDC lag (sub-second, observable as `pgcache_cdc_lag_seconds`) | Up to the TTL, 10 s by default |
| Coverage | Any read that reaches the datastore, including `ListObjects` and `ListUsers` | `Check`, and only partially `ListObjects` |
| Default state | n/a (external component) | Disabled — `--check-query-cache-enabled`, `--check-iterator-cache-enabled`, `--cache-controller-enabled` are all off in the shipped binary |

The correctness half of the question is not decoration. For an authorization system a stale
cached decision is a security incident, not a performance metric. The design therefore treats
agreement between paths as a **gate** that runs before any timing measurement (`PLAN.md` §11,
`docs/RUNBOOK.md` §5.3): if paths A and B disagree on a single decision, the run is aborted and
becomes a bug report rather than a benchmark.

The design also states its null hypothesis explicitly (`PLAN.md` H0): if the tuple working set
fits in the origin's `shared_buffers`, the baseline already serves everything from RAM and the
cache adds a network hop with no benefit, or with a loss. The scale ladder in §6 exists
specifically to locate where that stops being true.

---

## 2. Why OpenFGA is the test subject

### 2.1 Every read path is a cacheable shape

The lab audited OpenFGA's PostgreSQL adapter (`pkg/storage/postgres/postgres.go`, `main`,
release `v1.18.x`). The finding, quoted from `PLAN.md` §1:

> Every OpenFGA read path is a single-table `SELECT` with equality predicates. There is not a
> single CTE, `JOIN`, `LATERAL`, view, window function or `RECURSIVE` in the PostgreSQL
> adapter. `FOR UPDATE` appears only in the write transaction.

This matters because PgCache's non-cacheable list is exactly: views, RLS, tables without a
primary key, `LATERAL`, `RECURSIVE` CTEs, `FULL`/`CROSS JOIN`, locking clauses, and volatile
functions outside the `SELECT` list. OpenFGA emits none of them on the read path.

| Storage method | SQL emitted | Cacheable? |
|---|---|---|
| `ReadUserTuple` | `SELECT … FROM tuple WHERE _user=$1 AND object_id=$2 AND object_type=$3 AND relation=$4 AND store=$5 AND user_type=$6` | Yes — point lookup, covers the PK |
| `ReadUsersetTuples` | `SELECT … FROM tuple WHERE store=$1 AND user_type='userset' AND object_type=$2 AND relation=$3 AND (_user LIKE 'group:%#member' OR …)` | Yes |
| `ReadStartingWithUser` | `SELECT … FROM tuple WHERE _user IN (…) AND object_type=$2 AND relation=$3 AND store=$4 ORDER BY object_id collate "C"` | Yes |
| `Read` (TTU iterator) | `SELECT … FROM tuple WHERE store=$1 AND object_type=$2 AND object_id=$3 AND relation=$4` | Yes |
| `ReadPage` | same, plus `ORDER BY ulid LIMIT n+1` | Yes |
| `ReadAuthorizationModel` | `SELECT … FROM authorization_model WHERE authorization_model_id=$1 AND store=$2` | Yes (irrelevant — already behind an in-process LRU) |
| `ReadChanges` | `… FROM changelog WHERE store=$1 AND inserted_at < NOW() - interval '0ms' …` | **No — volatile.** `NOW()` is interpolated as a literal, so identical SQL text returns different rows as the wall clock advances |
| Write path | `… IN ((…)) FOR UPDATE`, `INSERT`, `DELETE` | No, by design |

**Methodological consequence.** PgCache is configured with
`ALLOWED_TABLES=tuple,authorization_model`. This excludes `changelog`, the only read whose
answer depends on the clock. With that exclusion, every query admitted to the cache is
deterministic, and the correctness argument is auditable rather than probabilistic.

### 2.2 Resolution happens in Go, not in SQL

OpenFGA resolves the authorization graph entirely in Go (`internal/graph/check.go`). A `Check`
is a breadth-first traversal of the model's rewrite tree, and **each returned tuple becomes a
child dispatch**. There is no batching: no `IN`-coalescing between sibling sub-problems.

| Situation | Datastore queries |
|---|---|
| Trivial direct `Check` | 1 |
| Relation typed `[user, user:*, group#member]` | 3 concurrent queries at the node |
| Userset with fan-out F | 1 + F sub-resolutions |
| TTU (`viewer from parent`) with P parents | 1 + P |
| Depth 3 × fan-out 10 | roughly 1 000 queries for a negative answer |

There is a decisive asymmetry: `union` short-circuits on the first `Allowed: true` and cancels
its siblings (`check.go:207`). **A positive answer terminates after a few queries; a negative
answer must exhaust the entire fan-out.** This is [openfga#727](https://github.com/openfga/openfga/issues/727),
closed as stale and never fixed in the engine; the official guidance was to remodel the data.

Measured on this lab's mass, `queries_per_request` — derived from OpenFGA's own
`datastore_query_count` histogram, as a delta inside the measurement window, not an estimate —
lands at **124 to 480 datastore queries per request** (`docs/RUNBOOK.md` §7.1).

This is the load profile in which a coherent read cache should produce its largest measurable
gain: many small, repetitive, cacheable queries per request, with the worst case being the case
of highest repetition. It is also a strong lever in the other direction: an overhead of 0.3 ms
per query becomes 60 ms per request at 200 queries. The runbook therefore instructs reading the
amplification number *before* the latency number — if amplification changed between two paths,
they are not doing the same amount of work and their latencies are not comparable.

### 2.3 There is an honest competitor

OpenFGA ships its own cache. It is off by default, so the factory binary is cache-free on the
tuple path, but ignoring it would make the benchmark dishonest. Measuring it is what turns the
question from "does a cache help?" (obviously yes) into "*which* cache, and at what correctness
cost?".

---

## 3. The three paths

```
                 fgabench (open-loop generator, outside the data path)
                          |  labels every metric with path in {A,B,C}
     +--------------------+--------------------+
     v                    v                    v
 openfga-a:18080     openfga-b:18090      openfga-c:18100
 caches OFF          caches OFF           caches ON (in-process, TTL 10 s)
     |                    |                    |
     |               pgcache:16432             |
     |            (PostgreSQL 18 embedded      |
     |             in the PgCache image)       |
     |                    |                    |
     +------------> origin:15432 <-------------+
                 PostgreSQL 17, wal_level=logical
                 shared_buffers pinned at 1 GB
```

| Path | Name | OpenFGA connects to | OpenFGA caches | What it isolates |
|---|---|---|---|---|
| **A** | `baseline` | origin directly | all off | The real cost of the datastore |
| **B** | `pgcache` | pgcache → origin | all off | The gain attributable **only** to PgCache |
| **C** | `appcache` | origin directly | check + iterator + controller on | The honest competitor |

**Golden rule.** All three point at the **same origin**, with the **same mass**, in the **same
window**, driven by the **same generator**, at the **same target rate**. Numbers from different
runs are never compared; `report.py` enforces this by grouping on
`(rung, workload, target rate, doc_dist, path)` and refusing to aggregate across it.

**Why path C exists.** Without it the question degenerates to "does caching help?", whose answer
is trivially yes and worth nothing. With it, the comparison is between two caches that differ in
the property that actually matters for authorization: PgCache invalidates on CDC and bounds
staleness by replication lag; OpenFGA's cache expires on a 10 s TTL and can therefore serve a
revoked authorization decision for up to 10 seconds. If the hypothesis holds, PgCache wins on
latency and on staleness simultaneously. If it does not, path C is the evidence that the loss is
real and not an artifact of comparing against an uncached baseline.

### Resource limits

Container limits are part of the result, not incidental configuration (`docker-compose.yml`,
`docs/RUNBOOK.md` §2):

| Service | cpus | memory |
|---|---|---|
| `origin` | 3.0 | 4 GiB |
| `pgcache` | `${PGCACHE_CPUS:-4.0}` | `${PGCACHE_MEM:-5g}` |
| `openfga-{a,b,c}` | 2.0 | 2 GiB each |

`cpu_perc` from `docker stats` is relative to a single core: 140 % in a container with
`cpus: 4.0` is 35 % utilization, while 202 % with `cpus: 2.0` is full saturation. A container
sitting at its own ceiling measures the ceiling, not the software.

### Configuration decisions that make the paths comparable

| Setting | Default | Used here | Reason |
|---|---|---|---|
| `--experimentals` | `["pipeline_list_objects"]` — **not** empty | explicitly `""` | Otherwise the paths do not run the same code |
| `--request-timeout` | 3 s | 60 s | Under saturation the default measures timeouts, not latency |
| `--datastore-max-open-conns` | 30 | pinned, identical on all three paths | This is the real concurrency ceiling against the database, regardless of any other flag |
| `PGCACHE_TELEMETRY` | on | `off` | Anonymous telemetry is uncontrolled network noise |
| `shared_buffers` (origin) | — | 1 GB on every rung | Without this the ladder measures how much RAM PostgreSQL was given, not scalability |

A known confound is recorded rather than hidden (`docs/RUNBOOK.md` §9): making the queries
visible to PgCache requires `PGX_EXEC_MODE=simple_protocol`, so path B is not "path A plus a
cache" but "path A **without prepared statements**, plus a cache". Running path A in
`simple_protocol` isolates that component; the knob exists in `.env`.

---

## 4. The authorization model

A Google-Drive-like domain: four types, tuple-to-userset, and unions of usersets — the shape
that produces real query amplification. The DSL, from `PLAN.md` §5, corresponds one-to-one with
`model/model.json`:

```
model
  schema 1.1

type user

type group
  relations
    define member: [user, group#member]

type folder
  relations
    define parent: [folder]
    define owner:  [user, group#member]
    define editor: [user, group#member] or owner or editor from parent
    define viewer: [user, user:*, group#member] or editor or viewer from parent

type document
  relations
    define parent: [folder]
    define owner:  [user, group#member]
    define editor: [user, group#member] or owner or editor from parent
    define viewer: [user, user:*, group#member] or editor or viewer from parent
```

Two independent scaling knobs, and their independence is the point:

- **Folder tree depth** → round-trips per `Check` (the TTU chain `viewer from parent`).
- **Group width (memberships per user)** → fan-out at a single node.

The recursive `group#member` in `define member: [user, group#member]` is deliberate. It engages
OpenFGA's recursive resolver and `ReadStartingWithUser`, whose `ORDER BY object_id collate "C"`
is not covered by the `idx_user_lookup` index in all cases — a guaranteed sort node at the
origin.

`document#viewer` accepts `user:*`, so a fraction of documents are public. This is what makes
the positive/negative asymmetry of §2.2 reachable from the workload generator.

---

## 5. The analytical oracle

`tools/internal/universe` defines the data mass **purely structurally**. The package's stated
invariant:

> Given the parameters, the authorization of any (user, document) pair is derivable in
> O(depth) without querying anything.

Topology is pure arithmetic on indices:

| Function | Rule |
|---|---|
| `GroupParent(j)` | `(j-1)/2` — groups form a binary tree; `g{j}` is a subset of its parent |
| `FolderParent(k)` | `(k-1)/FolderBranching` — folders form a B-ary tree |
| `UserGroup(i)` | `i % Groups` — the user's entry group |
| `FolderGrantGroup(k)` | `(k + Groups/2) % Groups` — the group granted `viewer` on folder `k` |
| `DocFolder(m)` | `m % Folders` |
| `DocOwner(m)` | `m % Users` |
| `IsPublicDoc(m)` | `PublicEvery > 0 && m % PublicEvery == 0` |

The ground truth, `Params.CanView(i, m)`, is true if and only if any of:

1. `d{m}` is public (`viewer: user:*`);
2. `u{i}` owns `d{m}` (owner ⇒ editor ⇒ viewer);
3. some folder in `FolderAncestors(DocFolder(m))` grants a group that appears in
   `GroupAncestors(UserGroup(i))` — the transitive closure over the folder chain and the group
   tree.

The `Groups/2` offset in `FolderGrantGroup` is not cosmetic and is documented as such in the
source: without it `folder:f0`, the root and an ancestor of every folder, would grant `group:g0`,
the root of the group tree, which transitively contains every user. Every pair would be positive
and the mass would be useless for negative-check workloads.

### The three things the oracle enables

1. **Seeding without state.** 10 M+ tuples can be written with `COPY`, because every tuple is a
   pure function of an index. The OpenFGA write API caps at 100 tuples per call — E3 would be
   roughly 97 000 HTTP requests.
2. **Exact workload mixes.** The generator can construct requests with a known outcome:
   `PositiveDocFor(i, r)` returns a document `u{i}` can see; `NegativeDocFor(i, r)` returns one it
   cannot. That is what makes W1 ("≈85 % positive") and W3 ("100 % negative") exact rather than
   approximate — and negative is the case that forces full fan-out, per §2.2.
3. **Verifying correctness, not just speed.** Every decision returned by OpenFGA, with and
   without PgCache, is checkable against `CanView` in O(depth) with no database access.

`make test` runs the oracle against a brute-force resolution over the tuple set actually
emitted. That test has already caught two real modeling errors: the root folder granting the
root group (which made 100 % of pairs positive), and the existence of **omniscient users** —
users whose entry group lies in the chain granted by the root folder, for whom no negative check
exists at all. The latter is a legitimate property of the topology, equivalent to an org-wide
admin group, but W3 must exclude them; `IsOmniscient(i)` and `SampleDeniableUser(r)` exist for
that.

The mass also contains deliberate noise — `NoiseGroupsPerDoc`, `NoiseUsersPerDoc` — which
inflates the table and the fan-out without changing the fundamental truth: noise groups have no
members and noise users are never test principals.

---

## 6. The scale ladder

Five rungs. Each rung is a complete mass, seeded from scratch, with its own snapshot and its own
`manifest.json`. Counts come from `universe.Rungs`; tuple totals are exact, computed by
`Params.TupleCount()`.

| Rung | Users | Groups | Folders | Docs | Folder branching | Noise groups / users | Tuples (exact) | Approx. size |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| **E0** smoke | 200 | 50 | 100 | 500 | 3 | 200 / 400 | 3 453 | ~3 MB |
| **E1** | 2 000 | 500 | 1 000 | 10 000 | 4 | 2 000 / 4 000 | 84 598 | ~30 MB |
| **E2** | 10 000 | 2 500 | 5 000 | 100 000 | 5 | 10 000 / 20 000 | 1 023 498 | ~300 MB |
| **E3** | 50 000 | 12 500 | 20 000 | 800 000 | 6 | 50 000 / 100 000 | 9 710 498 | ~3 GB |
| **E4** optional | 100 000 | 25 000 | 50 000 | 2 500 000 | 6 | 100 000 / 200 000 | 30 249 998 | ~9 GB |

`PublicEvery = 100` on every rung: one document in a hundred carries `viewer: user:*`.

`TupleCount()` is the sum of: one tuple per user (user → group), `Groups-1` group nesting
tuples, `Folders-1` folder-parent tuples, `Folders` folder-viewer grants, `Docs` document-parent
tuples, `Docs` document-owner tuples, `Docs × NoiseGroupsPerDoc`, `Docs × NoiseUsersPerDoc`, and
`Docs / PublicEvery` public-viewer tuples.

The ladder exists to find **the knee of the curve**: the point at which the mass stops fitting
in the origin's `shared_buffers` and a cache starts to be worth its cost. A single measurement
point proves nothing; the curve proves something. This is why `shared_buffers` is pinned at 1 GB
on every rung, and why the design commits in advance to publishing the rungs where the gain is
zero or negative.

Operational constraint: `scripts/seed.sh` runs `DELETE FROM tuple … DELETE FROM store` before
loading, so the database holds **one rung at a time**. Seeding E1 destroys E3. The `results/`
tree and each rung's `manifest.json` survive.

Post-seed validation is mandatory and compares a sample of the seeded mass against the oracle
through OpenFGA's own API — 300 positives and 300 negatives, zero divergences expected. If it
diverges, the mass is wrong and no timing measurement matters.

---

## 7. The workloads

All use a Zipf(α) distribution over users and objects except where noted, with α = 1.1 (the
classic Zipf exponent). Zipf because real authorization traffic is heavily skewed: a small set of
users and documents absorbs most of the checks. W2 is the uniform control — a α tuned too high
would fit in any cache, and α = ∞ would be a microbenchmark.

| ID | Name | Mix | Distribution | What it answers |
|---|---|---|---|---|
| **W1** | `check-hot` | 100 % Check, ≈85 % positive | Zipf α=1.1 | Cache-friendly steady state. The base case for the latency hypothesis. |
| **W2** | `check-cold` | 100 % Check | uniform | Worst case for the cache: no locality. Tests whether the proxy actively *hurts*, and prices the extra network hop. |
| **W3** | `check-deny` | 100 % Check, 100 % negative | Zipf α=1.1 | Maximum fan-out, no short-circuit. Largest predicted gain. |
| **W4** | `list-objects` | 100 % `ListObjects document#viewer` | Zipf | Extreme read amplification. |
| **W5** | `mixed-write` | 95 % Check / 5 % Write | Zipf | CDC lag, invalidation, staleness canary. |
| **W6** | `ceiling` | as W1, open ramp | Zipf | Sustained throughput under an SLO of p99 ≤ 200 ms. |
| **W7** | `differential` | identical Check pairs against A and B | uniform | **Correctness.** Measures agreement, not time. |

W7 is structurally different from the rest: `runDifferential` issues the same `(user, document)`
check to both targets, compares the two answers to each other *and* both to `CanView`, and exits
non-zero on any divergence — including the case where A and B agree with each other but disagree
with the oracle, which indicates a bad mass or model rather than a bad cache. It samples half its
pairs through `PositiveDocFor` so the positive branch is exercised; by construction the negative
branch is the majority.

### The document locality axis

```
-doc-dist zipf      # default: object locality, the intended behavior
-doc-dist uniform   # legacy: no locality
```

This is not a detail. `PositiveDocFor` / `NegativeDocFor` map their seed parameter through
modular arithmetic. When seeded with the request sequence number, a monotone sequence becomes a
permutation, and a 1 600-request run produced **99.4 % distinct documents**. With no repeated
`object_id` there is no repeated `WHERE`, and the benchmark stops exercising the only thing a
read cache does. `tools/cmd/fgabench/generator_test.go` locks this regression;
`make test` is not a formality.

Because both functions are deterministic in `(user, seed)`, seeding them with the Zipf draw makes
a hot seed return the *same* document — which is what turns user-axis locality into object-axis
locality.

---

## 8. The open-loop generator

`fgabench` schedules arrivals at a fixed rate and **does not wait for the previous response**.
Latency is measured from the **intended arrival instant**, not from the moment a worker became
free.

A closed-loop generator — N threads that each wait for a response before issuing the next
request — automatically reduces offered load when the server slows down. The result is
**coordinated omission**: a saturated system looks fast because the client stopped asking. The
requests that would have been slow are the ones that were never sent, so they never appear in the
distribution.

The open-loop design is implemented as a scheduler goroutine feeding a buffered channel
(`runPhase` in `main.go`). The buffer is sized at `4 × rate`, clamped to `[1024, 2^20]`. When the
queue is full the excess is counted as **dropped** rather than blocking the scheduler, because
blocking would reintroduce the omission. Jobs still queued after the deadline are also dropped
rather than drained, since draining would extend the run past `-duration` and measure a window
that does not exist.

Dropping has a consequence that must be read correctly:

```
drop_ratio = dropped / (requests + dropped)
```

**Drops are not random** — what disappears is exactly what would have taken longest. A p99
computed over the survivors has no referent. `report.py` flags anything above 10 % and refuses
the comparison outright, reporting that at least one path collapsed and its percentiles do not
describe the offered load.

### Warm-up

`PLAN.md` §8 requires excluding warm-up from the analysis window and, on path B, waiting for the
knee of the hit-rate curve before opening it — measuring a cold cache and calling it "PgCache" is
not a measurement. With `-warmup-max`, the generator warms in slices, measures the hit ratio *of
the slice* from PgCache's own metrics, and stops when consecutive slices differ by less than
`-warmup-knee-eps` (default 0.01). If the ceiling is reached without stabilizing, it prints a
warning, and the run must either be repeated with a higher ceiling or published with the
limitation recorded.

`-post-warmup-exec` runs a shell command between warm-up and measurement — in practice, resetting
`pg_stat_statements`. Resetting it before warm-up would make the server's window cover warm-up
plus measurement while the client's window covers only measurement, and the two would stop being
comparable.

### What each run emits

Per run, under `results/<rung>/<workload>/<path>-<run_id>/`: `summary.json`, `latencies.csv`
(one row per request: `latency_ms,ok,allowed`), `docker-stats.csv` sampled *during* the window,
`versions.txt` with image digests, `pg_stat_statements.psv`, and raw `/metrics` dumps from
OpenFGA and (on path B) PgCache taken immediately before and after the window. The cumulative
metric counters reset on process restart, so a negative `after − before` delta identifies a run
contaminated by a mid-window restart.

All runs also append a row to `results/all-runs.csv` with 26 columns. New columns are always
appended at the end, and `fgabench` rewrites a stale header on detection — otherwise `report.py`'s
`DictReader` would silently discard the extra columns, which is how `doc_dist` once arrived empty
and caused `zipf` and `uniform` runs to be aggregated together.

Every metric, log line and trace carries `scenario`, `rung`, `path` and `run_id`, without
exception.

---

## 9. Execution order

```
W7 correctness (E0)     <- gate: on failure, everything stops
    v
E0 smoke (W1)           <- functional gate: do the three paths answer identically?
    v
E1 -> E2 -> E3          <- W1, W2, W3 at each rung, 3 repetitions, 3 paths
    v
E2 (W4, W5)             <- ListObjects and write pressure
    v
E3 (W6)                 <- throughput ceiling
    v
report
```

Protocol rules that make the numbers usable (`PLAN.md` §8): restore the snapshot before **each**
path, never run B on the state left by A; exclude warm-up from the analysis window; run three
repetitions per (rung × workload × path) and report the median with min/max dispersion, never a
single execution; cut analysis windows by timestamp identically across paths; sample
`docker stats` throughout — if the origin saturates CPU on the baseline and does not under
pgcache, that *is* the result, not an artifact. Path B is restarted before each repetition so
every repetition starts from a cold cache and the repetitions are independent.

---

## 10. Known threats to validity

The design records these rather than hiding them.

| Threat | Why it is a risk | Mitigation |
|---|---|---|
| Single machine, competing containers | 8 vCPU split across origin, pgcache, 3× OpenFGA and the generator | Fixed `cpus:` / `mem_limit` per service; one path under load at a time. `scripts/suite.sh` aborts if foreign containers are running |
| Everything fits in RAM | At E0/E1 the origin serves entirely from `shared_buffers` and there is nothing for a cache to win | `shared_buffers` pinned at 1 GB; ladder extended to 10 M / 30 M tuples; rungs with zero or negative gain are published |
| Extra network hop on path B | PgCache adds proxy latency the baseline does not pay | Measured explicitly by W2 (no locality) |
| Seeding via SQL ≠ seeding via API | Tuples are written with `COPY`; the API caps at 100 tuples per call | Post-seed validation samples the mass through the API; `changelog` populated consistently; `ANALYZE` mandatory |
| Protocol confound | PgCache requires `simple_protocol`, so path B lacks prepared statements | Recorded as an open limitation; path A can be run in `simple_protocol` to isolate the component |
| Local origin | Between containers on one host the round-trip a hit avoids is ~0.1 ms — the condition where a cache has least to gain, by construction | Documented; a remote origin (1–3 ms × 124–480 queries per Check) is the test that could invert the sign, and is not executable in this topology |
| OpenFGA cache accidentally enabled | Would contaminate A and B | Explicit `=false` flags **and** `--experimentals=""` |
| `PGCACHE_MEM` above host RAM | A cgroup limit above physical RAM never cuts; the process grows until the machine is exhausted and restarts mid-window, zeroing the counters | Checked against `docker info --format '{{.MemTotal}}'` before running |

---

## 11. Publication checklist

No number is published unless all of these hold (`docs/RUNBOOK.md` §8):

- `make test` passed — oracle and locality regression
- Post-seed mass validated: 600/600 with no divergence
- W7 gate passed at 100.000000 % agreement
- `PGCACHE_MEM` below the host's physical RAM
- No foreign containers during measurement, or the contamination is recorded
- Warm-up reached the knee (no ceiling-without-stabilizing warning)
- `drop_ratio < 10 %` on every path being compared
- PgCache counter deltas positive (no mid-window restart)
- All three paths at the same target rate and the same `doc_dist`
- At least 3 repetitions per combination
- `versions.txt` with image digests — `latest` does not identify a build
- Window `docker-stats.csv` inspected: nothing sitting at its own resource ceiling

If any item fails, the limitation is recorded instead of the number being published.

# Synthetic benchmarks as PgCache subjects — analysis

**Date:** 2026-08-04 · **Tools assessed:** pgbench, sysbench, HammerDB, BenchBase, YCSB

---

## The reframe, first

These five tools cannot answer the question the platform has been asking.

The platform's question was *"does PgCache beat the application's own cache?"* —
a three-path comparison: A (origin), B (PgCache), C (the app's cache). A
synthetic benchmark has no application tier, therefore no path C, therefore no
such comparison. Adopting these tools without saying so would quietly change
the deliverable while keeping the old headline.

What they can answer is the question the real subjects kept failing to reach.
openFGA's r5/r6 campaigns ran at 40–160 rps against an origin sitting at a
99.93% buffer hit rate. The origin was bored. **A cache cannot help a bored
origin**, and no real subject in this lab ever generated enough load to stop it
being bored. pgbench put the same origin into throughput collapse in twenty
seconds.

So the honest framing is:

> Real subjects answer *"is PgCache worth adopting for this application?"*
> Synthetic subjects answer *"what is PgCache's performance envelope, and what
> conditions must hold for it to pay?"*

Both are worth having. They are not interchangeable, and a synthetic result must
never be reported as if it were an adoption verdict.

---

## What the probe found, because it changes the ranking

Full numbers in [RESULTS-probe0.md](RESULTS-probe0.md). Three findings drive
everything below.

**1. PgCache won.** First time in this platform. pgbench point-select over a
1,000-key hot set: origin 77 µs / 12,995 tps, PgCache **62 µs / 16,227 tps**.
At 64 clients the origin collapsed to 110,610 tps while PgCache held 162,097 —
a **47% throughput advantage** and 32% lower latency.

**2. The deciding variable is hit ratio, and it has a break-even.** Sweeping
Zipfian skew moved the hit ratio continuously and flipped the sign:

| skew | hit ratio | origin | PgCache | |
|---|---:|---:|---:|---|
| s=1.05 | 71.2% | 65,204 tps | 41,824 tps | PgCache −36% |
| s=1.20 | 82.3% | 68,672 tps | 57,798 tps | PgCache −16% |
| s=1.50 | 97.7% | 66,390 tps | 74,110 tps | PgCache **+12%** |

**3. Query cost is not one variable, it is two.** C9 says expensive origin
queries favour a cache. The range-scan script falsified that as written: at 95.6%
hit ratio a 100-row aggregate still lost (104 µs vs 159 µs), and a 10,000-row
aggregate lost by **65×**. Cost that comes from *result size* is not cost the
cache can bank — it is cost the cache must also pay, on every hit, plus
materialisation.

---

## The tools

### 1. pgbench — **the strongest candidate by a wide margin**

Ships with PostgreSQL. Nothing to build, nothing to package.

**Why it is uniquely clean: the protocol is right by default.**

`-M` defaults to `simple`. Under simple mode pgbench interpolates variables into
the SQL text as literals before sending — which is exactly the shape PgCache
caches. Verified: `--show-script=select-only` returns

```
\set aid random(1, 100000 * :scale)
SELECT abalance FROM pgbench_accounts WHERE aid = :aid;
```

This matters more than it looks. On openFGA, simple protocol had to be *forced*,
so path B was "path A without prepared statements, plus a cache" — the confound
that made nine cells of a result retractable. On pgbench there is no confound to
make: both paths run the tool's own default. `-M extended` and `-M prepared`
exist as first-class flags, so protocol becomes a **measurable axis** rather than
a hazard.

**C2 (transactions):** `select-only` is a bare `SELECT`, no `BEGIN`. Passes.
The default `tpcb-like` script *does* wrap in `BEGIN; … END;` and its reads are
therefore uncacheable — a fact to exploit, not avoid: it gives a
transactional-versus-autocommit contrast within one tool.

**C7 (amplification):** 1 statement per transaction. Nominally the worst score
in the platform's history. Irrelevant here — with no application tier there is
no "request" to amplify into, and `-f` lets any statement mix be written
directly.

**C9/C9b:** C9b is trivially passed — the datastore is ~100% of the transaction.
C9 is the live question and the probe answered it: at 77 µs the origin sits above
PgCache's floor, and PgCache served hits at ~57 µs.

**The decisive advantage — the hit ratio becomes a dial.** `random`,
`random_zipfian` and `random_exponential` are builtins. On openFGA and NetBox the
hit ratio was whatever the application happened to produce (90% and 40%) and
could not be moved. Here it sweeps continuously from 0.14% to 97.7%, which is how
the break-even above was found at all.

**Limitations, stated plainly.** Single-table PK lookups are not an application.
Nothing here validates correctness under CDC beyond what the existing W7-style
gate does. And a headline drawn from pgbench describes PgCache's envelope, not
any real workload's outcome.

**Verdict: adopt.** Cheapest instrument, cleanest protocol story, only tool that
makes hit ratio an independent variable.

---

### 2. sysbench — **adopt, second, and only for the write/CDC axis**

`--with-pgsql` at build time; the Docker path is a Percona sysbench image or a
short build.

**The reason to take it:** it is the only tool here that cleanly varies the
read/write mix, and **the platform has never once measured PgCache under
writes.** Every campaign to date ran read-heavy against a static dataset. CDC
invalidation cost, replication lag under load, and the behaviour of pinned tables
while rows change underneath are all unmeasured. `oltp_read_write` with
`--point_selects` / `--index_updates` / `--non_index_updates` /
`--delete_inserts` turns that into a swept axis.

**Two non-default flags are mandatory, and one of them is a trap.**

*`--skip_trx=on`* — verified in `oltp_common.lua`: `skip_trx`, default `false`,
"Don't start explicit transactions and execute all queries in AUTOCOMMIT mode".
Left at the default, every read is inside `BEGIN…COMMIT` and **C2 fails
outright** — PgCache passes through the entire workload and the campaign measures
nothing.

*`--db-ps-mode=disable`* — verified in `drv_pgsql.c`. The driver defaults to real
server-side prepared statements (`PQprepare` / `PQexecPrepared`); with `use_ps`
off it substitutes parameters client-side and sends via `PQexec`.

> **The trap.** Disabling prepared statements makes the *origin* slower, because
> it reparses and replans every statement. That raises the origin's per-query
> cost, which makes C9 easier to pass. Applying it to both paths keeps the
> comparison internally fair, but the whole experiment then runs against a
> deliberately de-optimised origin — which is the move C9 closes with ("do not
> fix this by strangling the origin") and the reason campaign r8 was killed.
>
> **Required mitigation:** run sysbench at `--db-ps-mode=auto` as the reference
> and report `disable` only as a labelled second arm, with the origin's per-query
> cost stated for both. If PgCache only wins with prepared statements off, that
> is the finding, and it is a negative one.

pgbench has no equivalent trap, which is why it ranks first.

**Verdict: adopt for the write/CDC axis only, with the protocol arm reported in
pairs.**

---

### 3. HammerDB — **reject**

**TPROC-C is structurally uncacheable.** The PostgreSQL driver implements the
five TPC-C transactions as **PostgreSQL functions by default**
(`pg_storedprocs false`; setting it `true` switches to true stored procedures).
Either way the transaction body executes server-side and the client sends one
opaque call. PgCache never sees the underlying statements, and a function call
is volatile — it must be passed through. There is nothing to cache. This is not
a tuning problem; it is the architecture of the workload.

**TPROC-H is client-side SQL and technically cacheable**, but it is the wrong
shape for a cache: 22 analytical queries, each running for seconds, each
returning a large aggregate result. Finding 3 above already showed what PgCache
does with large results — the 10,000-row aggregate lost by 65×. TPC-H is that
case, larger.

Secondary: the Tcl driver and GUI make it the hardest of the five to run
headless in a Job, for the least return.

**Verdict: reject.** Record it, so it is not re-proposed.

---

### 4. BenchBase — **defer; strongest *if* one thing checks out**

CMU's framework. The genuine attraction is that it is the only tool here with
**realistic multi-table read workloads**: Wikipedia (read-heavy, skewed),
Twitter (profile and feed fan-out), TPC-C, TATP, SEATS, plus YCSB. Those have
real amplification and real joins — the things pgbench cannot produce.

**The blocker is JDBC.** pgjdbc defaults to `preferQueryMode=extended` with
`prepareThreshold=5`, so after five executions every statement becomes a
server-side prepared statement. `preferQueryMode=simple` exists and is documented
as "execute, no parse, no bind, text mode only" — literals in the text, simple
protocol. So it is settable in the JDBC URL.

But this is sysbench's trap again, in a heavier package: switching pgjdbc to
simple mode de-optimises the origin, and every BenchBase workload would then be
running in a mode no BenchBase user runs. C3 records that PgCache parses extended
protocol correctly (Strapi spike, 25/25 statements), which means the *right*
experiment is `preferQueryMode=extended` — the default — and that has never been
tested at load.

**Also:** most BenchBase workloads are transactional by construction, so C2 bites
the same way it does in sysbench, and BenchBase exposes no `skip_trx` equivalent.
Wikipedia and the read-only YCSB profiles are the plausible survivors.

**Verdict: defer.** Revisit only after pgbench has established the envelope, and
enter it with `preferQueryMode=extended` as the reference arm.

---

### 5. YCSB — **reject as a separate subject; fold Workload C into pgbench**

`go-ycsb`'s PostgreSQL driver uses `lib/pq` with `$1` placeholders and a
statement cache (`getAndCacheStmt` → `stmt.QueryContext`) — extended protocol
with server-side prepare, same position as BenchBase and with no documented
simple-protocol escape.

The deeper problem is that YCSB adds nothing pgbench does not already give.
Its read path is `SELECT * FROM usertable WHERE YCSB_KEY = $1` — the identical
single-table PK lookup, against the identical uniform-or-Zipfian keyspace.
Workloads A/B/C/D are read/write ratios and access distributions, and pgbench
expresses all four with `-f` scripts, `@weight` and `random_zipfian`, in the
right protocol, with no extra image to maintain.

**Verdict: reject.** Reproduce the A/B/C/D *profiles* as pgbench scripts and cite
YCSB as their origin.

---

## Ranking

| # | tool | verdict | what it uniquely buys |
|---|---|---|---|
| 1 | **pgbench** | adopt | simple protocol by default; hit ratio as a dial; zero packaging |
| 2 | **sysbench** | adopt, scoped | the write/CDC axis, never yet measured |
| 3 | BenchBase | defer | realistic multi-table reads — behind a JDBC protocol question |
| 4 | YCSB | reject | duplicates pgbench in a worse protocol |
| 5 | HammerDB | reject | server-side functions; nothing reaches the cache |

---

## What must not be claimed from any of this

- **Not an adoption verdict.** No path C exists, so "PgCache beats the app's
  cache" is unprovable here in either direction.
- **Not transferable latency numbers.** The probe ran on Docker Desktop on macOS,
  where the origin measured 77 µs against the ~40 µs recorded on AKS nodes. The
  *shape* — break-even, saturation, sign changes — is what transfers. The
  absolute margins are not, and re-measuring them on AKS is the point of the
  campaign.
- **Not a scenario built to win.** The hot-set size is an independent variable
  swept across its whole range and reported at every point, including the values
  where PgCache loses by 36% and by 65×. The moment a rung is chosen *because*
  PgCache wins there, this becomes r8 again.

---

## Sources

- [pgbench — PostgreSQL 17 documentation](https://www.postgresql.org/docs/17/pgbench.html)
- [sysbench](https://github.com/akopytov/sysbench) · [`oltp_common.lua`](https://raw.githubusercontent.com/akopytov/sysbench/master/src/lua/oltp_common.lua) · [`drv_pgsql.c`](https://raw.githubusercontent.com/akopytov/sysbench/master/src/drivers/pgsql/drv_pgsql.c)
- [HammerDB — stored procedure driver options](https://www.hammerdb.com/docs4.9/ch04s08.html) · [pgoltp.tcl](https://github.com/TPC-Council/HammerDB/blob/master/src/postgresql/pgoltp.tcl)
- [pgjdbc connection parameters](https://jdbc.postgresql.org/documentation/use/)
- [go-ycsb PostgreSQL driver](https://raw.githubusercontent.com/pingcap/go-ycsb/master/db/pg/db.go)

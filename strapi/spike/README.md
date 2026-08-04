# Strapi × PgCache — R1 / R1b spike

A ~3-minute runnable check that resolves the two **blocking** protocol questions
in `strapi/STUDY.md` ("Open questions and risks" #1 and #2) *before* anything in
Phase 2 gets built. Both are stated in the study as things that "must happen
before anything else is built"; this is that thing.

| | Question | If it fails |
|---|---|---|
| **R1** | Can PgCache parse the **extended query protocol**? | Strapi is **DISQUALIFIED** |
| **R1b** | Can PgCache **serve reads from cache inside an explicit `BEGIN…COMMIT`**? | The cacheable ceiling collapses to 5 statements — **human decision required** |

---

## Run it

```
cd /Users/leonardo.benedet/BenedetLabs/pgcache/strapi/spike
./run.sh
```

Options: `./run.sh --keep` leaves the stack up for manual poking;
`./run.sh --down` tears it down.

Requires Docker, and network access on first run (the client container does
`npm install pg@8.20.0`). Machine-readable output lands in
`strapi/spike/out/result.json`.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | R1 PASS. R1b may be PASS **or** FAIL — read the banner. R1b is a scoping decision for a human, not a build gate, so it does not fail the script. |
| `1` | **R1 FAIL — subject disqualified.** |
| `2` | **INCONCLUSIVE.** Evidence insufficient. Nothing may be built on this. |

---

## Why node-postgres and not `psql`

`STUDY §5` ("Driver protocol"): Strapi is knex `3.0.1` + node-postgres `8.20.0`.
Knex parameterizes **every** query, and node-postgres sends parameterized
statements over the **extended protocol** as unnamed Parse/Bind/Describe/
Execute/Sync. There is no simple-protocol switch in `pg` and knex never names
statements — unlike openFGA's pgx, there is no `default_query_exec_mode` escape
hatch to fall back on.

`psql` sends the **simple** protocol. Testing with `psql` would answer a
question nobody asked and would produce a false PASS. So the client is
node-postgres, pinned to the exact version in Strapi's `yarn.lock`.

In `client/check.js`:

- `client.query({ text, values })` → **extended** protocol (what Strapi emits)
- `client.query('BEGIN')` → **simple** protocol (what knex emits for
  transaction control, `database/src/index.ts:177-183`)

---

## How to read the output

### R1

```
PASS  R1         parameterized SELECT ($1) through PgCache returned the correct row …
```

**R1 PASS** means a `$1`-parameterized `SELECT` went through PgCache over the
extended protocol and came back with the exact expected row. The harness also
prints a protocol-counter control — one *simple*-protocol `SELECT 1` — so you
can see that `pgcache_protocol_simple_queries` and
`pgcache_protocol_extended_queries` move independently and the R1 evidence is
not an artefact of a counter that increments on everything.

**R1 FAIL** (exit `1`) means **the subject is disqualified.** Do not write
`CONTRACT.md`. There is no workaround: node-postgres cannot be told to use the
simple protocol, and knex cannot be told to stop parameterizing, so ~100% of
Strapi's SQL would be invisible to the cache. Per `docs/ADDING-A-PROJECT.md`, a
study that disqualifies its subject is a success — record it and stop.

### R1b — the check that actually needs care

**The interesting answer is not "did the query succeed."** It will succeed
either way: PgCache passes through what it cannot cache, so a pass-through and a
cache hit are byte-identical to the client. Latency cannot separate them either
— a PgCache hit is itself a SQL round-trip to an embedded Postgres
(`METHODOLOGY §6`), and these tables are warm in the origin's `shared_buffers`,
so the two can be within noise of each other in *either* direction.

The verdict is therefore taken from **counters**, never from bytes and never
from time.

#### The exact counter to read

**Primary evidence — PgCache's own metrics**, scraped from `METRICS_PORT` (9090
in-container, published on host `19092`):

```
curl -s localhost:19092/metrics | grep pgcache_queries_
```

| Counter | Read it as |
|---|---|
| **`pgcache_queries_cache_hit`** | **THE counter.** Its delta across the measurement window is the number of reads PgCache answered from its own store. Non-zero inside `BEGIN…COMMIT` ⇒ R1b PASS. |
| `pgcache_queries_cache_miss` | Registered but not yet populated, or evicted. |
| `pgcache_queries_uncacheable` / `_unsupported` | PgCache saw the statement and declined it. Useful for *why*. |
| `pgcache_queries_allowlist_skipped` | The table is not in `ALLOWED_TABLES`. A config bug, not a result. |
| `pgcache_protocol_extended_queries` / `_simple_queries` | R1's corroboration. |

(The binary registers these as `pgcache.queries.cache_hit` etc.; the Prometheus
exporter sanitizes the dots to underscores. `check.js` resolves the live names
against the scrape and reports **INCONCLUSIVE** rather than guessing if they
have drifted in a future PgCache build.)

**Corroborating evidence — origin-side `pg_stat_statements.calls` delta** for
that statement shape. A read served from cache does not reach the origin; a
pass-through does. The two evidence sources must agree; if they do not,
`check.js` downgrades the verdict to INCONCLUSIVE and says so in
`result.json → notes` instead of picking the convenient one.

#### The four phases, and why a bare "no hits" would be worthless

"No hits inside the transaction" on its own is equally consistent with
*"transactions block caching"* and with *"this config caches nothing"* (bad
`ALLOWED_TABLES`, the `DISK_LIMIT` footgun, admission threshold, dead
replication slot). So the phases are ordered to make every outcome attributable:

| Phase | What it runs | What it establishes |
|---|---|---|
| **P1** | `Q_OUT` out of transaction | R1 + **R1a**: this configuration caches *something*. If P1 fails, everything downstream is INCONCLUSIVE by construction. |
| **P2** | `Q_TX` **inside** `BEGIN…COMMIT`, **virgin** cache entry | **R1b-1.** This is the operative Strapi case: L1, R1 and R3 only ever execute inside a transaction (`STUDY §4.4`), so nothing ever warms them from outside. |
| **P3** | `Q_TX` out of transaction, fresh connection | **Control.** Identical statement, no transaction. If P3 caches and P2 did not, the transaction is the only remaining difference — clean attribution. |
| **P4** | `Q_TX` **inside** `BEGIN…COMMIT`, entry now **warm** | Separates *cannot register in-trx* from *cannot serve in-trx*. |

`Q_OUT` and `Q_TX` are deliberately **different statement texts** (different
column lists): `pg_stat_statements` would otherwise normalize both to the same
`queryid` and the origin-side corroboration could not be attributed to a phase.

#### The four R1b outcomes

| `result.json → checks.R1b.mode` | Meaning |
|---|---|
| `register_and_serve_in_transaction` | **PASS.** PgCache registered *and* served the entry entirely from inside a transaction. Strapi's whole Document Service read body is cacheable; coverage approaches 100% of read statements (`STUDY §1`). |
| `serve_only_cannot_register_in_transaction` | **FAIL, with a caveat worth recording.** PgCache will *serve* an in-transaction read but will not *register* one. In-transaction reads are warmable by a side channel but never self-warming. Strapi never issues L1/R1/R3 outside a transaction, so without a deliberate out-of-band warm-up path the effective ceiling is still 5 statements — but the ceiling is *reachable*, which changes the campaign design. |
| `passthrough_in_transaction` | **FAIL.** Every in-transaction read was passed through while the identical statement cached out of transaction. |
| `incorrect_or_error_in_transaction` | **FAIL, worse than pass-through** — a correctness failure, not a coverage one. Stop and report. |
| `shape_not_cacheable` | **INCONCLUSIVE.** The statement shape, not the transaction, is the confound. Re-run with a different `Q_TX`. |

---

## What R1b FAILING means — the ceiling it implies

Every Document Service operation, **including pure reads**, is wrapped in a real
DB transaction (`STUDY §1`: `document-service/common.ts:8-10` →
`repository.ts:766-776` → `database/src/index.ts:177-183,201-217`). The auth-path
queries A1–A5 run *outside* transactions; `L1` (the i18n default-locale
`core_store` read) does **not** — it sits in the `async.pipe` of `findMany`
(`repository.ts:326`) and `count` (`repository.ts:560`) and joins the ambient
transaction.

So if in-transaction reads pass through, the **unconditionally cacheable surface
is exactly 5 statements** — A1, A2, A3 and A4's two — all issued by the
users-permissions auth strategy before the Document Service is entered. For an
anonymous request it is exactly **1** (A5, which *replaces* the authed prefix
rather than being dropped from it).

| Workload | SELECTs/request | Cacheable | Fraction |
|---|---|---|---|
| W1 authed `findOne` | 7 | 5 | **71%** |
| W1L locale-pinned (`?locale=en`) | 6 | 5 | **83%** |
| authed list, no populate | 9 | 5 | **56%** |
| W3 populated list | 14 | 5 | **36%** |
| W1a anonymous `findOne` | 3 | 1 | **33%** |

**State the expected sign plainly rather than calling this "a narrower win."**
Those 5 statements are PK/equality lookups on `strapi_core_store_settings`,
`up_users`, `up_roles`, `up_permissions` — tiny, near-static tables the origin
serves from `shared_buffers` on every hit. Per `METHODOLOGY §6`, a PgCache hit is
*itself* a SQL round-trip to an embedded Postgres. Substituting a warm-buffer
origin hit for an embedded-Postgres round-trip is the exact mechanism that sank
the openFGA campaign, so the fallback's expected sign is **plausibly negative,
not merely small** (`STUDY §1`).

It can only go positive under a topology condition: PgCache co-located with the
app while the origin carries meaningful RTT (cross-AZ / cross-region), so the
saved network latency exceeds the embedded-Postgres lookup cost.

**Therefore R1b FAIL is a human decision, not an automatic stop.** `STUDY §1`'s
recommendation: if R1b resolves badly **and** the high-RTT topology condition
does not hold, the honest call is to **disqualify the subject** rather than run a
campaign whose most likely headline is "path B is slower". `run.sh` exits `0` on
this path and prints the decision banner; it deliberately does not decide for
you.

---

## Files

| Path | What it is |
|---|---|
| `docker-compose.yml` | origin Postgres 17 (CDC prerequisites) + PgCache `0.6.2-a3` + the client. Wiring copied from `openFGA/docker-compose.yml`. |
| `init-origin.sql` | `pg_stat_statements` + `REPLICATION` grant, at cluster init. Mirrors `openFGA/scripts/init-origin.sql`. |
| `schema.sql` | `articles` (integer PK + text cols) and `articles_tags_lnk` (integer PK + two FK-shaped cols + float order col). 60 + 180 rows. |
| `client/check.js` | The node-postgres client. All the reasoning above is restated in its header comment. |
| `run.sh` | Up → wait healthy → apply schema → run client → tear down → map exit code. |
| `out/result.json` | Machine-readable verdict, per-phase counter deltas, and the ceiling text. Written on every terminating path, including harness errors. |

### Configuration that is load-bearing

- **`ALLOWED_TABLES: "articles,articles_tags_lnk"`** — an unlisted table is
  silently skipped (`pgcache_queries_allowlist_skipped`) and every check reports
  100% miss for a reason unrelated to R1/R1b (`STUDY §5` "PgCache
  `ALLOWED_TABLES`", risk R1d).
- **`DISK_LIMIT` deliberately unset** — `openFGA/docker-compose.yml:158-161`:
  PgCache compares it against **total filesystem usage**, not its own cache, so
  a "reasonable" value on a busy host makes it judge itself permanently under
  disk pressure and refuse **every** registration — silently 100% miss. That
  failure would masquerade as an R1b FAIL.
- **`ADMISSION_THRESHOLD: 1`** — anything higher makes a short spike
  indistinguishable from "cannot cache".
- **`wal_level=logical` + slots + senders on the origin** — `STUDY §5`, risk R1c.
  `run.sh` asserts `pg_replication_slots.active` and warns loudly if it is false.
- **Both tables have primary keys** — PgCache cannot cache PK-less tables, and
  `STUDY §1` records that Strapi has none
  (`metadata/relations.ts:257-259,471-473`), so a PK-less spike table would fail
  for the wrong reason.

### Ports

Host ports are deliberately disjoint from everything `STUDY §7` reserves for the
real lab (1337, 2337, 3337, 5432, 6432, 9090, 9187, 8080, 3000) **and** from the
openFGA lab's host mappings, so the spike can run without disturbing anything.
Container-internal ports follow lab convention.

| | container | host |
|---|---|---|
| origin Postgres | `5432` | `15433` (`SPIKE_PORT_ORIGIN`) |
| PgCache SQL | `6432` | `16433` (`SPIKE_PORT_PGCACHE`) |
| PgCache metrics | `9090` | `19092` (`SPIKE_PORT_PGCACHE_METRICS`) |

---

## Scope

This spike answers a **protocol** question, not a relational-fidelity one. It
deliberately does not model draft/publish row pairs, i18n columns, components,
dynamic zones, the `_cmps` morph table, or Strapi's identifier shortener. Those
belong to the seeder and the contract, not here. It also does not run Strapi —
it exercises the same *wire behaviour* Strapi's driver stack produces, which is
the whole of what R1 and R1b are about.

# Triage Criteria

What disqualifies or demotes a candidate subject, and how to check it cheaply.

`subject-scout` reads this file rather than knowing these rules. That is
deliberate: disqualifiers are knowledge the platform *accumulates*, and a rule
that lives in a versioned file carries the case that taught it. When a spike or
a campaign teaches something new, it becomes an entry here — by human
judgement, not automatically.

**Cost** is `metadata` (answerable from README, manifests, docs, issues) or
`clone` (needs `git clone --depth 1` and a grep).
**Severity** is `fatal` (disqualifies alone), `demoting` (worsens the ranking),
or `scoping` (changes what the campaign measures, disqualifies nothing).

---

## C1 — Postgres must be the primary store

**Severity:** fatal · **Cost:** metadata

If the app's system of record is MySQL, Mongo, SQLite-by-default with Postgres
as an afterthought, or a managed service the lab cannot host, it is out of
scope. Postgres being *supported* is not enough — it must be a first-class,
production-realistic target.

**Test:** dependency manifest for a Postgres driver; docs for a "supported
databases" table; the default connection config. Note the default: an app that
ships SQLite by default can still qualify, but the lab must pin Postgres
explicitly and that goes in the study's knob table.

**Origin:** platform scope, `docs/PLATFORM.md`.

---

## C2 — Plain reads wrapped in transactions

**Severity:** fatal · **Cost:** clone

If the app's data layer wraps *pure reads* in an explicit transaction, its
entire read body is uncacheable: PgCache passes through everything inside
`BEGIN…COMMIT`. What remains cacheable is whatever runs outside a transaction —
typically only auth and settings lookups, which are warm-buffer PK reads whose
expected sign under a cache is plausibly negative.

**Test:** find where the ORM or data layer defines its read methods
(`findMany`, `findOne`, `count`, or the equivalent) and check whether a
transaction wrapper is applied to them unconditionally. Look for a
`wrapInTransaction`-style helper applied to the repository's whole method map,
and for an ambient transaction context that other queries join.

**Origin:** Strapi, 2026-08-03. Evidence: `strapi/spike/out/result.json` and
`strapi/STUDY.md` §1. The spike measured it directly — the identical
parameterized statement cached 25/25 outside a transaction with zero origin
calls, and 0/25 inside one with 25 origin calls. A pre-warmed entry was still
not served in-transaction, so warming by a side channel does not help.

**Note:** this is unusual. Most ORMs leave plain `SELECT`s in autocommit and
open transactions only for writes. Check rather than assume — in both
directions.

---

## C3 — Extended query protocol — RESOLVED, no longer a disqualifier

**Severity:** none (cleared) · **Cost:** n/a

Recorded so nobody re-litigates it. PgCache parses the extended query protocol
correctly: 25/25 statements counted `extended`, 0 `simple`, all served with
correct results (Strapi spike, 2026-08-03).

This was a blocking unknown for any subject whose driver has no simple-protocol
fallback — which is most of them, including node-postgres. It is now closed.
Do not spend triage effort on it.

---

## C4 — Non-cacheable SQL constructs on the read path

**Severity:** fatal if pervasive, demoting if confined · **Cost:** clone

PgCache cannot cache: views, tables under RLS, tables without a primary key,
LATERAL joins, recursive CTEs, FULL/CROSS joins, locking reads (`FOR UPDATE`),
and predicates containing volatile functions (`now()`, `random()`).

**Test:** grep the read path for these constructs. Weigh by *read traffic*, not
by distinct query shapes — an app with one exotic query on a cold admin screen
is fine; an app whose hot list endpoint selects from a view is not.

**Origin:** `docs/METHODOLOGY.md`; PgCache's own capability list.

---

## C5 — Writes on the read path

**Severity:** demoting · **Cost:** clone

A table that is written during a read request (last-seen timestamps, hit
counters, session touch) invalidates its own cache entry from the very path
that reads it.

**Test:** grep authentication and middleware paths for `UPDATE` statements.

**Origin:** Strapi, 2026-08-03 — `strapi_api_tokens` runs
`UPDATE … SET last_used_at` on every token-authenticated read
(`strapi/STUDY.md` §1, row T1). Not fatal there because the benchmark could use
JWT auth instead and exclude the table, which is the usual mitigation: exclude
the table, or pick an auth mode that avoids the path. Record it either way —
it constrains the workload design.

---

## C6 — Native application cache

**Severity:** scoping · **Cost:** metadata, confirmed by clone

Whether the app ships its own data cache decides if the campaign has three
paths (origin / PgCache / app-cache) or two. Its absence disqualifies nothing,
but a two-path campaign must say so in every report.

**Test:** dependency manifest for cache libraries; docs for a caching section;
grep the data layer for memoization that crosses requests. Distinguish real
data caches from request-scoped memoization and from schema-derived caches —
both are common and neither is path C.

**Origin:** `docs/METHODOLOGY.md` §1. Strapi has none
(`strapi/STUDY.md` §3), so its campaign would have been A vs B only.

---

## C7 — Query amplification

**Severity:** demoting when low · **Cost:** clone

The more datastore queries an app issues per user-facing request, the more
there is for a cache to absorb. An app that answers a request with one indexed
lookup has little headroom; one that issues a dozen has a lot.

**Test:** trace one representative read endpoint and count the statements —
auth, settings, the body, the count, per-relation fetches.

**Origin:** `docs/METHODOLOGY.md` §4. Strapi scored well here (9–14 statements
per authenticated request), which is why it survived to the spike despite the
transaction problem being visible in the study.

---

## C8 — Project viability

**Severity:** demoting, occasionally fatal · **Cost:** metadata

A dead project, an incompatible licence, or a codebase nobody can run locally
wastes the effort regardless of how cacheable its SQL is.

**Test:** last release date, commit activity, licence file, whether a
`docker-compose` or equivalent exists for local runs, and whether the schema
can be created without a proprietary migration path.

**Origin:** platform practicality.

---

## How an entry gets added

A criterion earns a place here when a spike, a campaign, or a study produces
evidence for it — not when someone suspects it. Each entry must carry the case
that taught it, with a date and a pointer to the artifact holding the evidence.

A criterion that turns out to be wrong gets **corrected in place with a note**,
not deleted. C3 is the model: it was a real blocker, it was resolved by
measurement, and the record of that is what stops it being re-litigated.

---

## C9 — The origin's per-query cost must exceed the proxy's floor

**Severity:** demoting, and fatal in the extreme case · **Cost:** clone, then
one measurement

C1–C8 all ask *can this be cached?* None of them asks *is it worth caching?* A
subject can pass every structural screen and still be a bad subject, because
PgCache is a network proxy with an embedded PostgreSQL: a cache **hit** is a
full SQL round trip to a local database, not a memory read. That gives it a
floor. Below that floor, a "cache" is pure overhead.

**The rule:** estimate the origin's amortised cost per query. If it is not
comfortably above PgCache's serving floor — measured at **~45 µs** on dedicated
AKS nodes — the subject cannot show a gain no matter how the cache is
configured, and the campaign will measure proxy overhead instead of caching.

**Test, in order of cost:**

1. *Does the working set fit in RAM?* Sum the table sizes the read path touches
   and compare against the origin's `shared_buffers` **and** the node's memory.
   If the whole dataset lives in the buffer pool, the origin is already a cache
   and the experiment is cache-versus-cache.
2. *Are the queries cheap by construction?* Single-table lookups with equality
   predicates on covering indexes, returning few rows, are ~40 µs served warm.
   Joins, aggregates, sorts, and anything scanning are not. PgCache's own
   `mv_size_ratio` exists to gate materialisation — a subject whose results are
   never worth materialising has no upside.
3. *Measure it.* Latency ÷ queries-per-request from a baseline run. This is one
   number and it settles the question.

**Interaction with C7 (query amplification).** Amplification multiplies
whichever side is bigger. High amplification over *expensive* queries is the
best possible case; high amplification over *cheap* ones is the worst, because
the proxy's per-query overhead is what gets multiplied.

**Origin:** OpenFGA, campaigns r5–r6, 2026-08-03. OpenFGA passed C1, C2, C4, C5
and C7 cleanly and was still close to a worst case. Measured: the origin served
each query in **39.5–42.2 µs**, PgCache in **41.9–47.0 µs** — an overhead of
about 5 µs per query, which is *good* proxy behaviour, multiplied by 104–171
queries per `Check` into a visible per-request penalty. The whole 84,598-tuple
dataset fit in a 1 GB `shared_buffers` at a **99.93%** buffer hit rate. We had
built a read cache in front of a database that was already serving entirely
from memory.

Evidence: `openFGA/benchmark-docs/08-results-aks-r6.md`,
`openFGA/benchmark-docs/07-pgcache-configuration.md`.

**Do not fix this by strangling the origin.** Capping the origin's memory to
manufacture I/O produces a scenario, not a measurement. If a subject needs the
origin crippled to show a win, the subject is wrong — pick a different one.

### C9b — and the datastore must own a meaningful share of request latency

**Severity:** fatal · **Cost:** one measurement

C9 above catches an origin that is *too fast to beat*. It does not catch the
opposite failure, which is just as fatal: an application slow enough that the
datastore is a rounding error in its own request.

A datastore cache can only return the share of latency the datastore actually
owns. If a request spends 90% of its time in the application's own code, the
ceiling on any cache placed below it is 10% — before considering hit ratio,
warm-up, or configuration.

**Test.** Instrument one representative request and report `db_time / wall_time`.
In Django that is `CaptureQueriesContext` summing `q['time']`; equivalents exist
in every framework. Do it before any integration work.

**Threshold.** Below ~30% and the subject cannot produce a headline result.
Between 30% and 60%, proceed and state the ceiling in every report. Above 60% is
the profile the platform is looking for.

**Measure it on the deployment you will publish.** An in-process test client on a
virtualised laptop inflates the application side and understates the share. That
caveat is why the NetBox verdict is "discouraging, one test from settled" rather
than closed.

**Origin:** NetBox, Probe 0, 2026-08-04. Measured `db_time / wall_time` of
**3.2%** on the UI prefix list (117 statements, 6 ms of 190 ms) and **27.3%** on
the API device list (13 statements, 25 ms of 91 ms). PgCache was slower than the
uncached origin on both, stock and with pinned tables plus a warm cache, at two
rungs. Evidence: `netbox/RESULTS-probe0.md`.

**The pair, stated together:** openFGA failed C9 because the origin was too fast
(~40 µs/query, and the datastore was most of the request). NetBox failed C9b
because the application was too slow (the datastore was 3-27% of the request).
A subject must clear both to be worth a campaign.

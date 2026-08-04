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

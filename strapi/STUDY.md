# API Study — Strapi

> **STATUS: DISQUALIFIED — 2026-08-03, on empirical evidence.**
>
> The R1/R1b spike (`strapi/spike/`, result in `spike/out/result.json`) resolved
> both blocking questions:
>
> - **R1 PASS** — PgCache parses the extended query protocol (25/25 extended,
>   0 simple). The fatal risk is dead.
> - **R1b FAIL** — PgCache passes through *every* read inside an explicit
>   `BEGIN…COMMIT`, classifying them `unsupported`. The identical statement
>   outside a transaction cached 25/25 with zero origin calls; inside a
>   transaction, 0/25 with 25 origin calls. A pre-warmed entry was still not
>   served in-transaction, so this is "cannot serve", not "cannot register" —
>   no side-channel warming works around it.
>
> Since every Document Service read is transaction-wrapped (§1), the
> unconditionally cacheable surface collapses to the 5 out-of-transaction auth
> statements, whose expected sign this study argues is plausibly negative.
> Disqualification follows the decision rule written in §1 **before** the
> result was known.
>
> Nothing was built. Cost: one day of discovery plus one hour of spike.
>
> **This verdict is about PgCache's current in-transaction behaviour, not about
> Strapi's SQL, which is unusually cache-friendly.** If PgCache gains the
> ability to serve reads inside transactions, re-run the spike — coverage would
> approach 100% of read statements and Strapi becomes a first-rate subject.

> Sections map 1:1 to `docs/ADDING-A-PROJECT.md` Phase 1. A study that
> disqualifies the subject is a success, not a failure.

**Subject:** Strapi v5 (monorepo develop @ `23a306eae67ad1e1c277509794066e0c0a7ee625`, `@strapi/database` 5.51.1 — note: `lerna.json:2` reads `5.7.0`; the two agents disagreed on which is the subject version. Package manifests are authoritative for the audited code; record both in any report), https://github.com/strapi/strapi
**Storage layer audited:** `packages/core/database/src/**` (Knex query builder; Postgres dialect `packages/core/database/src/dialects/postgresql/index.ts`), document service `packages/core/core/src/services/document-service/**`, auth strategies `packages/plugins/users-permissions/server/src/strategies/users-permissions.js`, `packages/core/admin/server/src/strategies/*.ts`
**Author / date:** Discovery agent crew (sql-auditor, cache-hunter, oracle-designer), 2026-08-02

---

## 1. SQL profile of the read path

Strapi is DB-agnostic through Knex. SQL is *generated* in `packages/core/database/src/query/query-builder.ts` (`getKnexQuery()`, lines 585–737): every read is a parameterized `SELECT` over app-created plain tables; relations become `LEFT JOIN`s onto `_lnk`/`_cmps`/`_mph` join tables (`query/helpers/join.ts:143-172`, always `leftJoin` or `innerJoin` — never FULL/CROSS). Any query with joins gets `SELECT DISTINCT` (`query-builder.ts:518-520,606-608`) or `COUNT(DISTINCT …)` (`query-builder.ts:612-620`). All tables including join tables have an `id` increments PK (`metadata/relations.ts:257-259,471-473`). Populate is **not** done via JOIN fan-out into the root query: each populated relation is a separate batched `SELECT … WHERE fk IN (…)` (`query/helpers/populate/apply.ts`).

**Critical structural finding:** every Document Service operation — including pure reads `findMany`/`findOne`/`count` — is wrapped in a real DB transaction: `wrapInTransaction` (`packages/core/core/src/services/document-service/common.ts:8-10` → `repository.ts:766-776`) calls `strapi.db.transaction()`, which opens `this.connection.transaction()` → `BEGIN … COMMIT` (`packages/core/database/src/index.ts:177-183,201-217`). The entire content-API read body executes inside explicit transactions. The auth-path queries (rows A1–A5, T1, M1–M3 below) run *outside* transactions.

All rows below are **code intent** (static trace); `pg_stat_statements` was not run (see "Not determined from code alone").

| # | App operation | SQL emitted (shape) | Cacheable? | Why / why not |
|---|---|---|---|---|
| A1 | u&p auth: plugin settings, every authed content-API request | `SELECT t0.* FROM strapi_core_store_settings t0 WHERE t0.key = ? AND t0.environment IS NULL AND t0.tag IS NULL LIMIT 1` | **Yes** | Single-table, equality + IS NULL predicates, PK table; no volatile fns. `services/core-store.ts:79-91`, null→`whereNull` via `where.ts:264-268`. Uncached in app — fires per request (`users-permissions.js:8-10,31`) |
| A2 | u&p auth: fetch user | `SELECT t0.* FROM up_users t0 WHERE t0.id = ? LIMIT 1` | **Yes** | PK equality. `services/user.js:118-120`; `entity-manager/index.ts:292-303` |
| A3 | u&p auth: populate user role | `SELECT DISTINCT t0.*, t1.user_id AS __strapiuser_id FROM up_roles t0 LEFT JOIN up_users_role_lnk t1 ON t0.id = t1.role_id WHERE t1.user_id IN (?)` | **Yes** | LEFT JOIN + DISTINCT, equality IN; join tables have PKs. `populate/apply.ts:105-187` (XtoOne joinTable branch), DISTINCT from `query-builder.ts:518-520` |
| A4 | u&p auth: role permissions | `SELECT t0.id FROM up_roles t0 WHERE t0.id = ? LIMIT 1` then `SELECT DISTINCT t0.*, t1.role_id AS __strapirole_id FROM up_permissions t0 LEFT JOIN up_permissions_role_lnk t1 ON t0.id = t1.permission_id WHERE t1.role_id IN (?)` | **Yes** | `permission.js:13-15` → `load()` (`entity-manager/index.ts:1709-1731`) + `populate/apply.ts:190-315` (oneToMany) |
| A5 | u&p auth: public role permissions (anonymous requests) | `SELECT DISTINCT t0.* FROM up_permissions t0 LEFT JOIN up_permissions_role_lnk t1 ON … LEFT JOIN up_roles t2 ON t1.role_id = t2.id WHERE t2.type = ?` | **Yes** | Relation filter creates pivot joins (`where.ts:166-185` → `join.ts:29-54`). `permission.js:22-26`, `users-permissions.js:64-67`. Hot + near-static: ideal cache target |
| L1 | i18n default locale (per read pipeline stage on localized CTs when no `locale` param) | Same shape as A1, `key = 'plugin_i18n_default_locale'` | **Conditional** | `document-service/internationalization.ts:15-16` → `plugins/i18n/server/src/services/locales.ts:51`. Uncached; fires once per findMany *and* once per count. **Runs INSIDE the transaction** — `i18n.defaultLocale(contentType)` sits in the `async.pipe` of `findMany` (`repository.ts:326`) and `count` (`repository.ts:560`), both wrapped by `wrapInTransaction` (`repository.ts:766-777`); the core-store query joins the ambient transaction via `transactionCtx` (`database/src/index.ts:180-183`) |
| R1 | Content API `GET /api/:ct` root list (D&P + i18n) | *Inside `BEGIN…COMMIT`:* `SELECT t0.* FROM <tbl> t0 WHERE t0.published_at IS NOT NULL AND t0.locale = ? ORDER BY t0.id ASC LIMIT ? OFFSET ?` | **Conditional** | Shape is cacheable (no volatile fns; `published_at IS NOT NULL` from `draft-and-publish.ts:75-77` (the `draft` branch with `$null` is `:78-79`); stable `id ASC` appended by `query-builder.ts:486-516`). **But it runs inside an explicit transaction** (`common.ts:8-10`, `index.ts:181-183`) — cacheable only if PgCache serves reads inside `BEGIN…COMMIT` |
| R2 | Same, with relation/component filter | `SELECT DISTINCT t0.* FROM <tbl> t0 LEFT JOIN <lnk> t1 ON … LEFT JOIN <target> t2 ON … WHERE t2.<col> = ? …` | **Conditional** | Joins built in `where.ts:166-185`; DISTINCT `query-builder.ts:518-520,606-608`. Same transaction caveat |
| R3 | Content API count (default `withCount=true`) | *Own `BEGIN…COMMIT`:* `SELECT COUNT(t0.id) AS count FROM <tbl> t0 WHERE …` (`COUNT(DISTINCT t0.id)` with joins) | **Conditional** | `core-api/service/collection-type.ts:37-38`, `pagination.ts:45-67` (count defaults on), `entity-manager/index.ts:316-330`, `query-builder.ts:612-620`. Transaction caveat |
| R4 | Populate toOne via FK | `SELECT t0.*, t0.<ref_col> FROM <target> t0 WHERE t0.<ref_col> IN (?, …)` | **Conditional** | Batched, equality IN. `populate/apply.ts:89-94`. Runs inside the parent read's transaction (`query-builder.ts:748-756` executes populate within `execute()`) |
| R5 | Populate toMany / component via join table | `SELECT DISTINCT t0.*, t1.<join_col> AS __strapi<col> FROM <target> t0 LEFT JOIN <lnk\|_cmps> t1 ON t0.id = t1.<inv_col> [AND t1.field = ?] WHERE t1.<join_col> IN (…) ORDER BY t1.order` | **Conditional** | `populate/apply.ts:294-307,383-396`; the ON extra-condition is `on: { field: name }` **only** — both the component branch (`transform-content-types-to-models.ts:104-106`) and the dynamiczone branch (`:147-149`) define just `field`. `component_type` is a morph *type* column resolved by the separate per-type query (R7), **not** a join predicate. Applied via `onVal` (`join.ts:156-164`). ORDER BY the join-table `order` column, which is `float` not integer (`metadata/relations.ts:280-285`) |
| R6 | Populate count (`count: true`, admin relation counts) | `SELECT t1.<join_col>, COUNT(*) AS count FROM <target> t0 LEFT JOIN <lnk> t1 ON … WHERE t1.<join_col> IN (…) GROUP BY t1.<join_col>` | **Conditional** | `populate/apply.ts:129-142,257-270`. Aggregation over PK'd tables |
| R7 | Dynamic zone / media (morph) populate | `SELECT t0.* FROM <tbl>_cmps\|files_related_mph t0 WHERE t0.<join_col> IN (…) ORDER BY <join_col>, order` then, per distinct component type, `SELECT t0.*, t0.id FROM <cmp_tbl> t0 WHERE t0.id IN (…)` | **Conditional** | `populate/apply.ts:510-607` (morphToMany: 1 join-table query + 1 query *per component type*). Plain SELECTs |
| R8 | Search `_q` param | `… WHERE (t0.id::text ILIKE ? OR t0.<every-string-col>::text ILIKE ? OR …)` | **Yes (shape)** | Postgres branch `query/helpers/search.ts:40-51`. No volatile fns; large shape space (one predicate per searchable column) |
| R9 | Sort by related field (deep sort) | Outer `SELECT t3.* FROM <tbl> t3 INNER JOIN (SELECT t2.id, …, ROW_NUMBER() OVER (PARTITION BY t2.id ORDER BY …) AS __strapi_row_number FROM (<base joined query>) t2) t? ON … AND __strapi_row_number = 1 ORDER BY … LIMIT ?` | **Yes (shape)** | Window function, subquery join — but plain SELECT, no LATERAL/recursive CTE. `order-by.ts:166-314` |
| R10 | Sort by `status` (admin list) | `CASE WHEN NOT EXISTS(SELECT 1 FROM <tbl> sub WHERE sub.document_id = t0.document_id AND sub.published_at IS NOT NULL …) THEN 0 WHEN t0.updated_at > (SELECT MAX(sub.updated_at) …) THEN 1 ELSE 2 END` in SELECT + ORDER BY | **Yes (shape)** | Correlated subqueries, deterministic, parameterless. `order-by.ts:30-42` |
| M1 | Admin auth: session check (every admin request) | `SELECT t0.* FROM strapi_sessions t0 WHERE t0.session_id = ? LIMIT 1` | **Yes, churny** | `strategies/admin.ts:38` → `session-manager.ts:729-746` → provider `findBySessionId` (`session-manager.ts:87-92`). Expiry compared in JS, not `NOW()` in SQL. Table is hot-written (refresh/rotation/cleanup) |
| M2 | Admin auth: user + roles | `SELECT t0.* FROM admin_users t0 WHERE t0.id = ? LIMIT 1` + roles populate (join via `admin_users_roles_lnk`, shape as A3) | **Yes** | `strategies/admin.ts:50-52` |
| M3 | Admin RBAC: user permissions | `SELECT DISTINCT t0.* FROM admin_permissions t0 LEFT JOIN admin_permissions_role_lnk t1 … LEFT JOIN admin_roles t2 … LEFT JOIN admin_users_roles_lnk t3 … LEFT JOIN admin_users t4 … WHERE t4.id = ?` | **Yes** | Nested relation filter `{ role: { users: { id } } }` — `services/permission/queries.ts:86-87`, joins via `where.ts:166-185`. Fires on every admin request (`permission/engine.ts:73-75`) |
| T1 | API-token auth (content API via token) | `SELECT <fields> FROM strapi_api_tokens t0 WHERE t0.access_key = ? LIMIT 1` + 3 populate queries (`permissions`, `adminPermissions`, `adminUserOwner`) **then `UPDATE strapi_api_tokens SET last_used_at = ? WHERE id = ?`** | **Read: yes. Poisoned by write** | `strategies/content-api-token.ts:24,41` → `services/api-token.ts:615-631` (`POPULATE_FIELDS` line 123); `api-token-utils.ts:34-55`: UPDATE on the read path — unconditional when `last_used_at IS NULL`, else throttled to 1/hour/token. Invalidates the token-table cache from its own read path |
| X1 | Session GC (1 in 50 session ops) | `DELETE FROM strapi_sessions WHERE absolute_expires_at < ?` | N/A (write) | `session-manager.ts:227-230` (counter), `120-124`. Timestamp is a JS-bound parameter, not `NOW()` — no volatile fn reaches SQL |

**Against PgCache's non-cacheable list:** no views, no RLS, no PK-less tables (`metadata/relations.ts:257-259,471-473`), no LATERAL, no recursive CTEs, no FULL/CROSS JOIN (only `leftJoin`/`innerJoin`, `join.ts:143-156`), no volatile functions in predicates anywhere in the generated SQL (all `NOW()`-equivalents are JS `new Date()` bound as parameters — `api-token-utils.ts:27`, `session-manager.ts:739`). Locking reads (`forUpdate`, `query-builder.ts:322-325,665-667`) exist only on write/admin paths: EE license check `core/src/ee/index.ts:117`, `admin/server/src/services/user.ts`, `content-releases/server/src/services/release.ts`, `upload/server/src/services/folder.ts` — none on the content read path.

**Cacheable fraction of read traffic:** two numbers, and only the second one decides anything.

By *statement shape*, ≈95%+ of distinct shapes are cacheable (everything except T1's piggy-backed UPDATE). This number is not decision-relevant — METHODOLOGY requires the fraction over **read traffic**, not over distinct shapes.

By *executability under PgCache*, the decisive variable is in-transaction reads, and the fallback surface is exactly **5 statements** — A1, A2, A3, and A4's two — all issued by the u&p auth strategy before the Document Service is entered. L1 is **not** in that set (it runs inside the transaction, see the table). Traffic-weighted, per workload:

| Workload shape | SELECTs/request | Cacheable if in-transaction reads pass through | Fraction |
|---|---|---|---|
| Authed list (`GET /api/:ct`) | 9 | 5 | 56% (38% of ~13 wire commands) |
| Authed findOne | 7 | 5 | 71% |
| Authed populated list (W3) | ~14–15 | 5 | ~35% |
| Anonymous list | 5 | 1 (A5) | 20% |
| Anonymous findOne | 3 | 1 (A5) | 33% |

If PgCache *can* serve single-statement reads inside `BEGIN…COMMIT`, coverage approaches 100% of read statements in every row above.

**Expected sign of the fallback case — state this plainly rather than calling it "a narrower win".** The five unconditionally cacheable statements are PK/equality lookups on `strapi_core_store_settings`, `up_users`, `up_roles`, `up_permissions`: tiny, near-static tables the origin serves from `shared_buffers` on every hit. Per METHODOLOGY §6, a PgCache hit is *itself* a SQL round-trip to an embedded Postgres. Substituting a warm-buffer origin hit for an embedded-Postgres round-trip is the exact mechanism that sank the openFGA campaign, so the fallback's expected sign is **plausibly negative, not merely small**. It can only go positive under a topology condition: PgCache co-located with the app while the origin carries meaningful RTT (cross-AZ/region), so the saved network latency exceeds the embedded-Postgres lookup cost. If R1b resolves badly (in-transaction reads pass through) **and** that topology condition does not hold, the honest call is to disqualify the subject rather than run a campaign whose most likely headline is "path B is slower".

**Volatile exceptions and exclusion plan:**

- `strapi_api_tokens` (T1) → UPDATE-on-read; exclude table via ALLOWED_TABLES or benchmark with u&p JWT auth instead of API tokens.
- `strapi_sessions` (M1/X1) → correct-but-churny; exclude (also keeps stray admin-UI traffic from poisoning it — see §4 risk R5).
- `up_users` (A2) → written on login (`resetPasswordToken`, confirmation); low write rate, keep.
- No SQL-level volatile functions to rewrite — nothing needs exclusion on that ground.

**Verdict:** **Marginal — conditionally good.** Strapi's generated SQL is unusually clean for an ORM-heavy app: fully parameterized, PK'd tables only, no views/RLS/LATERAL/recursive CTEs, no locking or volatile functions on the read path, and its per-relation batched populate strategy produces exactly the repetitive, cache-friendly shapes PgCache wants (equality/IN predicates over join tables). Two things keep it from being an OpenFGA-class subject: (1) every Document Service read — i.e. the whole content API body — runs inside an explicit `BEGIN…COMMIT` (`document-service/common.ts:8-10`, `database/src/index.ts:181-183`), so the campaign's outcome hinges entirely on PgCache's in-transaction read semantics; this must be resolved (PgCache capability check, or a patched/no-transaction path documented as a confound) before committing to a full campaign. (2) The remaining unconditionally cacheable traffic is the auth/permissions hot path — exactly 5 near-static queries per authed request, 1 per anonymous request — and per the fraction analysis above, its expected sign under PgCache is plausibly **negative**, because those are precisely the warm-buffer PK lookups that PgCache replaces with an embedded-Postgres round-trip. Proceed only after resolving R1 (protocol) and R1b (in-transaction reads) with a 30-minute spike against PgCache itself. If in-transaction reads pass through, the ceiling is 56% of statements on the flagship authed-list workload (35% on W3, 20% anonymous) with an adverse expected sign — in that case disqualify unless the campaign is explicitly reframed around a high-RTT origin topology where the latency saved exceeds the lookup cost.

## 2. Query amplification

**Datastore queries per user-facing operation** (measured by static code trace, *not* app metrics — Strapi exposes no per-request query counter; numbers below must be verified with `pg_stat_statements` deltas before the campaign):

| User-facing operation | SQL statements | Breakdown (evidence) |
|---|---|---|
| Content API `GET /api/:ct` — authenticated (JWT), localized CT, D&P, no populate | **9 SELECTs + 2×(BEGIN+COMMIT) ≈ 13 wire cmds** | Auth 5 (A1 `users-permissions.js:31`, A2+A3 `user.js:118-120`, A4×2 `permission.js:13-15`); default-locale 2 (L1 runs in both the findMany and the count pipeline, `repository.ts:326,560`); body 1 (R1) + count 1 (R3, own transaction, `collection-type.ts:32-38`) |
| Same, anonymous (public role) | **5 SELECTs + 2 trx pairs** | A5 (1) + L1×2 + R1 + R3 |
| Same, + `populate=` N flat relations | **+N SELECTs** | 1 batched query per relation regardless of row count (`populate/apply.ts:89-94,294-307,383-396`) |
| Same, + dynamic zone with K component types | **+1+K SELECTs** | Join-table fetch + per-type query (`populate/apply.ts:528-586`) |
| Content API `GET /api/:ct/:documentId` (authed) | **7 SELECTs + 1 trx pair** | Auth 5 + L1 + R1-first (`repository.ts:351-367`); no count |
| Admin panel request (any endpoint), auth overhead only | **4 SELECTs** | M1 (sessions) + M2×2 (user + roles) + M3 (permissions) — `strategies/admin.ts:38,50-52`, `permission/queries.ts:86-87` |
| Admin content-manager list view | **4 auth + 2 (findPage: findMany‖count, `entity-repository.ts:54-70`) + 1 per relation-count column (R6) + status/locale metadata queries** | Metadata fan-out not fully traced (see "Not determined") |
| API-token content request | **1 + 3 populate SELECTs + 0–1 UPDATE** (then content as above minus A1–A4) | `api-token.ts:615-631`, `api-token-utils.ts:34-55` |

**Does the app batch reads, or issue them one by one?** Batched-per-relation, not per-row: populate collects distinct parent keys and issues one `WHERE … IN (…)` query per relation level (`populate/apply.ts:77-79,117-119,334-336`) — no classic N+1 on rows, but amplification grows linearly with populate depth × breadth, and dynamic zones add one query per component type. `findMany` and `count` for a page are issued **sequentially**, in separate transactions: `collection-type.ts:32-38` awaits `documents().findMany()` and only then, inside `if (shouldCount(...))`, awaits `documents().count()` (each wrapped via `repository.ts:766-777`). `Promise.all` exists only on the *admin* path (`entity-manager/entity-repository.ts:57-60`) — do not generalize it to the content API. Two serialized transaction chains per list request is a materially different latency model under a proxy than two concurrent ones, and both paths pay it. Auth queries (5/request) are issued sequentially, one by one, uncached at every layer — the dominant fixed amplification, and the only read traffic that reaches Postgres outside a transaction. Attractive as a cache target by *count*, but see §1's expected-sign analysis before treating it as the win story: they are warm-buffer PK lookups on tiny tables.

## 3. Native cache (path C)

| Question | Answer |
|---|---|
| Does the app ship a cache? | **No.** Neither `@strapi/database` nor `@strapi/core` contains any cache of database reads. `packages/core/database/src/` has zero cache constructs (grep for `cache` over the whole package returns nothing outside tests); every `strapi.db.query(...)` goes through the entity manager straight to Knex (`packages/core/database/src/index.ts:156-162`). No cache library (`lru-cache`, `keyv`, `node-cache`) appears in any core/plugin `package.json`. The official plugin set is `cloud, color-picker, documentation, graphql, i18n, sentry, users-permissions` — no cache plugin ships in the repo (`packages/plugins/`). |
| Coherence model | N/A — no data cache exists to be coherent. |
| Default state / flags | N/A. |
| What it covers / does not cover | N/A. |

**What *does* exist (and why none of it is path C):**

- **Request-scoped documentId→entryId map** — the Document Service batches and memoizes `documentId → id` lookups *within one request*, stored on request state and cleared after (`packages/core/core/src/services/document-service/transform/data.ts:23-33`, `.../transform/id-map.ts:53-54,139-142`). Reduces amplification; never crosses requests.
- **Schema-derived populate memo** — `deepPopulateCache`, an unbounded module-level `Map` keyed `uid + opts` (`.../document-service/utils/populate.ts:10-17,70`). Caches *computed populate shapes from the content-type schema*, not row data; no invalidation (schema is fixed at runtime).
- **Query-string parse memo**, per request object (`packages/core/core/src/middlewares/query.ts:26-33`).
- **Apollo `cache: 'bounded'`** in the GraphQL plugin (`packages/plugins/graphql/server/src/bootstrap.ts:181`) — parsed-document/validation cache, not response or data caching.
- **OpenAPI spec file cache** — enabled by default, TTL 60 s, but the route itself defaults to `access: 'disabled'` (`packages/core/core/src/configuration/index.ts:40-63`; `packages/core/core/src/services/server/openapi.ts:31-33`).
- **HTTP static caching only**: `koa-static` `maxage: 60000` for `/public` files (`packages/core/core/src/middlewares/public.ts:8`), favicon `maxAge: 86400000` (`.../favicon.ts:10`). **API responses carry no ETag/Cache-Control** — no etag middleware exists anywhere in core (grep), so there is no HTTP-level revalidation path either.

**Anti-cache, load-bearing:** authorization is re-read from Postgres on *every* content-API request. Public request: 1 query (`findPublicPermissions`, `users-permissions.js:65-67` → `services/permission.js:22-26`). Authenticated request: 3+ queries — user+role fetch (`services/user.js:118-120`), plugin-store `advanced` settings (`users-permissions.js:8-10,31`; the core store has no cache, every `get` is a DB query — `packages/core/core/src/services/core-store.ts:79ff`), role permissions load (`services/permission.js:13-15`). The CASL ability is rebuilt per request (`users-permissions.js:49`).

**If no native cache:** comparison is **A vs B only — must be noted in every report**, and the staleness probe has no TTL comparison column. The community `strapi-plugin-rest-cache` is not part of this repo and not an official plugin; adopting it as path C is a scope decision, not a discovery (see Open questions).

## 4. Correctness check design

**Chosen approach: analytical oracle (primary) + differential gate (secondary).**

We author the benchmark app's content schema, so the data mass is fully synthesizable and the correct response to any read is derivable from the request parameters without querying. The oracle is the primary check; the A/B differential is the gate that runs on top of it.

**Cacheability accounting used throughout this section.** Every Document Service operation is wrapped in a real transaction — `wrapInTransaction` is applied to `findMany`, `findOne`, `count`, `publish` and every other repository method (`repository.ts:766-776` → `common.ts:8-10` → `database/src/index.ts:177-183`), and any query issued while that context is live joins it (`query-builder.ts:739-748`: `const transaction = transactionCtx.get(); if (transaction) qb.transacting(transaction)`). **L1 (the i18n default-locale `core_store` SELECT) is inside the transaction**: `i18n.defaultLocale(contentType)` sits in the `async.pipe` of `findMany` (`repository.ts:326`), `findOne` (`repository.ts:359`) and `count` (`repository.ts:560`), and reaches the DB through `strapi.store(...).get()` → `db.query('strapi::core-store').findOne()` (`core-store.ts:79-91`) inside that same context. The **unconditionally cacheable (out-of-transaction) surface is exactly rows A1-A4** — the users-permissions auth prefix, which runs in the Koa auth strategy before any document call (`users-permissions.js:24,31,44-46`). For anonymous requests it is exactly **A5** (`users-permissions.js:65-67` → `services/permission.js:22-26`), which *replaces* the authenticated prefix rather than being dropped from it.

Per-request statement counts, recomputed (static trace; `pg_stat_statements` verification still required per §2):

| Operation | Statements | Out-of-trx cacheable | In-trx (gated on R1b) |
|---|---|---|---|
| authed `findOne` (no `locale` param) | 7 | 5 (A1,A2,A3,A4a,A4b) = 71% | L1 + body = 2 |
| authed `findOne` **with** `?locale=en` | 6 | 5 = 83% | body = 1 (L1 skipped, `internationalization.ts:19-29`) |
| anonymous `findOne` | **3** (A5 + L1 + body) | 1 = 33% | 2 |
| authed list, no populate | 9 (5 + L1×2 + R1 + R3) | 5 = 56% | 4 |
| authed list + author + tags + dz(K=2) | 14 | 5 = 36% | 9 |

### 4.1 The oracle

**Content model (§6's COPY columns are derived from this schema *and confirmed against the step-2 `information_schema` dump*, which is the actual contract — see the `locale` caveat in §6).** One collection type `article` — `"collectionName": "articles"` declared explicitly at the top level of `schema.json` so the table name is fixed (`loaders/apis.ts:152`: `collectionName = schema.collectionName || schema.info.singularName`) — with `draftAndPublish: true`, i18n on, single locale `en`. Plus `tag` and `author` (both D&P off, i18n off) and one dynamic zone with two component types.

Relation attributes, declared with their exact type and directionality:

| Attribute on `article` | Declaration | `hasOrderColumn` (`metadata/relations.ts:66`) | `hasInverseOrderColumn` (`relations.ts:67-68`) | Physical result |
|---|---|---|---|---|
| `author` | `{ "type":"relation", "relation":"manyToOne", "target":"api::author.author" }` — **unidirectional**, no `inversedBy`/`mappedBy` | `isAnyToMany(manyToOne)` = **false** (`relations.ts:49-52`) | `isBidirectional` = **false** (`relations.ts:54-56`) | join **table** (`relations.ts:161-166`), columns `id, article_id, author_id` — **no `author_ord`, no `article_ord`** |
| `tags` | `{ "type":"relation", "relation":"manyToMany", "target":"api::tag.tag" }` — **unidirectional** | `isAnyToMany(manyToMany)` = **true** | false | join table `id, article_id, tag_id, tag_ord` — `tag_ord` only, **float** (`relations.ts:543-548`) |
| `dz` | `{ "type":"dynamiczone", "components":[...×2] }` | n/a (morphToMany) | n/a | rows in `<collection>_cmps` with `entity_id, cmp_id, component_type, field, order` (`transform-content-types-to-models.ts:20-23,24-40,206-240`); `order` is **float** (`:233-239`), `component_type` holds the component **UID** and `field` the attribute name `dz` (`document-service/components.ts:83,97`) |
| `createdBy`/`updatedBy` | injected by core, `useJoinTable: false` (`domain/content-type/index.ts:121-145`) | — | — | join **columns** `created_by_id`, `updated_by_id` on `articles` |

Choosing both relations unidirectional is deliberate: it removes every inverse order column, so the seeded join tables have the minimum column set and populate ordering has exactly one source of truth.

**Generator — every field of article *i* is a pure function of *i*:**

| Field | Generator | Why |
|---|---|---|
| `document_id` | `letter(i) ‖ base36(hmac(seed,i))[0:23]`, where `letter(i) = 'a' + (i mod 26)` — 24 chars, `[a-z][a-z0-9]{23}` | Strapi's default is `createDocumentId = createId` from `@paralleldrive/cuid2` (`transform-content-types-to-models.ts:3,193`), which **always begins with a letter**. Forcing a leading letter makes the seeded ids shape-identical to cuid2 and retires R3 by construction rather than by S0 observation. No format validation exists on the read path, so lookup is plain string equality; there is also no unique constraint on `document_id` (the only declared index is the composite btree on `(document_id, locale, published_at)`, `transform-content-types-to-models.ts:324-340`) — **the seeder asserts uniqueness itself** (§6). |
| `id` (PK) | published = `2i`, draft = `2i−1` | response `id` is derivable; v5 responses carry both `id` and `documentId` (`core-api/controller/transform.ts:38-69`) |
| `title`, `slug` | `"art-{i}"` | byte-reconstructible |
| `body` | seeded-PRNG text keyed on `i`, ~1 KB (kept under the TOAST threshold) | byte-reconstructible, dominates row size |
| `views` (integer) | `(i · 2654435761) mod 10^6` | makes `filters[views][$gte]=X` membership **and** `meta.pagination.total` analytically computable |
| `author` | `i mod N_authors` | 1 populate query, derivable |
| `tags` | `{i mod N_t, (i·7+1) mod N_t, (i·13+2) mod N_t}` with `tag_ord = 1,2,3` in that order | populate result order is `ORDER BY t1.tag_ord ASC` (`populate/apply.ts:294-307` via `getJoinTableOrderBy`, `apply.ts:20-31`) and rows are re-grouped per parent in encounter order (`apply.ts:309-313`) — distinct `tag_ord` values per parent make the order deterministic. Duplicate tags per article are forbidden (they would tie). |
| dz | one component, type = `i mod 2`, payload = `g(i)`, `order = 1` | exercises `_cmps` + the per-component-type query (`apply.ts:528-534` join-table read ordered by `(entity_id, order)`, then `:576-582` one query per distinct type) |
| `created_at`/`updated_at`/`published_at` | `epoch0 + i seconds`; draft row identical to published | Strapi timestamps are JS `new Date()` bound as parameters, never SQL `NOW()` (§1), so seeded values survive round-trips verbatim |

**Key-space partitioning (load-bearing).** Any write mutates `updated_at` and mints a new published-row PK (`entries.ts:152-171`: `omit('id')`, `assoc('publishedAt', new Date())`), which destroys the oracle for that document. The keyspace is therefore split at seed time:

- `i ∈ [0, N_imm)` — **immutable range**. Oracle checks, the differential gate, and all read workloads draw from here. Never written.
- `i ∈ [N_imm, N)` — **mutable range**, ~5% of the mass. Staleness probes (§4.3) and W4's writes draw only from here. Oracle assertions on this range are limited to structural invariants (row pair exists, `document_id` unchanged).

**Oracle-checkable operations** (content API, `core-api/controller/collection-type.ts:24-45` → `service/collection-type.ts:24-63`; the service injects `status: 'published'` by default, `core-service.ts:2-7`):

- `GET /api/articles/{documentId}` (+ `?populate=...`) — full body reconstruction, byte level.
- `GET /api/articles?sort=id&pagination[page]=p&pagination[pageSize]=s` — page contents and `meta.pagination.total` (count is on by default: `api.rest.withCount` defaults `true`, `core-api/service/pagination.ts:45-70`).
- `GET /api/articles?filters[views][$gte]=X&sort=id&...` — membership and totals from `f(i)`.
- `?status=draft` vs default published — both rows seeded, both derivable.

**Ordering rule.** `ensurePaginationOrderStability` appends `id ASC` to *paginated* selects only (`query-builder.ts:486-516`), and skips when `limit === -1` (`:495-497`) or when deep sort applies (`:499-501`). The generator therefore (a) always sends an explicit `sort=id`, and (b) never sends `pagination[limit]=-1`, so the oracle never depends on that code path.

**Seed validation (METHODOLOGY §2.1).** After each rung: sample K=10,000 uniform `i` from the immutable range, issue `findOne` plus a stratified set of `findMany`/filter pages **against path A only**, byte-compare to the oracle. This validates the seeder before PgCache exists in the picture. Failure here is a seeder bug, not a cache bug.

### 4.2 The differential gate

Same request stream to A and B, **byte-compared first**. Pinned request rules:

- Explicit `sort=id` on every list request; no `pagination[limit]=-1`.
- **Never send `strapi-encode-source-maps: true`** — it switches the controller into source-map encoding, which rewrites string fields (`core-api/controller/index.ts:47` → `controller/transform.ts:56-63`). **Never send `strapi-response-format: v4` either** (`controller/index.ts:46` → `transform.ts:56-57`): it routes the response through `transformEntry` and produces a different serialization. The v4 header would not break the A/B diff (both paths would receive it) but it **does** break the §4.1 oracle, which reconstructs v5-format bytes.
- Auth via users-permissions JWT only, never API tokens: the API-token path runs `UPDATE strapi_api_tokens SET last_used_at=…` on the read path (`api-token-utils.ts:34-55`), which makes A and B differ in write traffic.
- Requests drawn from the **immutable key range only**; no write workload may run concurrently with the gate. (Two requests to A and B are separated in time; a concurrent write would legitimately change the answer and produce a false diff.)
- **No conditional-request headers, but not because of ETags.** There is **no ETag middleware anywhere** in `packages/core/core/src` or `packages/core/admin/server/src` (grep for `etag` returns zero hits outside tests) and `koa-etag` is not a dependency (`packages/core/core/package.json:95-103` lists `koa`, `koa-body`, `koa-compose`, `koa-compress`, `koa-favicon`, `koa-helmet`, `koa-ip`, `koa-session`, `koa-static` — nothing else). API responses carry no validators, so `If-None-Match` would simply be ignored; the rule is retained only as generator hygiene, and §3's statement is the correct one.

**Normalization policy (per METHODOLOGY §7.4).** Compare **status code + raw response bytes**, with **no normalization**, on the first gate run. Sorted-key re-serialization is *not* applied by default: a row-reconstructing proxy is exactly the kind of component that could reorder object keys, and re-serializing would suppress the divergence class we most need to see. If byte-compare produces diffs, each one is triaged; only after a specific drift is demonstrated benign (key order identical in content, differing in sequence, reproducible across ≥3 runs, traced to a serialization path and not to data) may a normalization be enabled — and then it is recorded in the campaign's defect log with the evidence that justified it. Any normalization in force must be printed in the run's `summary.json`.

**Sample:** ≥100k request pairs per gate run, drawn from the same mix as the workload about to be measured (W1 Zipf + W2 uniform + W3 populated-list shapes). Target: 100.000000% agreement or abort.

### 4.3 Staleness probe

Two probes, because Strapi has two very different write shapes. **Both probe writes go through path A — directly to the origin Postgres, bypassing PgCache entirely.** That is deliberate: writing through path A and reading through path B measures *pure CDC propagation lag* (origin commit → PgCache invalidation → fresh read), with no write-through/self-invalidation shortcut inside the proxy. A write through path B would measure something else (the proxy invalidating what it just wrote) and would understate lag. All probe documents come from the **mutable key range**.

**Probe 1 — publish (delete + re-insert; the hard CDC case).**

- *Write (path A):* `PUT /api/articles/{documentId}` with `{"data":{"title":"probe-<nonce>"}}`. No `status` param is sent, and the core service injects `status: 'published'` (`core-api/service/core-service.ts:2-7`), so `update` runs the **publish** branch (`repository.ts:544-548`). Publish loads the drafts and the old published versions (`repository.ts:577-594`), then **deletes every old published row** (`repository.ts:621`: `await async.map(oldPublishedVersions, (entry) => entries.delete(entry.id))`) and **creates a new published row with a new PK** (`repository.ts:628-630` → `entries.ts:152-171`, which does `omit('id')` and `assoc('publishedAt', new Date())` before `createEntry`). The old row's `_lnk`/`_cmps` rows go with it — join-table FKs are `onDelete: 'CASCADE'` (`metadata/relations.ts:505-521`; `_cmps` at `transform-content-types-to-models.ts:260-268`) — and new join rows are inserted for the new PK. This is DELETE+INSERT row-identity churn across four tables, the worst case for CDC.
- *Read:* poll `GET /api/articles/{documentId}` through **path B** at 2 ms intervals.
- *Converged:* first response with `data.title == "probe-<nonce>"` **and** a `data.id` different from the pre-write published PK (both conditions — matching only on `title` would miss a stale row-identity mapping).
- *Report:* `t(first fresh read) − t(write HTTP 200)`, distribution over ≥1,000 probe cycles on distinct documents.

**Probe 2 — draft update (in-place UPDATE; the easy case).**

- *Write (path A):* `PUT /api/articles/{documentId}?status=draft` with a nonce title. With `status: 'draft'` the publish branch at `repository.ts:544` is not taken; the draft row is updated in place (`repository.ts:515-523`).
- *Read:* poll `GET /api/articles/{documentId}?status=draft` through path B; criterion on `title`.
- The Probe-1 − Probe-2 gap isolates delete/re-insert identity churn from plain update propagation.

**Conditions.** Each probe runs (a) against a quiesced system and (b) under W4 background load — lag under invalidation churn is the number that matters. Under (b) the background W4 writes travel through whichever path the app under test uses, while the probe's own write still goes through path A; state this in the report. Because path C does not exist (§3), the probe yields **CDC lag with no TTL column** — every report must say so.

### 4.4 Workload sketch

All workloads are open-loop, target-rate driven (METHODOLOGY §3). Auth = users-permissions JWTs **pre-signed offline** by the generator with the pinned `jwtSecret` — no login traffic, no bcrypt in the measurement window, no API-token `last_used_at` writes. Anonymous variants need public-role `find`/`findOne` permissions on `article` (§6). Unless a row says otherwise, requests send **no `locale` param**, so L1 fires.

| ID | Name | Concrete operations | Key distribution | Statements/request | What it isolates |
|---|---|---|---|---|---|
| **W1** | hot-read | authed `GET /api/articles/{documentId}` | Zipf s≈1.0 over the immutable documentId range; locality verified empirically per D-01 and locked with a regression test | **7** = A1+A2+A3+A4a+A4b (5, out-of-trx) + L1 (in-trx) + findOne body (in-trx) | best case for PgCache: 5 near-static auth SELECTs cacheable regardless of R1b |
| **W1L** | locale-pinned control | same, `?locale=en` | same Zipf | **6** = 5 out-of-trx + body | L1 is skipped when `params.locale` is set (`internationalization.ts:24-26`). W1 − W1L is the marginal cost of one in-transaction `core_store` SELECT — a cheap, direct read on R1b |
| **W1a** | anonymous | same, no `Authorization` header | same Zipf | **3** = A5 (1, out-of-trx) + L1 + body | A5 *replaces* the 5-statement authed prefix, it does not merely drop it. Cacheable-by-default share falls 71% → 33%; W1 vs W1a quantifies how much of B's win is the auth-path cache |
| **W2** | no-locality control | authed `GET /api/articles/{documentId}` | uniform over the full immutable keyspace | 7 | cache worst case; at S3 also the origin's worst case (see §6 memory ceiling) |
| **W2q** | search control | authed `GET /api/articles?_q=<random 8-char token>&sort=id` | random tokens, ~0 hits | 9 (5 + L1×2 + R8 + count) | `_q` compiles to `ILIKE` over every string column (§1 R8) — seq-scan-shaped, unamortizable across keys; upper bound on "cache cannot help" |
| **W3** | max amplification | authed `GET /api/articles?populate[0]=author&populate[1]=tags&populate[2]=dz&sort=id&pagination[page]={zipf}&pagination[pageSize]=25` | Zipf over the first 1,000 pages | **14** + 2 trx pairs = auth 5 + L1×2 (`repository.ts:326` and `:560`) + R1 + R3 + author 1 (`apply.ts:105-187`) + tags 1 (`apply.ts:294-307`) + dz 1+K, K=2 (`apply.ts:528-534,576-582`) | Strapi's highest queries-per-request lever. Out-of-trx cacheable share bottoms out at 5/14 = 36% |
| **W4** | mixed 90/10 | 90% W1 reads (immutable range); 10% writes = `PUT /api/articles/{documentId}` on the **mutable range**, uniform | reads Zipf, writes uniform | reads 7; writes ≥15 (auth 5 + the publish path of `repository.ts:544-548,577-594,621,628-630`) — exact write count to be measured, not asserted | CDC invalidation churn under multi-table DELETE+INSERT. Staleness Probe 1 re-runs under this load |

**Run protocol (METHODOLOGY §3-§4), binding for every rung:**

1. **Adaptive warm-up to the hit-rate knee** (D-08) — never a fixed duration. The generator samples PgCache's hit rate and declares warm when the slope over a sliding window falls below a pinned epsilon; the warm-up decision and its curve go into `summary.json`. A cold-cache measurement published as PgCache is fraud.
2. **Restart PgCache before *each* path-B repetition** — not only after a re-seed (D-15). Every repetition of every workload on path B starts cold; the restart timestamp is recorded per run.
3. **Drop ratio > 10% voids percentiles** (D-04): report "did not sustain the rate", never a p99. The campaign runner aborts the rung.
4. **Amplification before latency** (METHODOLOGY §4). Strapi exposes no datastore-query counter, so record `pg_stat_statements` `calls`-delta ÷ request count per run per path (and, for path B, the origin-side delta *plus* PgCache's own counters). If per-request amplification differs between A and B, the paths did not do the same work and the latency comparison is void.
5. **Counters that go backwards** = restart mid-window → discard (D-06). Aggregation keys: rung, workload, target rate, key distribution, network mode; median over ≥3 repetitions (D-03/D-07). W2q is never mixed into aggregates with other workloads.

## 5. Comparability knobs

| Knob | Default | Pinned value | Why it matters |
|---|---|---|---|
| `DATABASE_CLIENT` | `'sqlite'` (`packages/cli/create-strapi-app/templates/vanilla/config/database.ts:6`) | `postgres` | Default app never touches Postgres at all. |
| Driver protocol | Knex `3.0.1` (`packages/core/database/package.json:54`) + `pg` `8.20.0` (`yarn.lock`, `"pg@npm:8.20.0"`). Knex parameterizes every query; node-postgres sends parameterized queries via the **extended protocol** (unnamed Parse/Bind/Execute). There is **no simple-protocol switch** in node-postgres, and no client-side statement cache (knex never names statements). | Same driver everywhere; verify PgCache parses extended protocol **before** any other work | The openFGA-pgx equivalent, inverted: if PgCache is simple-protocol-only, ~100% of Strapi's content queries are invisible and the subject is disqualified. If it handles extended protocol, visibility is actually *better* than openFGA (stable query shapes, literals as parameters). |
| Pool `min`/`max` | Template: `DATABASE_POOL_MIN=2`, `DATABASE_POOL_MAX=10` (`templates/vanilla/config/database.ts:32,53`); `examples/getstarted/config/database.js:13-22` sets **no pool** → Knex/tarn defaults (min 2, max 10) | `min=10, max=10`, identical A/B | Pool ramp-up during warm-up shifts latency; ceiling caps concurrency identically against origin vs proxy. |
| `acquireConnectionTimeout` | 60000 ms (`templates/vanilla/config/database.ts:67`) | 60000, explicit | Silent queueing above pool ceiling turns saturation into latency, not errors. |
| `NODE_ENV` / run mode | `'development'` (`packages/core/core/src/configuration/index.ts:19`) | `production`, run `strapi start` | Dev mode enables autoReload/file-watching and different admin serving; `strapi develop` also opens extra ports (§7). |
| `HOST` | `process.env.HOST \|\| os.hostname()` (`configuration/index.ts:22`) | `0.0.0.0` | Binding to `os.hostname()` under hostNetwork resolves unpredictably per node — deploy-time trap. |
| `PORT` | 1337 (`configuration/index.ts:23`) | unique per path (see §7) | Bind collision under hostNetwork; Strapi exits on EADDRINUSE (`services/server/http-server.ts:37-39`). |
| Telemetry | **On** whenever app `package.json` has a `strapi.uuid` (`services/metrics/index.ts:24-27`); installs a per-request middleware that fires up to 1000 outbound events/day to `https://analytics.strapi.io` (`services/metrics/index.ts:47`, `middleware.ts:19-60`, `sender.ts:108`) | `STRAPI_TELEMETRY_DISABLED=true` | Outbound HTTP piggybacked on request handling = noise in every path; the openFGA "non-empty default experimentals" equivalent. |
| Cron | `server.cron.enabled: false` for *user* tasks (`configuration/index.ts:25`), but `cron.start()` runs unconditionally (`providers/cron.ts:14`) — internal jobs still fire: telemetry ping daily 12:00 (`services/metrics/index.ts:40-45`), admin project-info daily 00:00 with 2 DB count queries (`packages/core/admin/server/src/services/metrics.ts:31-38`) | telemetry disabled kills the ping; avoid 00:00 UTC measurement windows | Background DB queries mid-window differ per path start time. |
| Log level | `'http'` → one console line per request (`packages/core/core/src/Strapi.ts:272-276`, `middlewares/logger.ts:9`) | `server.logger.config.level: 'warn'`, identical A/B | Console I/O throttles Node at high RPS; must not differ between paths. |
| Middleware stack | Default list `logger…public` (`services/server/register-middlewares.ts:7-18`); `compression`, `responseTime`, `ip` exist but are **not** enabled (`middlewares/index.ts:17-32`) | identical `config/middlewares.ts` in all paths; leave `strapi::compression` off | Compression trades CPU for bytes and would skew CPU-bound Node differently per path. |
| REST pagination | `api.rest.defaultLimit` 25, `maxLimit` null (`core-api/service/pagination.ts:35-36`) | explicit `pageSize` in the workload | Response size = work per request; defaults must not drift. |
| Workload auth mode | public = +1 DB query/req; authenticated = +3 DB queries/req (§3 evidence) | pick one per workload, document it, same tokens/paths everywhere | Changes both amplification and the cacheable fraction. Auth-route rate limit (10/min, `users-permissions/server/src/config.js:31-34`) means tokens must be minted once (or signed offline per §4.4), never per request. |
| GraphQL plugin | `shadowCRUD: true`, `endpoint: '/graphql'`, `subscriptions: false`, `maxLimit: -1`, `apolloServer: {}` (`plugins/graphql/server/src/config/default-config.ts:1-8`); Apollo `cache: 'bounded'` (`bootstrap.ts:181`) | exclude the plugin from the campaign (REST only), or pin all of these | Different query planner + no maxLimit = unbounded result sets. |
| `server.http.serverOptions` | `{}` (`services/server/http-server.ts:23`) → Node defaults (keepAliveTimeout 5 s, headersTimeout 60 s, requestTimeout 300 s) | leave default, identical everywhere | Keep-alive churn vs the generator differs per path if touched. |
| `server.transfer.remote.enabled` | `true` (`configuration/index.ts:27-31`) — mounts data-transfer websocket on the main port (`admin/server/src/middlewares/data-transfer.ts:16`, `data-transfer/src/strapi/remote/constants.ts:1`) | `false` | Surface hygiene; nothing should be able to push/pull data mid-window. |
| `server.mcp.enabled` | `false` (`services/mcp/internal/McpConfiguration.ts:20`) | leave `false` | New in 5.x; mounts `/mcp` on the main port if enabled. |
| `DATABASE_SCHEMA` | `'public'` (`templates/vanilla/config/database.ts:51`); when set, dialect issues `SET search_path` per pooled connection (`dialects/postgresql/index.ts:42-47`) | unset (or identical) in all paths | Per-connection setup statements pass through PgCache; the dialect also installs type parsers per connection (`postgresql/index.ts:19-36`) — same all paths, but `afterCreate` traffic exists. |
| Sessions | koa-session, cookie-based, no server store (`middlewares/session.ts:5-16`) | default | No DB impact — recorded to confirm it is *not* a hidden DB knob. |
| `plugin::users-permissions.jwtSecret` (`JWT_SECRET`) | `env('JWT_SECRET')`, i.e. **unset** (`users-permissions/server/src/config.js:13`). Scaffolding writes a **freshly random** value per app (`cli/create-strapi-app/src/utils/dot-env.ts:6,40`). If unset at boot: throws under `NODE_ENV!=='development'` (`server/src/bootstrap/index.js:147-153`), else generates `crypto.randomBytes(16)` and appends it to `.env` (`bootstrap/index.js:155-161`) | **one identical secret across A/B/C**, injected by the harness, never scaffolded per path | §4.4 pre-signs users-permissions JWTs offline with this exact key; verification is `jwt.verify(token, config.get('plugin::users-permissions.jwtSecret'))` (`server/src/services/jwt.js:93`). A per-path secret makes path B return **401 on every request** — near-zero latency, zero DB queries, and a "cache" that looks impossibly fast. The highest-severity silent divergence in the table. |
| `plugin::users-permissions.jwtManagement` | `'legacy-support'` (`users-permissions/server/src/config.js:22`; read at `services/jwt.js:32,67`) | `legacy-support`, explicit | Under `'refresh'`, `verify()` stops being a pure HMAC check and becomes SessionManager validation **plus a `up_users` findOne per request** (`services/jwt.js:69-82`), and offline-signed tokens are rejected outright (`type !== 'access'`). Silently changes both amplification and the auth-prefix shape assumed by W1/W1a. |
| UP JWT algorithm / lifetime | `expiresIn: '30d'` (`users-permissions/server/src/config.js:15`); verify accepts `[jwtConfig?.algorithm \|\| 'HS256']` (`services/jwt.js:89`) | `HS256`, `expiresIn` ≥ campaign duration, identical A/B/C | The generator must sign with the same alg the server whitelists; an alg mismatch is another 401-shaped "cache defect". A token expiring mid-window turns a run into 401s partway through. |
| `admin.auth.secret` (`ADMIN_JWT_SECRET`) | `env('ADMIN_JWT_SECRET')` (`templates/vanilla/config/admin.ts:5`), random per scaffold (`dot-env.ts:6,39`). Missing → boot throws whenever `admin.serveAdminPanel` is true, and that defaults to **true** (`admin/server/src/services/token.ts:44-51`; default at `core/src/configuration/index.ts:77`) | identical across paths | Boot-blocking, so it will not fail silently — but it is also the **fallback UP secret** for SessionManager (`users-permissions/server/src/bootstrap/index.js:134`: `upConfig.jwtSecret \|\| config.get('admin.auth.secret')`), so a divergence here can leak into content-API auth. Admin-panel logins also break, blocking manual sanity checks. |
| `admin.apiToken.salt` (`API_TOKEN_SALT`) | `env('API_TOKEN_SALT')` (`templates/vanilla/config/admin.ts:8`), random per scaffold (`dot-env.ts:6,37`); missing → boot throws unless the deprecated env is set (`admin/server/src/services/api-token.ts:889-905`) | identical across paths | Tokens are stored as `HMAC-SHA512(salt, accessKey)` (`api-token.ts:682`) and looked up by that hash (`api-token.ts:688`). A per-path salt makes any seeded API token unresolvable in that path — 401. Benchmarks use UP JWTs, but any smoke/oracle script using an API token would fail on exactly one path. |
| `admin.transfer.token.salt` (`TRANSFER_TOKEN_SALT`) | `env('TRANSFER_TOKEN_SALT')` (`templates/vanilla/config/admin.ts:12`), random per scaffold (`dot-env.ts:6,38`); read at `admin/server/src/services/transfer/utils.ts:8`, hashing at `services/transfer/token.ts:302`; missing only disables transfer with a warning (`transfer/token.ts:317-318`) | fixed dummy in all paths (transfer itself pinned off above) | Lowest severity of the secrets, but leaving it unset changes the boot log and the mounted transfer surface between paths if one path happens to have it. Pin for symmetry. |
| `server.app.keys` (`APP_KEYS`) | `env.array('APP_KEYS')` (`templates/vanilla/config/server.ts:8`); scaffold emits **4 comma-separated random keys** (`dot-env.ts:36`). Read at `core/src/services/server/index.ts:21` → `app.keys` (`services/server/koa.ts:48-50`). `strapi::session` is in the default middleware list (`register-middlewares.ts:13`) and **throws at boot** if keys are missing/empty (`middlewares/session.ts:22-27`) | one identical `APP_KEYS` list in all paths | Boot-blocking, so not silent — but it is the koa cookie signing key. Different keys per path invalidate each other's signed session cookies, so a cookie signed on A is rejected on B and looks like an auth bug. |
| `admin.secrets.encryptionKey` (`ENCRYPTION_KEY`) | `env('ENCRYPTION_KEY')` (`templates/vanilla/config/admin.ts:16`), random per scaffold (`dot-env.ts:6,41`); missing only **warns** (`admin/server/src/services/encryption.ts:9-11`) | identical dummy value in all paths | The only secret that neither throws nor 401s — it just emits a differing warning line per path. Pin so boot logs are byte-comparable across paths. |
| `features.future.experimental_firstPublishedAt` | **`false`** — the only *server-side* future flag (`packages/core/types/src/core/config/features.ts:8`; `:7` also declares `unstableMediaLibrary`, consumed admin-side only at `packages/core/upload/admin/src/index.ts:36`, and `:9` is an open index signature `[futureFlagName: string]: boolean | undefined`); resolved as `strapi.config.get('features.future.experimental_firstPublishedAt', false)` (`packages/core/utils/src/content-types.ts:257-258`) and via the features service (`core/src/services/features.ts:16-19`). No `config/features.ts` ships in the scaffold, so the key is absent → `undefined` → false | `features.future` pinned as an **explicit object** in `config/features.ts` in every path, with `experimental_firstPublishedAt: false` — not merely absent, since the interface accepts arbitrary flag names | The openFGA "non-empty experimentals" analogue, and it moves *two* things at once. (a) Schema: when on, `firstPublishedAt` is injected as a real attribute into every D&P content type (`core/src/domain/content-type/index.ts:105-111`) → a `first_published_at` column that **invalidates §6's COPY column list** and triggers a data migration (`core/src/migrations/index.ts:9`, `migrations/first-published-at.ts:26-63`). (b) Write shape: publish gains an extra draft `UPDATE` inside the same transaction (`document-service/repository.ts:623-625` → `first-published-at.ts:13-30`), changing W4's write amplification and CDC invalidation footprint. Pin, do not merely leave default. |
| `api.rest.withCount` | **`true`** — `return Boolean(strapi.config.get('api.rest.withCount', true))` (`core-api/service/pagination.ts:70`). Also **overridable per request** via `pagination[withCount]` (`pagination.ts:45-53`) | config `true` **and** the generator never sends `pagination[withCount]` | Same `api.rest.*` namespace as `defaultLimit`/`maxLimit` which are already pinned — omitting it was arbitrary. `shouldCount()` gates a second `documents().count()` call (`collection-type.ts:37-38`), i.e. one extra SELECT **and** its own BEGIN/COMMIT pair per list request: ~15% of W3's statements and 1 of its 2 transaction pairs. Drift here changes the amplification denominator §4.4 divides by. |
| Origin `shared_buffers` | Postgres server default 128 MB (not set anywhere in `_sources/strapi`; `docker-compose.dev.yml:4-14` passes no `-c` flags) | **1 GB, identical on the A and B origins** — lab precedent `openFGA/docker-compose.yml:77`, `openFGA/infra/aks/chart/values.yaml:60` | §6's scale ladder is *defined* by this number (S1 fits, S2 at the knee, S3 exceeds). At the 128 MB default even S1 spills and the ladder measures buffer-pool starvation instead of cache economics; if it differs between A and B, path B's "win" is just extra RAM. |
| Origin `effective_cache_size` | Postgres server default 4 GB | **2 GB, identical A/B** (`openFGA/docker-compose.yml:78`) | Planner-only, but it decides index-scan vs seq-scan at exactly the S2/S3 boundary where W2 and W2q live. A plan flip on one path is indistinguishable from a cache effect in the latency series. |
| Origin container memory ceiling / CPU | unset in `_sources/strapi` (`docker-compose.dev.yml` sets neither `mem_limit` nor `cpus`) | **real ceiling** **`mem_limit: 4g`, `cpus: 3.0`** (`openFGA/docker-compose.yml:67-68`), identical A/B — §6's S1/S2/S3 boundaries are computed from this exact 4 GiB number; **CPU requests, never limits**, on k8s | METHODOLOGY §5 (`docs/METHODOLOGY.md:88-90`): a ceiling above what the host can give means the process grows until the machine dies mid-window (D-06). It also makes S3 meaningful — with unbounded memory the page cache absorbs the whole working set and S3 collapses into S2. `docs/METHODOLOGY.md:85-87`: CPU *limits* produce CFS-throttled p99 spikes indistinguishable from cache behaviour. |
| Origin `pg_stat_statements` | not loaded by default | `shared_preload_libraries=pg_stat_statements`, `track=all`, `max=10000` — identical on both origins (`openFGA/docker-compose.yml:74-76`) | §4.4's amplification accounting *is* the calls-delta from this extension, and the "void the comparison if paths differ" rule is unenforceable without it. `track`/`max` must match or the deltas are not comparable (truncation at different thresholds). |
| Origin `max_connections` | Postgres default 100 | 400, identical (`openFGA/docker-compose.yml:81`, `openFGA/infra/aks/chart/values.yaml:61`) | Pool ceiling is pinned at 10/path, but PgCache multiplexes its own upstream pool on top. Headroom must be identical so neither path hits connection refusal as a latency artifact. |
| Origin `wal_level` | Postgres default **`replica`** | **`logical`**, identical on both origins (`openFGA/docker-compose.yml:71`) | **Prerequisite for path B existing at all.** §4.3's probe is defined as origin commit → PgCache invalidation, i.e. logical decoding; the reference deployment has PgCache consume a publication and slot (`openFGA/docker-compose.yml:162-163`: `PUBLICATION: "pgcache_fga_pub"`, `SLOT: "pgcache_fga_slot"`). At the default, PgCache cannot create its slot: path B either fails to start or never invalidates, and §4.3 measures unbounded lag with no diagnosis path. Also a comparability knob — logical WAL changes write volume, so **both** origins carry it or W4's write latency is not comparable. |
| Origin `max_wal_senders` / `max_replication_slots` | 10 / 10 in modern Postgres, but not guaranteed | **10 / 10**, identical both origins (`openFGA/docker-compose.yml:72-73`) | Pinned with `wal_level` as one unit. Pre-flight assertion before any path-B run: the slot exists and `pg_replication_slots.active = true`. |
| Origin `random_page_cost` | 4.0 | **1.1**, identical both origins (`openFGA/docker-compose.yml:82`) | §5 already pins `effective_cache_size` because "a plan flip on one path is indistinguishable from a cache effect" — the same argument applies here and was inconsistent to omit. W2q is an ILIKE seq scan by design and W3 sorts populated pages; at the default the S2→S3 transition can flip plans on one path only. |
| Origin `work_mem` | 4 MB | **32 MB**, identical both origins (`openFGA/docker-compose.yml:79`) | Same plan-stability argument: at 4 MB, W3's populated-page sorts spill to disk at S2+ and may spill on one path and not the other. |
| Origin `track_io_timing` | `off` | **`on`**, both origins (`openFGA/docker-compose.yml:83`) | §6's S3 acceptance test is I/O-based (`pg_statio_user_tables` + read IOPS); without this the test cannot attribute time to I/O. |
| **PgCache `ALLOWED_TABLES`** | lab default is subject-specific (`openFGA/docker-compose.yml:155`) | **must be resolved before the first path-B run, then frozen** — proposed: content tables + `up_users`/`up_roles`/`up_permissions`/`up_permissions_role_lnk`/`strapi_core_store_settings`; **excluding** `strapi_sessions` and `strapi_api_tokens` (§1 exclusion plan, R5) | Not a deferrable decision: §1's cacheable-fraction table changes meaning depending on it, and Open Question 15 admits the value swings the unconditionally cacheable surface between 5 statements and **0**. Two path-B runs with different values are not comparable. |
| **PgCache `DISK_LIMIT`** | unset | **leave unset** — deliberately | `openFGA/docker-compose.yml:158-161` records the footgun: PgCache compares this value against **total filesystem usage**, not its own cache, so a "reasonable" limit on a busy host makes the cache judge itself permanently under disk pressure and **refuse to register every query — silently, 100% miss presented as a cache result**. |
| **PgCache `CACHE_POLICY` / `ADMISSION_THRESHOLD` / `NUM_WORKERS`** | `clock` / `1` / build default (`openFGA/docker-compose.yml:156-157`) | pinned identically across every path-B repetition and every rung | Changing eviction policy or admission between runs makes rungs incomparable; these belong in `summary.json` for every run. |
| **PgCache `PGCACHE_TELEMETRY`** | build default | **`off`** (`openFGA/docker-compose.yml:164`) | Same class as Strapi's telemetry: outbound traffic on the measurement path. |
| **PgCache `mem_limit` / `cpus`** | unset | **real ceiling**, sized before S3 and recorded | R12: openFGA needed 4.8 GiB at 85K tuples (METHODOLOGY §6); at S3 the mass is ~1M documents across 6+ tables. If PgCache cannot hold a useful working set inside its ceiling, S3's path-B numbers measure proxy eviction, not caching. METHODOLOGY §5: a ceiling above host RAM is not a ceiling (D-06). |
| **App container `mem_limit` / `cpus`** | unset | real ceiling, identical A/B/C | METHODOLOGY §5 applies to every container, not just the origin. Single-process Node with a 1M-document populate workload is the exact D-06 shape; a CPU *limit* would also produce CFS-throttled p99 spikes indistinguishable from cache behaviour, so use requests. |
| `DATABASE_URL` | unset by default, but the template sets `connectionString: env('DATABASE_URL')` **alongside** `host`/`port` (`templates/vanilla/config/database.ts:37-39`) | **unset in every path**, and assert the effective endpoint at startup | `DATABASE_URL` wins in `pg`, so a stray value in path B's environment silently routes path B **straight to the origin, bypassing PgCache** — producing a perfectly plausible "PgCache gives no benefit" result. Assert with `SELECT inet_server_port()` from the app's own pool, or require PgCache connection-count metrics non-zero, before any run. |

## 6. Seeding at scale

**Direct SQL — mandatory.** There is no bulk create endpoint in the core API (`core-api/controller/collection-type.ts:50-73` is one document per call), and each create is a multi-statement transaction that also runs the publish clone. Add the fixed 5-statement auth prefix per HTTP call (`users-permissions.js:24,31,44-46`) and 1M documents becomes ~1M HTTP calls at ≥10 statements each. `COPY` it is.

### Sequence

**1. Boot Strapi once against an empty origin**, with the benchmark app's content-type and component schemas checked into the *project* (never into `_sources/strapi`). Schema sync + internal migrations create the content tables, join tables, `strapi_core_store_settings`, `i18n_locale` (plugin content types get `collectionName = pluginName_singularName`, `loaders/plugins/index.ts:162-163`), the u&p tables (`up_users`, `up_roles`, `up_permissions` — `content-types/user/index.js:6`, `role/index.js:4`, `permission/index.js:4`) and the admin tables.

Keep collection and attribute names short (`article`, `tag`, `author`, `dz`) so the identifier shortener never has to hash. The budget constant is **`IDENTIFIER_MAX_LENGTH = 55`** (`packages/core/database/src/utils/identifiers/index.ts:21`; `:451` is only where the singleton `Identifiers` is constructed with it), and it applies to the **entire generated identifier** — table name + column name + suffix, and separately to derived index names (`getNameFromTokens`; index helpers at `:186-215`). It is not a limit on the attribute name. If the budget is exceeded, compressible tokens are truncated and a 5-char hash is appended (`getShortenedName`, `identifiers/index.ts:225-262`), silently changing the physical names.

**2. Capture the physical schema — do not assert it.** Immediately after the first boot, dump `information_schema.tables`, `information_schema.columns` and `pg_indexes` for the app schema, and **generate the seeder's table and column lists from that dump**. Assertions the guard must make before any COPY runs:

- every expected table exists, and **no** table/column/index name carries a 5-char hash suffix (i.e. nothing was shortened);
- `articles` has **no `first_published_at` column** — that attribute only appears when `features.future.experimental_firstPublishedAt` is set (`packages/core/utils/src/content-types.ts:257-259` → `domain/content-type/index.ts:104-118`); if it is present, the flag leaked in and the run is invalid;
- the composite index on `(document_id, locale, published_at)` exists (`transform-content-types-to-models.ts:324-340`, named via `getIndexName([collectionName, 'documents'])`). This index carries W1/W2's entire `findOne` path — without it, path A degrades to a seq scan at S2+ and the comparison measures the missing index, not the cache.

Expected names, **derived** from the §4.1 attribute schema and `relations.ts`, to be confirmed against the dump (not hardcoded):

| Object | Derivation | Expected |
|---|---|---|
| content table | `apis.ts:152` with explicit `collectionName` | `articles` |
| author join table | `getJoinTableName('articles','author')`, suffix `links`→`lnk` (`identifiers/index.ts:116-121`, replacement map `:38-50`) | `articles_author_lnk (id, article_id, author_id)` — **no order columns** (`relations.ts:66-68`, `manyToOne` + unidirectional) |
| tags join table | same, plus `isAnyToMany` order column named `getOrderColumnName('tag')` | `articles_tags_lnk (id, article_id, tag_id, tag_ord)` — `tag_ord` **float** (`relations.ts:543-548`) |
| dz/component join table | `getComponentJoinTableName` / `getDzJoinTableName` (`transform-content-types-to-models.ts:20-32`) | `articles_cmps (id, entity_id, cmp_id, component_type, field, order)`, `order` **float** (`:233-239`) |
| u&p link tables | `permission.role` and `user.role` are `manyToOne` **with `inversedBy`** (`content-types/permission/index.js`, `user/index.js:64-70`) → `isBidirectional && isManyToAny` ⇒ an **inverse** order column exists | `up_permissions_role_lnk (id, permission_id, role_id, permission_ord)`; `up_users_role_lnk (id, user_id, role_id, user_ord)` |

**Order columns are `float`, not integer, and nothing enforces density.** `relations.ts:543-548` (and `:561-566` for the inverse) and `transform-content-types-to-models.ts:233-239` declare `type: 'float', defaultTo: null`. Strapi's own writers happen to assign `idx + 1` (`entity-manager/index.ts:770`, and `:622,:659,:956,:997,:1006,:1138`), so **"dense integers from 1" is a seeding convention chosen to match what the app would have written** — it is not a schema guarantee. The §4.1 oracle depends on populate ordering (`ORDER BY tag_ord`, `ORDER BY entity_id, order`), so the seeder must (a) write dense distinct values per parent and (b) assert post-load that no `(article_id, tag_ord)` pair repeats and no `tag_ord` is NULL.

**3. Create the u&p role/permission fixtures through the app, once.** Rather than guess action strings, enable `find`/`findOne` on `article` for the public and authenticated roles through the running app on the S0 rung, then dump the resulting `up_roles`/`up_permissions`/`up_permissions_role_lnk` rows and replay them verbatim as COPY fixtures at every later rung. This removes an entire class of "the permission row is subtly wrong and every request 403s" failures.

**4. Stop the app (or hold all traffic), run the seeder.**

### COPY inventory

| Table | Rows | Notes |
|---|---|---|
| `articles` | 2 per document | draft row `published_at IS NULL`, published row `published_at` set; identical `document_id`, `locale='en'`; `created_by_id`/`updated_by_id` NULL (avoids `admin_users` FKs — these are join *columns*, `domain/content-type/index.ts:121-145`) |
| `articles_author_lnk` | 2/doc | one row per article **row** (draft and published each link); columns `article_id, author_id` only |
| `articles_tags_lnk` | 6/doc | 3 tags × both rows; `tag_ord` = 1,2,3 per parent |
| `articles_cmps` | 2/doc | `entity_id, cmp_id, component_type, field='dz', order=1`; `component_type` = the component **UID** string (`document-service/components.ts:83,97`) |
| component tables (2) | 2/doc total | physical names from each component's `collectionName`; read them from the step-2 dump |
| `tags`, `authors` | 10k / 5k, fixed at every rung | D&P off → single row each, no draft twin. **Each still carries a physical `locale` column** (nullable): the i18n plugin's register hook sets a `locale` string attribute on **every** content type, localized or not (`plugins/i18n/server/src/register.ts:56-91`). The companion `localizations` attribute is `unstable_virtual: true` with a `document_id` join column (`register.ts:69-90`) and creates no join table — correctly absent from this inventory. This is exactly why step 2's dump, not §4.1, is the contract. |
| `up_users` | 10k, fixed | one precomputed bcrypt hash reused for all rows (cost pinned low); `confirmed=true`, `blocked=false` |
| `up_users_role_lnk` | 10k | all → authenticated role; `user_ord` set |
| `up_roles`, `up_permissions`, `up_permissions_role_lnk` | fixtures from step 3 | includes `permission_ord` |

**Fallback-surface fixtures — created by the step-1 boot, never COPYed, but their CONTENT must be asserted.** These three rows *are* the entire unconditionally cacheable surface plus the L1 target; if any is missing or malformed, either every request 500s or the workload silently stops exercising the path under test:

| Row | Assertion |
|---|---|
| `i18n_locale` | exactly one row with `code = 'en'` (table name per `loaders/plugins/index.ts:162-163`; schema `plugins/i18n/server/src/content-types/locale/schema.json`) |
| `strapi_core_store_settings` where `key = 'plugin_i18n_default_locale'`, `environment IS NULL`, `tag IS NULL` | exists, `value` decodes to `"en"`. Key format from `core-store.ts:82-88` (`prefix = type + '_' + name`, `key = prefix + '_' + key`) with the i18n store scoped as `{type:'plugin', name:'i18n'}` (`plugins/i18n/server/src/utils/index.ts:27-29`) and the lookup at `locales.ts:51`. **This is L1.** |
| `strapi_core_store_settings` where `key = 'plugin_users-permissions_advanced'`, `environment IS NULL`, `tag IS NULL` | exists, `value` decodes to an object with `email_confirmation: false`. Store scoped `{type:'plugin', name:'users-permissions'}` at `users-permissions.js:8-10`, read on every authenticated request at `:31`. **This is A1** — the single hottest cacheable statement in the whole subject. |

### Invariants of the seeded mass

These are properties **we impose on the seeded data**, not properties of Strapi in general:

- Every article document has exactly one draft row (`published_at IS NULL`) and one published row (`published_at` set), sharing `document_id` and `locale`. Strapi does **not** guarantee this: `clone()` mints a *new* `documentId` (`repository.ts:451`) and creates the copy with `status: 'draft'` (`repository.ts:468-472`), producing a document with a draft and **no** published row. The pair invariant is what the *publish path* maintains for documents that have been published (`repository.ts:621` deletes the old published row, `:628-630` creates the replacement) — and it holds for our mass because we seed it that way and only the mutable range is ever written.
- Join-table rows reference **both** article row ids (draft and published) — populate for `?status=draft` must return the same relations.
- `tag_ord` dense 1..3 per parent, `articles_cmps.order` = 1, no NULLs (seeding convention; see above).
- `document_id` unique across the mass: `SELECT count(*) = count(DISTINCT document_id) FROM articles` — no DB constraint enforces it.

### Post-load steps

1. `SELECT setval(pg_get_serial_sequence(t, 'id'), max(id)+1)` on **every** seeded table including all `_lnk`/`_cmps` tables (they all have `increments` PKs, `relations.ts:257-259,471-473`) — the first API write otherwise collides.
2. `ANALYZE` every seeded table.
3. Restart Strapi (it holds no data cache — §3 — but a clean process removes any doubt about the module-level `deepPopulateCache`).
4. **Restart PgCache after the re-seed (D-15), and again before every path-B repetition** (§4.4 run protocol).
5. Run the invariant assertions above, the row-count checks, and the §4.1 seed-validation sample **against path A**.

### Scale ladder

**Memory ceiling, pinned (this is what makes the ladder mean anything).** Origin Postgres runs with `shared_buffers = 1 GB` and a **hard container memory limit of 4 GiB** (METHODOLOGY §5: memory ceilings must be real). Under cgroup v2 the page cache is charged to the container, so a 4 GiB `memory.max` bounds `shared_buffers` *plus* the OS page cache the origin can use — that, not the host's total RAM, is what S3 must exceed. `effective_cache_size` is pinned to **2 GB** (§5, matching `openFGA/docker-compose.yml:78`) — under a 4 GiB container ceiling that is the defensible value. §5's knob table is the single source of truth for all three; they are the parameters that decide whether S3 does what it claims.

Sizing basis per document: heap ≈ 2.6 KB (2 rows × ~1.2 KB, ~1 KB body inline, page overhead) + join/component heap ≈ 0.9 KB + indexes ≈ 1.1 KB (PK, the `(document_id, locale, published_at)` composite, and the fk/inv-fk/unique/order indexes each `_lnk` and `_cmps` table carries, `relations.ts:493-505,552-557` and `transform-content-types-to-models.ts:241-258`) ≈ **4.6 KB/document**.

| Rung | Documents | Approx size | Purpose |
|---|---|---|---|
| S0 smoke | 1k | ~5 MB | pipeline, invariants, oracle, `documentId` round-trip (R3) |
| S1 | 50k | ~230 MB | fits `shared_buffers` comfortably; first comparable numbers |
| S2 | 250k | ~1.15 GB | at the knee — working set ≈ buffer pool, still inside the 4 GiB ceiling |
| S3 | 1M | ~4.6 GB | exceeds `shared_buffers` **and** the origin's entire 4 GiB memory ceiling → W2 forces real disk reads |
| S4 (conditional) | 4M | ~18 GB | only if the S3 verification below fails |

**Spacing deviates from the template** (S1→S2 is 5×, S2→S3 is 4×, template suggests ~10×): declared deliberately — the interesting region is the 1-5 GB band around a 1 GB `shared_buffers` and a 4 GiB ceiling, and 10× steps would jump straight over it.

**S3 must be verified, not assumed.** During W2 at S3, record `pg_statio_user_tables.heap_blks_read` / `heap_blks_hit` on the origin and the container's disk read IOPS. If the physical-read ratio is ≈ 0, the ceiling is not binding (wrong cgroup version, limit not applied, or the estimate was low) and the rung has not demonstrated anything about out-of-RAM behaviour — promote to S4 and re-run.

`up_users`, `tags` and `authors` stay fixed across rungs: hot, near-static auth metadata in front of a large content mass is the realistic shape, and it keeps the A1-A4 cacheable surface constant so its contribution can be compared across rungs.

## 7. Ports and endpoints

Strapi opens **exactly one port**: a single Node `http.Server` (`services/server/http-server.ts:25`), default `1337` (`configuration/index.ts:23`). Everything is multiplexed on it: content API `/api` (`configuration/index.ts:71`), admin UI + admin API `/admin` (same origin by default), static `/public` and favicon (`middlewares/public.ts`, `favicon.ts`), `/uploads`, GraphQL `/graphql` (plugin), `/mcp` (off by default), `/openapi.json` (off by default), and the data-transfer **websocket** `/admin/transfer/runner/{push,pull}` (upgrade on the same server — enabled by default, pin off per §5). There is **no metrics port, no gRPC, no separate admin port** — the "metrics" service is outbound-only telemetry to `analytics.strapi.io` (`services/metrics/sender.ts:108`). The dev-mode admin watcher is the only thing that opens extra ports; never run `strapi develop` in the lab. On collision Strapi exits cleanly with EADDRINUSE (`http-server.ts:37-39`) — that is the deploy-time failure mode if a port is reused.

The **database endpoint each path connects out to** is the defining difference between paths, so it is a column here, not a footnote:

| Path | HTTP (listens) | Metrics | Database endpoint (connects out) | Other |
|---|---|---|---|---|
| A `baseline` | **1337** (`HOST=0.0.0.0`) | none — no endpoint exists | **origin Postgres `5432`** — `DATABASE_HOST=<origin>`, `DATABASE_PORT=5432` (`templates/vanilla/config/database.ts:38-39`) | transfer WS shares the HTTP port; disabled per §5 |
| B `pgcache` | **2337** | none | **PgCache SQL listener `6432`** — `DATABASE_HOST=<pgcache>`, `DATABASE_PORT=6432` (lab convention: `openFGA/infra/aks/chart/values.yaml:95`, `openFGA/docker-compose.yml:168`). PgCache in turn connects to origin `5432` | same as A |
| C `appcache` | **3337** | none | **n/a — path C does not exist** (§3: strapi core ships no data cache). If the community REST-cache plugin is later adopted, C connects to origin `5432` and adds a backing store (Redis `6379`) | — |
| — platform (shared) | — | **PgCache metrics/admin `9090`** (`openFGA/infra/aks/chart/values.yaml:96`) | origin Postgres `5432`; PgCache SQL `6432` | `postgres_exporter 9187`, `cAdvisor 8080`, Prometheus `9090`\*, Grafana `3000` (`openFGA/docker-compose.yml:236,248,262,276`) |

\* Prometheus and PgCache's metrics endpoint **both default to 9090** — under `hostNetwork` on a shared node that is a guaranteed bind collision. Remap one. In the openFGA compose they only coexist because each sits behind a distinct host-port mapping (`16432:6432`/`19091:9090` vs `19090:9090`), which `hostNetwork` removes.

**hostNetwork uniqueness sweep (all distinct):** 1337, 2337, 3337, 5432, 6432, 9090 (+9187, 8080, 3000 if observability is co-located).

**Deploy trap (see §5 `DATABASE_URL`):** the template sets `connectionString: env('DATABASE_URL')` *and* `host`/`port` (`templates/vanilla/config/database.ts:37-39`). `DATABASE_URL` wins in `pg`, so a stray value routes path B straight to the origin, bypassing PgCache, and yields a plausible-looking "no benefit" result. Leave it unset everywhere and assert the effective endpoint at startup.

---

## Open questions and risks

1. **R1 — Protocol gate. RESOLVED 2026-08-03: PASS.** PgCache parses the extended query protocol — 25/25 statements counted `extended`, 0 `simple`, all served correctly. Evidence: `spike/out/result.json`. This risk is closed.
2. **R1b — In-transaction reads. RESOLVED 2026-08-03: FAIL. This is the disqualifying finding.** PgCache passed through every read issued inside `BEGIN…COMMIT` (`cache_hit +0/25`, origin `calls +25`, counted `unsupported`), while caching the *identical* statement outside a transaction (`cache_hit +25/25`, origin `calls +0`). The transaction was the only difference, so attribution is clean. A pre-warmed entry was still not served in-transaction, which rules out warming by a side channel. Ceilings this implies, per the table in §1: 83% (W1L), 71% (W1), 56% (list), 36% (W3), 33% (W1a) — over a residual surface whose expected sign §1 argues is negative.
3. **R1c — CDC prerequisites on the origin (blocking, pre-flight).** Path B cannot exist without `wal_level=logical` plus replication slots/senders on **both** origins (§5). Assert before any path-B run: the publication and slot exist and `pg_replication_slots.active = true`. At the Postgres default this fails as "PgCache never invalidates", which §4.3 would record as unbounded staleness with no diagnosis path.
4. **R1d — PgCache's own configuration is a first-class comparability surface (blocking, pre-flight).** `ALLOWED_TABLES` must be **resolved and frozen before the first path-B run**, not deferred: §1's fraction table changes meaning depending on it, and it swings the unconditionally cacheable surface between 5 statements and 0. `DISK_LIMIT` must be left unset (a "reasonable" value makes PgCache refuse every registration — silently 100% miss). Policy/admission/worker settings and PgCache's memory ceiling go in `summary.json` for every run.
5. **R2 — No path C.** Strapi core ships no native data cache; comparison is A vs B only and the staleness probe has no TTL column. Must be stated in every report (METHODOLOGY §1). Open scope decision: adopt the community `strapi-plugin-rest-cache` (v5 compatibility unverified, not in this repo) as an unofficial path C, or publish A vs B only.
6. **R3 — Synthetic documentId acceptance (largely retired by construction).** §4.1's generator forces a leading letter, making seeded ids shape-identical to cuid2 (`transform-content-types-to-models.ts:3,193`); lookup is plain string equality with no format validator on the read path. Residual check: round-trip seeded ids through `findOne` at S0.
7. **R4 — Schema drift vs strapi's identifier shortener (restated).** The seeder no longer hardcodes names: §6 step 2 derives every table/column from an `information_schema` dump taken after the first boot and fails loudly if any name carries the 5-char shortener hash (`identifiers/index.ts:225-262`, budget `IDENTIFIER_MAX_LENGTH = 55` at `:21`). The residual risk is that a Strapi upgrade changes the shortener between the dump and the run.
8. **R5 — Session/api-token noise.** `strapi_sessions` is hot-written with periodic GC DELETEs (`session-manager.ts:227-230`) and `strapi_api_tokens` is updated on read. Benchmarks use neither path (offline-signed u&p JWTs), but both tables must be on PgCache's exclusion list anyway — stray admin-UI traffic would otherwise poison them.
9. **R6 — List-response ordering.** Any differential request without an explicit `sort` is invalid by construction; enforce in the generator, not by convention. Same enforcement class: the generator must never send `pagination[withCount]` (which overrides the pinned config, `pagination.ts:45-53`), `strapi-encode-source-maps` (`controller/index.ts:47`), or `strapi-response-format: v4` (`controller/index.ts:46`).
10. **R7 — `_q` at S3 is origin-hostile.** W2q at large rungs can saturate path A with seq scans; it is a control, run at a reduced target rate, never mixed into aggregates with other workloads (D-03/D-07 aggregation keys).
11. **R8 — Writes destroy the oracle for the documents they touch.** Mitigated by §4.1's immutable/mutable key split, but the split must be enforced in the generator and regression-tested exactly like the Zipf claim (D-01). A W4 write leaking into the immutable range poisons every subsequent differential gate on that rung.
12. **R9 — The publish path's write-statement count is unmeasured.** §4.4 says "≥15" for W4 writes; the relation-sync steps (`repository.ts:596-655`) fan out per relation and were not fully traced. Must come from `pg_stat_statements` before any W4 number is published.
13. **R10 — `document_id` has no unique constraint.** The only declared index is the composite `(document_id, locale, published_at)` btree (`transform-content-types-to-models.ts:324-340`). The seeder's own uniqueness assertion is the sole guard; an HMAC truncation bug would produce duplicate documents that `findOne` resolves nondeterministically.
14. **R11 — The 4 GiB origin ceiling is unverified in this environment.** It depends on cgroup v2 page-cache accounting on the actual nodes. §6's S3 physical-read check (`pg_statio_user_tables`) is the acceptance test; S4 (4M docs) is the fallback if the ceiling turns out not to bind.
15. **R12 — PgCache's own footprint is unsized for this subject.** openFGA needed 4.8 GiB at 85K tuples (METHODOLOGY §6). At S3 the mass is ~1M documents across 6+ tables; if PgCache cannot hold a useful working set inside its own ceiling, S3's path-B numbers measure proxy eviction, not caching. Size it before S3, not during.
16. **R13 — Secret and endpoint divergence between paths (silent-failure class).** Per-path scaffolding generates six fresh random secrets (`cli/create-strapi-app/src/utils/dot-env.ts:35-41`), which makes "path B 401s on every request" the *default* outcome rather than an edge case; and a stray `DATABASE_URL` silently routes path B past PgCache to the origin (`templates/vanilla/config/database.ts:37-39`). Both are pinned in §5. Pre-flight assertion required: one pre-signed JWT returns 200 on every path, and the app's own pool reports the expected server port, before any measurement.
17. **Auth-table caching policy (decision needed).** The 1–5 permission/user queries per request hit `up_permissions`/`up_users`/`strapi_core_store_settings` on every request. Allow PgCache to cache them (staleness window on role edits — point a §4.3 probe variant here) or exclude those tables (which would zero the unconditionally cacheable surface entirely)?
18. **Single-process Node.** No cluster mode ships; the app may CPU-saturate before the DB does. Record CPU per path so a flat B result can be distinguished from "app-bound".
19. **Admin panel hygiene.** Keep the admin browser closed during measurement windows — it polls content-manager APIs and adds unmeasured DB traffic.
20. **PgCache admin port unclaimed.** The reference lab exposes only SQL (6432) and metrics (9090) — no distinct admin listener exists in `openFGA/infra/aks/chart/templates/pgcache.yaml:10-12,43-50`. If the PgCache build used here adds one, it is missing from §7's uniqueness sweep. Confirm against the image before deploy — this is the exact class of miss that bit the openFGA lab with gRPC.

## Not determined from code alone

1. **Ground truth via `pg_stat_statements`** — the monorepo was not built/run. Every §1/§2 row is code intent; the amplification table needs a runtime delta check, especially the admin content-manager metadata fan-out (`content-manager/server/src/services/document-metadata.ts` — available-status/locales queries not fully traced).
2. **PgCache behaviour for reads inside explicit transactions** — decisive for the verdict; needs a spike against PgCache itself, not Strapi.
3. **Wire protocol confirmation** — Knex/`pg` uses the extended protocol with unnamed parameterized statements; whether PgCache sees these must be checked empirically (methodology §6 confound).
4. **User-land lifecycle subscribers / plugins (GraphQL, EE audit logs)** — can inject arbitrary extra queries per read; the audit covers stock REST content API + admin only.
5. **Exact write-statement fan-out of the publish path** (R9) — the relation-sync branch `repository.ts:596-655` was not fully traced.
6. **Subject version** — `lerna.json:2` says `5.7.0`, `packages/core/database/package.json` says `5.51.1`. Not reconciled; pin the exact commit, not a version string, when the lab starts. (Spot-checks confirmed the source tree matches the 5.51.1 package layout at `23a306e`.)

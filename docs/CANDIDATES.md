# Candidates

The living shortlist. Maintained by `subject-scout` (`/scout`), read by humans
deciding where to spend a day of `/discover`.

Criteria referenced by code (C1…C8) are defined in `docs/TRIAGE-CRITERIA.md`.

**Verdicts:** `promising` · `disqualified` · `uncertain` · `completed`
**Ranking:** by cost-to-resolve, not by attractiveness. The scarce resource is
verification effort.

A verdict here is not overwritten silently. If a criterion changes and a past
verdict no longer holds, the reversal is stated with the criterion that caused
it.

---

## Recorded results

These two are not scout output — they are real outcomes from work already done,
seeded here so the scout never re-proposes them without knowing what happened.

### strapi — `disqualified`

**URL:** https://github.com/strapi/strapi
**Decided:** 2026-08-03 · **Criterion:** C2 (plain reads wrapped in
transactions)

Every Document Service read — `findMany`, `findOne`, `count` — is wrapped in a
real transaction by `wrapInTransaction`
(`packages/core/core/src/services/document-service/repository.ts:766-776` →
`common.ts:8-10` → `packages/core/database/src/index.ts:177-183`). The R1/R1b
spike then measured PgCache passing through every in-transaction read: the
identical parameterized statement cached 25/25 outside a transaction with zero
origin calls, and 0/25 inside one with 25 origin calls. A pre-warmed entry was
still not served in-transaction.

Evidence: `strapi/spike/out/result.json`, `strapi/STUDY.md`.

**Worth stressing:** Strapi's SQL is unusually cache-friendly — fully
parameterized, every table has a PK, no views, no RLS, no LATERAL, no recursive
CTEs, batched populate with no row-level N+1, and 9–14 statements per
authenticated request. It is disqualified by PgCache's current in-transaction
behaviour, not by its own design. **If PgCache gains the ability to serve reads
inside transactions, re-run `strapi/spike/run.sh` — it answers in three minutes
and Strapi becomes a first-rate subject.**

### openFGA — `completed`

**URL:** https://github.com/openfga/openfga
**Status:** the platform's reference lab. Integrated, campaigned, published.

Its chart, generator, seeder and runner under `openFGA/` are the reference
implementations every `stack-*` builder starts from, and its defect log is the
source of most rules in `docs/METHODOLOGY.md` and `docs/TRIAGE-CRITERIA.md`.
The campaign result was not favourable to PgCache; see `openFGA/report/` and
`openFGA/benchmark-docs/` for the numbers and the analysis rather than relying
on any summary here.

Not a candidate — recorded so it is not re-proposed as one.

---

## Shortlist

_Swept 2026-08-03 by `/scout`, no domain hint (broad sweep across headless CMS,
commerce, scheduling, project management, auth/identity, infra/IPAM, feature
flags, notification infra)._

Ranked by **cost-to-resolve**. The C2 method used here was first validated
against the Strapi regression case and does catch it — see "Method validation"
at the end of this section.

---

### 1. NetBox — `promising`

**URL:** https://github.com/netbox-community/netbox · Apache-2.0 · active
(pushed 2026-08-03) · clone at `_sources/netbox`

Cheapest to resolve of the shortlist: every fatal criterion came back clean and
the only thing left is measurement, not investigation.

- **C1 pass, unusually strong.** Docs: *"NetBox requires PostgreSQL 14 or later.
  Please note that MySQL and other relational databases are **not**
  supported."* ([installation
  docs](https://netboxlabs.com/docs/netbox/en/stable/installation/1-postgresql/))
  No multi-DB abstraction to fight.
- **C2 pass.** No `ATOMIC_REQUESTS` anywhere in the tree, so Django's default
  (autocommit) holds. `netbox/netbox/middleware.py:52-96` (`CoreMiddleware`) is
  the only app-wide middleware touching request lifecycle and it opens no
  transaction. All 27 `transaction.atomic` call sites are qualified with
  `router.db_for_write(...)` — e.g.
  `netbox/netbox/views/generic/object_views.py:303`,
  `netbox/netbox/views/generic/bulk_views.py:413`. Reads are outside `BEGIN`.
- **C4 pass.** No `CREATE VIEW`, no RLS, no `managed = False` models, no
  `LATERAL`, no `WITH RECURSIVE`. Nested groups use `MPTTModel`
  (`netbox/netbox/models/__init__.py:162`) — nested-set `lft`/`rght` integer
  ranges, *not* recursive CTEs, which is the cache-friendly choice. All
  `select_for_update` sites are write paths
  (`netbox/netbox/api/viewsets/__init__.py:299,333`; `netbox/dcim/signals.py:86,126`).
  The `RawSQL` uses (`netbox/ipam/views.py:529`, `netbox/ipam/querysets.py:232`)
  are scalar correlated subqueries, which are not on PgCache's exclusion list.
- **C5 pass by default.** `SESSION_SAVE_EVERY_REQUEST = bool(LOGIN_PERSISTENCE)`
  and `LOGIN_PERSISTENCE` defaults to `False`
  (`netbox/netbox/settings.py:157,446`) — no session write on the read path
  unless the operator opts in. Record the knob; leave it off.
- **C6 — no path C.** This is the one real cost. `CACHES` is
  `django_redis` (`netbox/netbox/settings.py:410-419`) but it is used only for
  config revision, release check and the plugin catalog
  (`netbox/netbox/config/__init__.py:78-79`, `netbox/core/plugins.py:233-237`).
  `netbox/netbox/context_managers.py:20-31` is a `ContextVar` query cache —
  **request-scoped memoization, not a cross-request data cache**. NetBox
  removed its object cache (cacheops) years ago. So this is an **A-vs-B
  campaign** and every report must say so, per METHODOLOGY §1.
- **C7 good.** List views prefetch several relations each
  (`netbox/dcim/views.py:1099` prefetches `site__region`, `tenant__group`,
  `location`, `role`), on top of per-object permission evaluation and config
  lookup.

**What makes it interesting:** the cleanest cacheability profile found in this
sweep — no construct on PgCache's exclusion list anywhere near the read path.
It is the closest thing to a best-case subject, which makes it a good control:
if PgCache does not win here, the reason is topology or hit cost, not SQL shape.

**Open, and cheap:** exact statements per request. Not traced — that needs the
app running. `/discover` resolves it in an afternoon with `pg_stat_statements`.

---

### 2. Plane — `promising`

**URL:** https://github.com/makeplane/plane · AGPL-3.0 · very active · clone at
`_sources/plane`

- **C1 pass.** `apps/api/plane/settings/common.py:211`
  `"ENGINE": "django.db.backends.postgresql"`. Also ships a first-class read
  replica alias (`common.py:224-227`), which mirrors real deployments.
- **C2 pass.** No `ATOMIC_REQUESTS`; middleware list
  (`apps/api/plane/settings/common.py:121-135`) contains no transaction wrapper;
  only 13 non-migration `transaction.atomic` sites, all writes.
- **C4 essentially clean.** No views, no RLS, no LATERAL on the read path.
- **C5 confined.** `apps/api/plane/middleware/logger.py:80-92`
  (`APITokenLogMiddleware`) persists a log row to Postgres on API requests — but
  `process_request` returns early unless an `X-Api-Key` header is present
  (`logger.py:130-136`). **Same shape and same mitigation as Strapi's
  `last_used_at`:** benchmark with session/JWT auth and the write disappears.
- **C6 — thin but real path C.** `apps/api/plane/utils/cache.py:25-51`
  (`cache_response`) is a genuine cross-request Redis response cache keyed on
  path + user id. But it decorates only **5** endpoints (estimates, labels,
  instance config — `plane/app/views/workspace/label.py:21`,
  `plane/license/api/views/instance.py:34`), none of them the hot issue list.
- **C7 good.** Issue list does `select_related("workspace","project","state","parent")`
  plus multiple `prefetch_related` blocks
  (`apps/api/plane/app/views/issue/base.py:118,566,572`).

**Trade-off vs NetBox:** Plane has a real path C where NetBox has none, so the
campaign can answer the platform's actual question ("which cache") rather than
"does caching help". But its path C covers cold endpoints, so path C would be
near-inert on the hot workload — which is itself a publishable finding, provided
the report states it rather than hiding it.

---

### 3. Saleor — `promising`

**URL:** https://github.com/saleor/saleor · BSD-3-Clause · very active · clone
at `_sources/saleor`

- **C1 pass.** `saleor/settings.py:136-149`, default DSN
  `postgres://saleor:saleor@localhost:5432/saleor`, plus a replica connection.
- **C2 pass.** No `ATOMIC_REQUESTS`; middleware is three entries and none opens
  a transaction (`saleor/settings.py:281-285`).
- **C4 pass on read traffic.** 248 `select_for_update` hits sounds alarming and
  is not: they cluster entirely in mutation and lock modules —
  `saleor/warehouse/management.py` (16), `saleor/discount/utils/promotion.py`
  (13), `saleor/checkout/complete_checkout.py` (8), `saleor/*/lock_objects.py`.
  Weighted by read traffic (as C4 instructs) the read path is clean: no views,
  no RLS, no LATERAL, no recursive CTE.
- **C6 — weak path C.** `saleor/settings.py:971` `CACHES = django_cache_url.config()`
  is operator-configured. The GraphQL DataLoaders
  (`saleor/graphql/core/dataloaders.py:7,20`) are **request-scoped
  memoization** — exactly the case C6 warns not to mistake for path C.
- **C7 best in sweep.** A GraphQL API with a full dataloader layer is the
  highest-amplification read path here: one storefront query fans out to many
  batched statements.

**Why it is ranked below Plane despite better amplification:** the dataloaders
cut both ways. They already batch away the N+1 that PgCache would otherwise
absorb, so the headroom is smaller than the raw statement count suggests, and
quantifying that needs a trace. That is a more expensive question than anything
outstanding on NetBox or Plane.

---

### 4. Medusa — `uncertain`

**URL:** https://github.com/medusajs/medusa · MIT · very active · clone at
`_sources/medusa`

Genuinely undecided, and the missing piece is specific.

- **C1 pass.** *"PostgreSQL installed and running"* listed as a mandatory
  prerequisite ([installation
  docs](https://docs.medusajs.com/learn/installation)); Postgres-only.
- **C2 pass at the module-service layer — encouragingly explicit.** Read methods
  `retrieveProduct`, `listProducts`, `listAndCountProducts` carry `@InjectManager()`
  (`packages/modules/product/src/services/product-module-service.ts:222,271,323`),
  and that decorator **deliberately strips the transaction from the context**:
  `packages/core/utils/src/modules-sdk/decorators/inject-manager.ts:32-34`
  skips the key `transactionManager` when copying context. Transactions come
  only from `@InjectTransactionManager`
  (`inject-transaction-manager.ts:27-53` → `mikro-orm-repository.ts:63-75`),
  used on write internals.
- **C4 pass.** The 147 `CREATE VIEW` hits are false positives — UI "view
  configuration" tests (`integration-tests/http/__tests__/view-configurations.spec.ts`).
  No database views, no RLS, no recursive CTE on the read path.

**What is unknown:** whether Medusa's HTTP read path reaches those module
services *without* an ambient transaction opened higher up — at the API
middleware or workflow-engine layer. Medusa v2 routes store/admin reads through
`query.graph()` / remoteQuery and its workflow engine has its own transaction
semantics. The module layer being clean does not prove the request layer is.
This is precisely the layer where Strapi's wrapper lived.

**Cheapest resolution (~30 minutes, no build):** grep the workflow engine and
HTTP layer for transaction entry —
`grep -rn "\.transaction(\|transactionManager" packages/core/workflows-sdk/src packages/medusa/src/api` —
and confirm whether the store product-list route enters one. If the answer is
still ambiguous, the decisive test is empirical and cheap: run Medusa against a
logging Postgres (`log_statement=all`) and check whether `BEGIN` precedes the
`SELECT`s on `GET /store/products`. That is the same three-minute experiment
`strapi/spike/run.sh` performs.

---

### 5. Cal.com — `uncertain`

**URL:** https://github.com/calcom/cal.com (repo now resolves to
`calcom/cal.diy`) · MIT · active · clone at `_sources/calcom`

Attractive subject, real C4 doubt — ranked here because resolving it needs
traffic weighting, which is the expensive kind of question.

- **C1 pass.** `packages/prisma/schema.prisma`: `provider = "postgresql"`.
- **C2 pass.** All 22 non-test `$transaction` uses are mutation handlers
  (`packages/trpc/server/routers/viewer/eventTypes/heavy/update.handler.ts:421`,
  `packages/features/bookings/lib/handleSeats/create/createNewSeat.ts:44`,
  `packages/features/bookings/repositories/BookingRepository.ts:2022`). No
  `$extends`/`$use` wrapper on the client (`packages/prisma/index.ts:48`),
  no ambient transaction context. Plain reads are in autocommit.
- **C4 — the problem, and it is on read traffic.** Two independent hits:
  1. **Recursive CTE in the feature-flag path.**
     `packages/features/flags/features.repository.ts:209` and
     `:413`, plus `repositories/PrismaTeamFeatureRepository.ts:188` and
     `PrismaUserFeatureRepository.ts:209`, all run
     `WITH RECURSIVE TeamHierarchy AS (...)` inside
     `checkIfUserBelongsToTeamWithFeature`. No `unstable_cache` wrapper in that
     file. Feature-flag checks fire on many user-facing reads, so this is a
     recursive CTE **on hot traffic**, not a cold admin screen.
  2. **Real database views.** `RoutingFormResponse` and
     `public."BookingTimeStatus"` are created by migrations
     (`packages/prisma/migrations/20250313110448_update_routing_form_response/migration.sql:1`,
     `packages/prisma/migrations/20240909162522_union_insights_data/migration.sql:1`).
     These back the Insights dashboard — warmer than an admin screen, colder
     than the booking page.

**Why `uncertain` and not `disqualified`:** C4 is "fatal if pervasive, demoting
if confined". One recursive CTE per request plus a view-backed analytics
dashboard is confined — the booking/availability read path itself has no
excluded construct. But whether that leaves enough cacheable read volume is a
traffic question, not a grep question.

**Cheapest resolution:** pick the intended benchmark workload first (booking
page + availability only, excluding Insights), then count what fraction of its
statements are the `TeamHierarchy` CTE. If feature-flag checks turn out to be
memoized per request upstream of the repository, the concern largely evaporates
— check the tRPC middleware before anything else.

---

### 6. Keycloak — `disqualified`

**URL:** https://github.com/keycloak/keycloak · Apache-2.0 · very active ·
clone at `_sources/keycloak`
**Criterion:** C2 (plain reads wrapped in transactions)

Keycloak opens a real database transaction on **every** REST request, without
regard to whether the request only reads:

`quarkus/runtime/src/main/java/org/keycloak/quarkus/runtime/integration/resteasy/TransactionalSessionHandler.java:77-82`

```java
KeycloakTransactionManager transactionManager = currentSession.getTransactionManager();
if (!transactionManager.isActive()) {
    // This handler is always running in a blocking thread.
    beginTransaction(currentSession);
}
```

The chain to a real `BEGIN` is complete and unconditional:

1. `.../resteasy/TransactionalSessionHandler.java:77-82` — per request, begins
   if not already active. There is no read/write branch.
2. `.../transaction/TransactionalSessionHandler.java:35-36` —
   `session.getTransactionManager().begin()`.
3. `services/src/main/java/org/keycloak/services/DefaultKeycloakTransactionManager.java:98,116-118`
   — begins every enlisted transaction.
4. `model/jpa/src/main/java/org/keycloak/connections/jpa/JpaKeycloakTransaction.java:42-43`
   — `em.getTransaction().begin()`, a genuine JDBC transaction with autocommit
   off.

The session itself is `@RequestScoped`
(`.../integration/cdi/KeycloakBeanProducer.java:37-43`), so the scope is exactly
one HTTP request. Every token, userinfo and admin-API read therefore executes
inside `BEGIN…COMMIT` and PgCache passes all of it through — the identical
condition measured on Strapi.

**Painful, because it was otherwise the best subject in the sweep.** C1 passes
(PostgreSQL 18 is a tested first-class target, [supported
databases](https://www.keycloak.org/server/db)), and it has by far the strongest
**path C** of any candidate: Infinispan is a real cross-request distributed data
cache, not memoization, which would have made a genuine three-path A/B/C
campaign. It is disqualified by PgCache's in-transaction behaviour, not by its
own design.

**Re-open if PgCache gains in-transaction serving** — alongside Strapi, this is
the second candidate that becomes first-rate the moment that limitation lifts.
That is now two independent stacks (Node/Knex and Java/Hibernate) killed by the
same single limitation, which is a finding about PgCache more than about the
subjects.

---

### Died in phase 1 (metadata only)

All on **C1**, each with a quoted source. Killing these cost minutes:

| Candidate | Category | Evidence |
|---|---|---|
| **Ghost** | headless CMS | *"MySQL 8"* listed as the prerequisite for "the officially recommended production installation"; no Postgres path ([install docs](https://docs.ghost.org/install/ubuntu/)) |
| **Novu** | notification infra | Community compose defines `mongo:8.0.17` and `redis:alpine` as the only datastores ([docker-compose](https://github.com/novuhq/novu/blob/next/docker/community/docker-compose.yml)) |
| **GrowthBook** | feature flags | *"you will also need a MongoDB instance (or MongoDB compatible database) to store login credentials, cached experiment results, and metadata"* ([self-host docs](https://docs.growthbook.io/self-host)) |
| **Matomo** | analytics | *"MySQL version 5.5 or greater, or MariaDB"*; Postgres not supported ([README](https://github.com/matomo-org/matomo)) |

### Swept but not taken to phase 2

Checked for C8 only (licence, activity, archived status via the GitHub API on
2026-08-03 — all active, none archived, all workable licences). **Not
disqualified — simply not yet examined.** Recorded so a later sweep does not
redo the C8 pass:

Discourse (GPL-2.0), Chatwoot, Zammad (AGPL-3.0), OpenProject (GPL-3.0),
Directus, Payload (MIT), Unleash (AGPL-3.0), Twenty, Gitea (MIT), Zitadel
(AGPL-3.0), Vendure, Flagsmith (BSD-3-Clause).

Two are worth a future look. **Discourse** is Postgres-only with a real Redis
data cache (a true path C) and very high amplification; the cost is operational,
it is a heavy app to run. **Unleash** is Postgres-only, but its hot endpoint is
served from an in-memory snapshot, which likely leaves PgCache little to absorb
— cheap to confirm, and a probable C7 demotion.

Payload and Gitea carry a known C1 risk (MongoDB and SQLite defaults
respectively); Directus, Vendure and Gitea are multi-database, which per C1 is
not fatal but means the SQL is lowest-common-denominator.

### Method validation (C2 regression check)

The C2 test used above — locate the data layer's read method definitions, check
for an unconditional transaction wrapper, then look for an ambient transaction
context other queries join — was run against Strapi *before* the finalists, and
it does catch it:

- `packages/core/core/src/services/document-service/repository.ts:766-773`
  wraps `findMany`, `findFirst`, `findOne` and `count` in `wrapInTransaction`
  alongside the mutations;
- `common.ts:8-10` shows the wrapper is
  `strapi.db.transaction?.(() => fn(...args))` — a real transaction;
- `packages/core/database/src/transaction-context.ts:1,29,31` is the
  `AsyncLocalStorage` ambient context that subsequent queries join.

Strapi comes out disqualified on C2, as the recorded verdict requires. The same
method found Keycloak's wrapper in a completely different language and
framework, which is some evidence it generalises.

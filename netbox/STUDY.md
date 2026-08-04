# API Study — NetBox

**Subject:** NetBox 4.6.7 (Community), `netbox/release.yaml:1-3`; audited commit `4afdb31b89ea1adc457f38af945b58f9743ff610` (shallow clone, 2026-08-03), https://github.com/netbox-community/netbox
**Storage layer audited:** Django 6.0.7 ORM + PostgreSQL only. Query generation in `netbox/utilities/querysets.py`, `netbox/utilities/api.py`, `netbox/netbox/tables/tables.py`, `netbox/netbox/api/viewsets/__init__.py`, `netbox/extras/querysets.py`, `netbox/netbox/search/backends.py`. Paths relative to `_sources/netbox/`.
**Author / date:** Discovery agent crew (sql-auditor, cache-hunter, oracle-designer), 2026-08-03

> **Evidence basis.** Rows marked **[T]** are *truth*: captured from a live PostgreSQL 17.7 running NetBox 4.6.7 with `pg_stat_statements` (`track=top`, `track_utility=on`), 198 tables migrated, 60 devices / 60 prefixes / 60 IPs seeded with every FK populated, driven through `django.test.Client` with `CaptureQueriesContext`. Rows marked **[I]** are *intent*: read from code only. This study is measured where the previous subject's was traced.

---

## 1. SQL profile of the read path

### 1.1 Structural screen against PgCache's non-cacheable list

Run against the live migrated schema and against the 217 distinct `SELECT` shapes `pg_stat_statements` recorded across every read path exercised:

| PgCache disqualifier | Result | Evidence |
|---|---|---|
| Views / materialized views | **0** (only `pg_stat_statements` itself) | **[T]** `pg_class` scan, `relkind IN ('v','m')`, schema `public` |
| Row-level security | **0** tables with `relrowsecurity` | **[T]** `pg_class.relrowsecurity` scan |
| Tables without a PK | **0 of 198** | **[T]** `pg_constraint contype='p'` anti-join. Every m2m through-table carries Django's implicit serial `id` |
| `LATERAL` | **0** | **[T]** `pg_stat_statements` scan over 217 SELECT shapes |
| Recursive CTE | **0** | **[T]** same scan. Hierarchies are MPTT nested sets (`lft`/`rght`/`tree_id`/`level`), `netbox/netbox/models/__init__.py:162` |
| `FULL OUTER` / `CROSS JOIN` | **0** | **[T]** same scan |
| Locking clauses | **0 on any read path** | **[T]** same scan. **[I]** 5 `select_for_update()` sites, all write-path: `netbox/netbox/api/viewsets/__init__.py:299,333`, `netbox/dcim/signals.py:86,126`, `netbox/dcim/utils.py:212` |
| Volatile functions in predicates | **0** | **[T]** same scan. Django binds timestamps client-side as parameters |

**No disqualifier is present.** The cleanest structural screen the platform has recorded.

### 1.2 The transaction question (C2) — verified, and clean

The previous candidate (Strapi) was disqualified because its reads ran inside `BEGIN…COMMIT`, which PgCache passes through. NetBox does not.

| Check | Result | Evidence |
|---|---|---|
| `ATOMIC_REQUESTS` anywhere in the tree | **zero hits** | **[T]** repo-wide grep |
| Middleware opens a transaction | No | **[I]** `netbox/netbox/middleware.py:52-98`; the only request processor is `event_tracking`, which sets contextvars only (`netbox/netbox/context_managers.py:9-31`) |
| `transaction.atomic` on read paths | None. 27 non-test sites; 25 are `atomic(using=router.db_for_write(...))` in write handlers (`views/generic/object_views.py:303`, `views/generic/bulk_views.py:413,726,930,1083,1190,1327`, `api/viewsets/__init__.py:256,295,329`, `api/viewsets/mixins.py:64,125,188`). The 2 unqualified ones (`dcim/signals.py:100,347`) are inside `post_save`/`pre_delete` | **[I]** |
| **Statements observed inside a complete read request** | **`BEGIN`/`COMMIT`/`ROLLBACK`: 0.** A full `/ipam/prefixes/?per_page=50` request produced 121 statements, all `SELECT` | **[T]** `pg_stat_statements_reset()` -> warm request -> measured request -> dump, `track_utility=on` confirmed via `SHOW` |
| Non-SELECT statements on the API read path | **0** | **[T]** `CaptureQueriesContext` over `/api/dcim/devices/` and `/api/ipam/prefixes/` under token auth |

**Verdict on C2: NetBox reads run in autocommit. The entire read body is visible to PgCache as cacheable single statements.**

### 1.3 Read-path SQL catalog

Counts are for a 50-row page against a fully-populated dataset.

#### Workload A — REST API list, token auth, superuser: `GET /api/dcim/devices/?limit=50` -> **28 statements, 22 distinct shapes** **[T]**

| # | App operation | SQL emitted (shape) | Cacheable? | Why / why not |
|---|---|---|---|---|
| 1 | Token authentication | `SELECT … FROM users_token WHERE version = $1 AND key = $2 LIMIT $3` | **Yes** | Single-table equality. `netbox/netbox/api/authentication.py:58-67` |
| 2 | Token -> user | `SELECT … FROM users_user WHERE id IN ($1)` | **Yes** | Single-table PK prefetch |
| 3 | ObjectType resolution | `SELECT … FROM core_objecttype INNER JOIN django_content_type ON (…) WHERE app_label = $1 AND model = $2 LIMIT $3` | **Yes** | 2-table inner join, equality. `netbox/core/models/object_types.py:67-108`; memoized per-request in `query_cache` so it fires once per distinct model |
| 4-5 | Custom-field resolution (x2) | `SELECT … FROM extras_customfield INNER JOIN extras_customfield_object_types ON (…) LEFT OUTER JOIN django_content_type T4 ON (…) WHERE contenttype_id = $1 ORDER BY …` | **Yes** | 3-table join, equality, no volatile fn. `netbox/netbox/api/viewsets/mixins.py` |
| 6 | Pagination count | `SELECT COUNT(*) FROM dcim_device LEFT OUTER JOIN … (8 joins, inherited from the config-context annotation)` | **Yes** | Plain LEFT/INNER joins. `StripCountAnnotationsPaginator` at `netbox/dcim/api/views.py:424` strips the *annotations* but not the joins |
| 7 | **Main list query** | `SELECT dcim_device.*, (SELECT JSONB_AGG(V0.data ORDER BY V0.weight, V0.name) FROM extras_configcontext V0 LEFT OUTER JOIN x18 m2m/MPTT tables WHERE … AND (V13.tag_id IN (SELECT DISTINCT U0.tag_id FROM extras_taggeditem U0 …) OR V13.tag_id IS NULL) AND ((V16.level <= dcim_location.level AND V16.lft <= dcim_location.lft AND V16.rght >= dcim_location.rght AND V16.tree_id = dcim_location.tree_id) OR …)) AS config_context_data FROM dcim_device LEFT/INNER JOIN x8 ORDER BY name, id LIMIT $1` | **Yes, with a caveat** | **No LATERAL, no recursive CTE, no FULL/CROSS JOIN, no volatile fn.** A correlated scalar subquery with nested `IN (SELECT …)` and MPTT range predicates — passes the checklist. Caveat: **~22 tables** feed one cached result. Suppressed by `?brief=true` or `?exclude=config_context`. `netbox/extras/api/mixins.py:31-50`; subquery at `netbox/extras/querysets.py:88-100`, MPTT ranges `:130-183`; wired at `netbox/dcim/api/views.py:413-418` |
| 8-27 | 20 serializer-driven prefetches | `SELECT … FROM <table> WHERE id IN ($1,…,$n)` — one per relation, including **7x `ipam_ipaddress`** (`primary_ip4`, `primary_ip6`, `oob_ip` and their `nat_inside`/`nat_outside` legs) | **Yes — ideal** | Single-table `IN` SELECTs, the openFGA-shaped case. `get_prefetches_for_serializer` at `netbox/utilities/api.py:154-194`, applied at `netbox/netbox/api/viewsets/__init__.py:103-105` |
| 28 | Tag prefetch | `SELECT DISTINCT (extras_taggeditem.object_id) AS _prefetch_related_val, extras_tag.* FROM extras_tag INNER JOIN extras_taggeditem ON (…) INNER JOIN django_content_type ON (…) WHERE app_label = $1 AND model = $2 AND object_id IN (…) ORDER BY weight, name` | **Yes** | 3-table inner join, equality + `IN` |

**Cacheable: 28/28 = 100%.**

#### Workload B — same request as a *non-superuser* with an `ObjectPermission` -> **32 statements** **[T]**

NetBox's analogue of the auth prefix that dominated the previous subject:

| # | App operation | SQL emitted (shape) | Cacheable? | Why / why not |
|---|---|---|---|---|
| a | ObjectPermission fetch | `SELECT DISTINCT ON (users_objectpermission.id) … FROM users_objectpermission LEFT OUTER JOIN users_user_object_permissions … LEFT OUTER JOIN users_group_object_permissions … LEFT OUTER JOIN users_group … LEFT OUTER JOIN users_user_groups … WHERE (users_user_object_permissions.user_id = $1 OR users_user_groups.user_id = $2) AND enabled ORDER BY id` | **Yes** | `DISTINCT ON` + 4 LEFT JOINs, not on the exclusion list; all 5 tables have PKs. `netbox/netbox/authentication/__init__.py:101-104` |
| b | Its `object_types` prefetch | `SELECT (users_objectpermission_object_types.objectpermission_id) AS _prefetch_related_val, django_content_type.* FROM … WHERE objectpermission_id IN (…)` | **Yes** | m2m join, `IN` |
| c-d | Django `ModelBackend` permission strings (x2) | `SELECT content_type.app_label, auth_permission.codename FROM auth_permission INNER JOIN users_user_user_permissions … WHERE user_id = $1` and `… INNER JOIN users_group_permissions … WHERE group_id IN (SELECT U0.id FROM users_group U0 INNER JOIN users_user_groups U1 … WHERE U1.user_id = $1)` | **Yes** | Inner joins + non-correlated `IN (SELECT …)`. `AUTHENTICATION_BACKENDS` at `netbox/netbox/settings.py:581-584`; `restrict()` calls `user.get_all_permissions()` (`netbox/utilities/querysets.py:56`) |

**Constrained permissions** add no new statement — `restrict()` folds them into the *same* query as `WHERE pk IN (SELECT … FROM <model> WHERE <constraints>)` (`netbox/utilities/querysets.py:59-66`), deliberately not `DISTINCT` (comment at `:63` cites issue #8715). **Cacheable: 32/32 = 100%.**

The permission cache is per-`User`-instance (`netbox/netbox/authentication/__init__.py:77-79`) and `AuthenticationMiddleware` rebuilds the user object every request, so this prefix is paid once per request, not once per queryset.

#### Workload C — HTML UI list, session auth: `GET /dcim/devices/?per_page=50` -> **74 statements, 21 distinct shapes** **[T]**

| # | App operation | SQL emitted (shape) | Cacheable? | Why / why not |
|---|---|---|---|---|
| 1 | **Session lookup** | `SELECT session_key, session_data, expire_date FROM django_session WHERE expire_date > $1::timestamptz AND session_key = $2 LIMIT $3` | **Structurally yes, practically no** | `$1` is a *client-generated* timestamp, new every request — not `NOW()`, so it does not trip the volatile-function rule, but the answer legitimately differs per call. See §1.5. `netbox/netbox/settings.py:446-448` |
| 2 | User load | `SELECT … FROM users_user WHERE id = $1 LIMIT $2` | **Yes** | PK equality |
| 3-7 | Chrome: ObjectType, custom fields x4, custom links, `users_userconfig`, `extras_exporttemplate`, `extras_tableconfig` | equality / m2m-join SELECTs | **Yes** | `users_userconfig` at `netbox/netbox/tables/tables.py:180-186`, `netbox/utilities/paginator.py:88-90` |
| 8-9 | Navbar unread badge x2 | `SELECT $1 AS "a" FROM extras_notification WHERE user_id = $2 AND read IS NULL LIMIT $3` | **Yes** | Single-table `EXISTS` probe |
| 10-11 | Paginator | `SELECT COUNT(*) FROM dcim_device`, `SELECT $1 AS "a" FROM dcim_device LIMIT $2` | **Yes** | |
| 12 | Main list query | `SELECT dcim_device.*, dcim_virtualchassis.* FROM dcim_device LEFT OUTER JOIN dcim_virtualchassis ON (…) ORDER BY name, id LIMIT/OFFSET` | **Yes** | `select_related('virtual_chassis')` at `netbox/dcim/views.py:2606`. **The UI list does NOT annotate config context** |
| 13-19 | 7 table-driven prefetches | `SELECT … FROM <tenancy_tenant\|dcim_site\|dcim_location\|dcim_rack\|dcim_devicerole\|dcim_devicetype\|dcim_manufacturer> WHERE id IN (…)` | **Yes** | Derived from *visible columns* at `netbox/netbox/tables/tables.py:119-159`, invoked at `:207`. Uses `prefetch_related`, never `select_related` |
| 20-69 | **N+1: 50 x primary-IP lookup** | `SELECT … FROM ipam_ipaddress WHERE id = $1 LIMIT $2` — one per row | **Yes — the best possible case** | `DeviceTable.primary_ip` (`netbox/dcim/tables/devices.py:193-197`) is a default column (`:281-284`) but `Device.primary_ip` is a **property**, not a field (`netbox/dcim/models/devices.py:1098-1106`). `_apply_prefetching` hits `FieldDoesNotExist` and `break`s (`netbox/netbox/tables/tables.py:145-148`), so nothing is prefetched. Identical shape, tiny row, high repetition |
| 70-74 | Tag column, misc | `SELECT DISTINCT extras_tag.* … GROUP BY / ORDER BY` | **Yes** | |

**Cacheable: 73/74 = 98.6%** (only `django_session` is effectively uncacheable).

#### Workload D — UI prefix list: `GET /ipam/prefixes/?per_page=50` -> **120 statements, 18 distinct shapes** **[T]**

| # | App operation | SQL emitted (shape) | Cacheable? | Why / why not |
|---|---|---|---|---|
| 1-18 | Chrome + main query + 3 prefetches (`ipam_vrf`, `tenancy_tenant`, `ipam_role`) | `SELECT ipam_prefix.* FROM ipam_prefix ORDER BY vrf_id NULLS FIRST, prefix, id LIMIT/OFFSET`; `WHERE id IN (…)` | **Yes** | `netbox/ipam/views.py:777-783`. `depth`/`children` read the denormalized `_depth`/`_children` columns (`netbox/ipam/tables/ip.py:173-185`) — no extra query |
| 19-68 | **N+1a: 50 x utilized-range scan** | `SELECT start_address, end_address FROM ipam_iprange WHERE CAST(HOST(end_address) AS INET) BETWEEN $1 AND $2 AND mark_utilized AND CAST(HOST(start_address) AS INET) BETWEEN $3 AND $4 AND vrf_id = $5` | **Yes** | `inet` range predicate, immutable functions only |
| 69-118 | **N+1b: 50 x child-IP count** | `SELECT COUNT(DISTINCT (HOST(ipam_ipaddress.address))::inet) FROM ipam_ipaddress WHERE CAST(HOST(address) AS INET) BETWEEN $1 AND $2 AND vrf_id = $3` | **Yes** | `HOST()`/`INET` are immutable |
| — | Source | `PrefixTable.utilization` uses `accessor='get_utilization'` (`netbox/ipam/tables/ip.py:224-228`), a Python method issuing both queries per row (`netbox/ipam/models/ip.py:594-624`) | | |

**Cacheable: 119/120 = 99.2%.**

#### Workload E — global search: `GET /search/?q=dev` -> **360 statements, 16 distinct shapes** **[T]**

| # | App operation | SQL emitted (shape) | Cacheable? | Why / why not |
|---|---|---|---|---|
| 1 | Object-type probe | `SELECT extras_cachedvalue.object_type_id AS object_type FROM extras_cachedvalue WHERE UPPER(value::text) LIKE UPPER($1) ORDER BY … LIMIT $2` | **Yes** | `UPPER`/`LIKE` are immutable. `netbox/netbox/search/backends.py:139-151` |
| 2 | **Ranked result set (raw SQL)** | `SELECT * FROM (SELECT extras_cachedvalue.*, ROW_NUMBER() OVER (PARTITION BY object_type_id, object_id ORDER BY weight ASC) AS row_number FROM extras_cachedvalue WHERE UPPER(value::text) LIKE UPPER($1) ORDER BY … LIMIT $2) t WHERE row_number = $3` | **Yes, flagged** | Window function inside a derived table, via `.raw()` at `netbox/netbox/search/backends.py:162-167`. Not on the exclusion list, but it is the only hand-written SQL on any read path and the only window function — worth a targeted differential test |
| 3-352 | **N+1: 350 x per-row FK dereference** | 7 shapes x 50 rows, each `WHERE id = $1 LIMIT $2` | **Yes — ideal** | `prefetch_related_objects(objects, 'object__site', …)` at `netbox/netbox/search/backends.py:170-189` cannot batch across the `GenericForeignKey` |

**Cacheable: 359/360 = 99.7%.**

#### Workload F — REST API detail: `GET /api/dcim/devices/<id>/` -> **27 statements** **[T]**
Identical to Workload A minus the `COUNT(*)`, with `WHERE dcim_device.id = $1 LIMIT $2`. **27/27 cacheable.**

#### Other paths measured **[T]**

| Path | Statements | Notes |
|---|---|---|
| `/api/dcim/devices/?limit=50&brief=true` | 9 | `brief_fields` yields **0** prefetches (`netbox/netbox/api/viewsets/__init__.py:128-131`) |
| `/api/ipam/prefixes/?limit=50` | 11 (su) / 15 (restricted) | 4 prefetches. `netbox/ipam/api/views.py:85-88` |
| `/api/ipam/ip-addresses/?limit=50` | 11 | `GenericPrefetch` at `netbox/ipam/api/views.py:106-117` correctly batches the GFK — **no N+1 on the API side** |
| `/ipam/ip-addresses/?per_page=50` (UI) | 19 | Default columns avoid the GFK |
| `/dcim/devices/<id>/` (UI) | 39 | Panel-driven; MPTT ancestor walks |
| `/` (dashboard) | 68 | 21 x `core_objecttype` for 21 *distinct* models — the per-request memo is working |

#### Read paths that look dangerous but are not

| Thing | Finding | Evidence |
|---|---|---|
| `RawSQL` in `PrefixQuerySet.annotate_hierarchy()` | Two scalar correlated subqueries. **Write path only** — called from signals | `netbox/ipam/querysets.py:226-247`; `netbox/ipam/signals.py:10-26` |
| `RawSQL` on the aggregate list | `child_count = RawSQL('SELECT COUNT(*) FROM ipam_prefix WHERE …')` — scalar correlated subquery | `netbox/ipam/views.py:528-530` |
| Nested groups | MPTT nested sets — integer range comparisons, **never** a recursive CTE | `netbox/netbox/models/__init__.py:162` |
| `count_related()` annotations | `Coalesce(Subquery(… Count('*')), 0)` — scalar correlated subquery | `netbox/utilities/query.py:13-27` |
| `django_prometheus` DB backend | Instrumentation wrapper only; emits no additional SQL | `netbox/netbox/settings.py:294-297` |

### 1.4 Cacheable fraction of read traffic

Weighted by **statements**, not by distinct shapes:

| Workload shape | Statements/req | Cacheable | Fraction |
|---|---|---|---|
| REST API list, **token auth**, superuser | 28 | 28 | **100.0%** |
| REST API list, token auth, restricted user | 32 | 32 | **100.0%** |
| REST API detail, token auth | 27 | 27 | **100.0%** |
| REST API list, `?brief=true` | 9 | 9 | **100.0%** |
| HTML UI device list, **session auth** | 74 | 73 | **98.6%** |
| HTML UI prefix list, session auth | 120 | 119 | **99.2%** |
| HTML UI IP list, session auth | 19 | 18 | **94.7%** |
| Global search, session auth | 360 | 359 | **99.7%** |
| Dashboard, session auth | 68 | 67 | **98.5%** |

**Headline: 100% for a token-authenticated REST API workload; 98-99% for any session-authenticated HTML workload.** The single non-cacheable statement under session auth is the `django_session` lookup.

### 1.5 Volatile exceptions and the exclusion plan

1. **`django_session` expiry predicate** — `WHERE expire_date > $1::timestamptz AND session_key = $2`. `$1` is `timezone.now()` bound client-side, so not a volatile SQL function, but it changes every request. **Under the simple protocol — which is what NetBox actually uses (§5.1) — the timestamp is interpolated into the query text, producing a new query text on every single request:** unbounded shape-space growth and cache pollution.
   **Plan (preferred): drive the benchmark through token auth**, which never touches `django_session` and replaces it with a stable `users_token WHERE version=$1 AND key=$2` equality SELECT. **Fallback:** set `SESSION_FILE_PATH` (`netbox/netbox/settings.py:447-448`), or exclude `django_session` via `ALLOWED_TABLES`. Pin identically across paths.
2. **`users_token.last_used` UPDATE inside a GET** — throttled to at most once per minute per token (`netbox/netbox/api/authentication.py:92-100`). A CDC invalidation event fired from inside a read request. Negligible at 1/min/token; document it.
3. **`MAINTENANCE_MODE` session-state statement** — `SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY` per request when enabled (`netbox/netbox/middleware.py:258-276`). Default `False` (`netbox/netbox/config/parameters.py:199-203`). **Pin to False.**
4. **Shape-space confound (not volatility, but it belongs here).** The 20 serializer prefetches emit `WHERE id IN ($1,…,$n)`. Under the simple protocol the id list is interpolated, so the shape space grows with the *set of ids on the page*. With Zipf-skewed key selection the working set stays bounded; with uniform selection it does not. This is the openFGA `simple_protocol` confound in a new costume and must be stated in every report. Workload W3 (§4.5) is designed to measure it.

### 1.6 Verdict

**Good subject — the best structural fit the platform has found, and materially more interesting than openFGA.** The structural screen is perfectly clean: zero views, zero RLS, zero PK-less tables among 198, zero LATERAL, zero recursive CTEs, zero FULL/CROSS joins, zero locking reads, zero volatile functions in predicates, across 217 distinct SELECT shapes captured from a live instance. Critically, the killer that disqualified the previous candidate is absent and *empirically* absent: `pg_stat_statements` with `track_utility=on` recorded **zero `BEGIN`/`COMMIT`** during complete read requests, so the whole read body is individually cacheable. Where NetBox is *more* interesting than openFGA is amplification: because NetBox 4.6 resolves relations dynamically with `prefetch_related` rather than `select_related`, a single API list request fans out to 28 statements and a UI list to 74-120, of which 50 are the *same* single-row shape — precisely the profile where per-query overhead reduction compounds. Two things to watch, neither disqualifying: (a) the config-context correlated subquery on the default device and virtual-machine responses is cacheable but has a ~22-table invalidation surface, so a device-heavy write mix will invalidate it constantly — measure both with and without `?exclude=config_context` and treat the flag as a pinned knob; and (b) session-authenticated HTML traffic carries one structurally-uncacheable `django_session` lookup per request which, under the simple protocol, becomes a shape-space leak — use token auth for the primary workload. **Recommend proceeding to Phase 2.**

---

## 2. Query amplification

**Measured, not estimated.** `django.test.Client` + `CaptureQueriesContext` against a live PostgreSQL 17.7, cross-validated against `pg_stat_statements` deltas after `pg_stat_statements_reset()`. Agreement was exact (19 vs 19, 74 vs 74, 120 vs 120). One warm request precedes each measured request so per-process caches are hot. Page size 50 (`netbox/netbox/config/parameters.py:134-138`). Dataset: 60 devices / 60 prefixes / 60 IPs with **every** FK populated.

| User-facing operation | Datastore queries | Distinct shapes | Notes |
|---|---|---|---|
| `GET /api/dcim/devices/?limit=50` (token, superuser) | **28** | 22 | 20 of 28 are prefetch round trips |
| Same, restricted user | **32** | 26 | +4 permission-prefix queries |
| `GET /api/dcim/devices/<id>/` | **27** | 21 | detail is nearly as expensive as a 50-row list |
| Same, `?brief=true` | **9** | 9 | prefetch list collapses to empty |
| `GET /api/ipam/prefixes/?limit=50` | **11** / **15** (restricted) | 10 / 14 | |
| `GET /api/ipam/ip-addresses/?limit=50` | **11** | 10 | GFK correctly batched |
| `GET /dcim/devices/?per_page=50` (UI) | **74** | 21 | 50 are one repeated shape |
| Same, restricted user | **78** | 25 | |
| `GET /ipam/prefixes/?per_page=50` (UI) | **120** | 18 | 100 are two repeated shapes |
| `GET /ipam/ip-addresses/?per_page=50` (UI) | **19** | 15 | |
| `GET /dcim/devices/<id>/` (UI) | **39** | 37 | almost no repetition — panel-driven |
| `GET /` (dashboard) | **68** | 26 | ~20 widget `COUNT(*)`s |
| `GET /search/?q=dev` | **360** | 16 | 350 are seven repeated shapes |

**Sensitivity to data density — state this in every report.** Django's `prefetch_related` skips a level entirely when the parent FK set is all-`NULL`. With sparse FKs, `/api/dcim/devices/?limit=50` cost **19** queries; with every FK populated, **28**. **Amplification on NetBox is a property of the seed, not of the endpoint.** §6 pins FK density per rung; without that, A and B runs on differently-shaped data are not comparable (METHODOLOGY §4).

**Does the app batch reads, or issue them one by one? Both — and the split is the whole story.**

*It batches.* NetBox 4.6 resolves relations dynamically: `get_prefetches_for_serializer()` walks the serializer's fields honouring `?fields=`/`?omit=`/`?brief=` and emits one `WHERE id IN (…)` per relation (`netbox/utilities/api.py:154-194`); `BaseTable._apply_prefetching()` does the same from *visible columns* (`netbox/netbox/tables/tables.py:119-159`). Resolved lists dumped from the loaded app registry: `DeviceSerializer` -> **23** prefetches, `PrefixSerializer` -> **8**, `IPAddressSerializer` -> **8**; all collapse to **0** under `brief_fields`. It is `prefetch_related`, never `select_related` — batching means *N separate round trips*, not one join. That design decision is what makes NetBox amplification-rich.

*It does not batch, in four measured places:*

1. **UI device list — 50 queries/page.** `DeviceTable.primary_ip` accessor names a Python **property**, not a model field; `_apply_prefetching` hits `FieldDoesNotExist` and breaks (`netbox/netbox/tables/tables.py:145-148`). 68% of that request's statements.
2. **UI prefix list — 100 queries/page.** `PrefixTable.utilization` uses `accessor='get_utilization'` (`netbox/ipam/tables/ip.py:224-228`), issuing two queries per row (`netbox/ipam/models/ip.py:594-624`). 83% of that request's statements.
3. **Global search — 350 queries per 50 results.** `prefetch_related_objects` cannot batch across the `GenericForeignKey` (`netbox/netbox/search/backends.py:170-189`). 97% of that request's statements.
4. **Per-request fixed overhead — 6-10 queries before any object is touched.** On a `?brief=true` prefix list this is **5 of 6 statements (83%)**. Two things that could have been overhead are *not*: the config revision is served from Redis (`netbox/netbox/config/__init__.py:74-90`), and `ObjectType` lookups are memoized per request (`netbox/core/models/object_types.py:74-77`).

**Why this matters.** Per METHODOLOGY §1.2 a 1-3 query/request CRUD app "barely registers a signal". NetBox is not that app: 28 on the primary API list, 74-120 on UI lists, 360 on search — inside openFGA's 100-480 band on UI paths and an order of magnitude above the CRUD floor on API paths. And the amplification is *concentrated in repeated identical shapes* (50x one shape, 100x two, 350x seven), which is the cache's best case.

---

## 3. Native cache (path C)

**Verdict: NetBox has no cross-request cache of database reads. There is no path C. The campaign is A vs B only, and every report must say so.**

Under `docs/PLATFORM.md` as amended 2026-08-03, path C's absence is scoping, not disqualifying.

| Question | Answer |
|---|---|
| Does the app ship a cache? | **No cache of object/query data.** A Redis `CACHES['default']` (django_redis) exists and is a *required* parameter (`netbox/netbox/settings.py:68`, `:394-395`, `:410-419`), but an exhaustive audit of every `django.core.cache` call site (18 non-test sites) finds exactly **one** that stores anything read from Postgres, and it stores the *configuration revision*, not application data. NetBox's queryset cache (django-cacheops) was removed in v3.0 — `docs/release-notes/version-3.0.md:488` "Drop support for queryset caching (django-cacheops)"; `:303` removed the `cacheops_*` metrics. `requirements.txt` (45 pinned packages) contains no cache library beyond `django-redis==7.0.0`. |
| Coherence model | N/A for data. The one DB-derived cached value (config) is **event-invalidated, no TTL** (`cache.set('config', …, None)` — `netbox/core/models/config.py:77-78`, written from a `post_save` on `ConfigRevision`). Everything else cached is external-HTTP content with a fixed TTL. |
| Default state and flags | No cache flag to turn on. Redis caching is not optional and not tunable: `REDIS` is required and the `caching` subsection raises `ImproperlyConfigured` if missing (`settings.py:68`, `:394-395`). No `CACHE_TIMEOUT`, no `CACHE_ENABLED`, no per-model cache setting exists anywhere in the tree. |
| What it covers / does not cover | Covers: config revision, OpenAPI schema page, RSS feed bodies, plugin catalog, GitHub release check. Does **not** cover: any model, queryset, serializer output, permission set, or list/detail response. |

### Full inventory of `django.core.cache` writes

| Site | Key | Source of the value | TTL | Category |
|---|---|---|---|---|
| `netbox/core/models/config.py:77-78` + `netbox/netbox/config/__init__.py:78-79, 94-122` | `config`, `config_version` | **Postgres** (`core_configrevision`) | `None`, invalidated on save | **(c) config caching** |
| `netbox/core/plugins.py:233-237` | `plugin_catalog_feed` | outbound HTTPS (`settings.py:935`) | 3600 s | (d) external HTTP |
| `netbox/core/views.py:849-854` | catalog-error flag | n/a | 300 s | (d) |
| `netbox/core/jobs.py:233`, `netbox/extras/management/commands/housekeeping.py:171`, read at `netbox/netbox/views/misc.py:57-66` | `latest_release` | outbound HTTPS to GitHub | `None` | (d) |
| `netbox/extras/dashboard/widgets.py:373, 401` | `dashboard_rss_2_<sha256>` | outbound HTTPS RSS | default 3600 (`widgets.py:312-318`), 14400 in the shipped dashboard (`netbox/extras/constants.py:144-153`) | (d) |
| `netbox/netbox/urls.py:59-65` | `cache_page(86400)` on `SpectacularAPIView` | in-process introspection, no DB | 86400 s | **(d) HTTP-level page cache** |

Nothing else writes to the Django cache.

### The request cache is memoization, not a cache — confirmed

`query_cache` is a `ContextVar` (`netbox/netbox/context.py:12`), set fresh at the start of every request and reset to `None` at the end (`netbox/netbox/context_managers.py:20`, `:31`, invoked per request by `CoreMiddleware` at `netbox/netbox/middleware.py:62-63`). Its two consumers memoize only lookup tables — `ObjectType` rows (`netbox/core/models/object_types.py:73-77`) and `CustomField` sets (`netbox/extras/models/customfields.py:74-87`). Nothing survives the response. Object permissions are likewise an attribute on the `user_obj` instance (`netbox/netbox/authentication/__init__.py:77-79`), rebuilt per request. **Category (b). Not path C.**

Two more things that look like caches and are not:

- `SEARCH_BACKEND = 'netbox.search.backends.CachedValueSearchBackend'` (`settings.py:200`) writes to `extras_cachedvalue`, a **Postgres table** populated by `post_save` signals (`netbox/netbox/search/backends.py:199-251`) and read via `.raw()` (`:164`). A denormalized search index living inside the origin, not a cache in front of it. PgCache sees those reads as ordinary SQL.
- `Config` is held in a `threading.local` (`netbox/netbox/config/__init__.py:19, 28-35`) cleared at the end of every request (`middleware.py:93`). Per-thread, per-request. Not a cache.

**Consequence for the design.** The deployment is *simpler and more honest* than openFGA's: there is no app-cache flag whose off-state must be verified, and no risk of a stray cache flag differing between A and B. The Redis instance still must be pinned identically across paths (§5) because it is mandatory infrastructure, and a divergent Redis would show up as a config divergence indistinguishable from a cache defect.

---

## 4. Correctness check design

**Chosen approach:** **Differential (A vs B, byte-exact) is the primary campaign gate**, backed by a **partial analytical oracle** used for seed validation and as response invariants asserted on every gate sample.

### 4.1 Why differential is primary here (and was not on openFGA)

OpenFGA's response is a boolean. NetBox's is a DRF-serialized object graph: 42 concrete columns on `dcim_device` alone, nested brief serializers, `display` strings computed from `__str__`, hyperlinked `url`/`display_url` fields, and a `config_context` value that is a JSONB aggregate deep-merged in Python. A *full-response* analytical oracle would mean reimplementing DRF serialization — a large body of code whose own bugs would surface as PgCache defects. Wrong trade.

What *is* analytically derivable, cheaply and exactly, is a small set of scalars and one large one (`config_context`). Those become **invariants**, not a replacement for the byte-compare:

| Check | Catches | Cannot catch |
|---|---|---|
| Differential A vs B, byte-exact | any divergence introduced by PgCache | A and B wrong the same way (a bad seed) |
| Analytical oracle O1-O5 | a bad seed; a systematically wrong answer on *both* paths | field-level divergence outside the projected scalars |

Both run. The differential is the gate that aborts the campaign; the oracle is the gate that must pass **before** the differential is meaningful.

### 4.2 Differential gate

**What is compared:** HTTP status, `Content-Type`, and the raw response body as bytes. Nothing else.

**Normalization permitted: NONE.** Two known sources of legitimate divergence are engineered away rather than normalized:

1. **Absolute URLs.** `BaseModelSerializer.url` / `.display_url` (`netbox/api/serializers/base.py:19-20`) are `HyperlinkedIdentityField`s whose `get_url()` calls `self.reverse(..., request=request)` (`netbox/api/serializers/fields.py:37`), i.e. `request.build_absolute_uri`. The paginator's `next`/`previous` are the same. Under `hostNetwork` with a distinct port per path (§7), A and B would emit different `url` values for identical data. **Fix, not normalization:** the generator sends an identical `Host: netbox.lab` header to both paths (they are distinguished at the socket by IP:port, not by Host), and both deployments pin `ALLOWED_HOSTS = ['netbox.lab']` and identical `BASE_PATH`. Verified by the A-vs-A pre-test, never assumed.
2. **Session auth.** Removed from the primary gate entirely.

**A-vs-A determinism pre-test (runs before the A-vs-B gate).** Each gate endpoint is issued 3x against path A on a quiescent, read-only database and the bodies byte-compared. An endpoint whose own output is non-deterministic cannot judge PgCache. **This is the only mechanism by which an endpoint may be excluded or a normalization rule admitted**; any exclusion is recorded in the defect log with the observed diff.

Determinism is expected for the list endpoints because every relevant `Meta.ordering` carries a primary-key tiebreak and is therefore a total order:

- `Device`: `ordering = ('name', 'pk')` — `dcim/models/devices.py:251`
- `Prefix`: `ordering = (F('vrf').asc(nulls_first=True), 'prefix', 'pk')` — `ipam/models/ip.py:291`
- `IPAddress`: `ordering = ('address', 'pk')` — `ipam/models/ip.py:1024`
- `IPRange`: `ordering = (F('vrf').asc(nulls_first=True), 'start_address', 'pk')` — `ipam/models/ip.py:712`
- `ConfigContext`: `ordering = ['weight', 'name']` with `name` `unique=True` (`extras/models/configs.py:107`) — total, which is what makes the `EmptyGroupByJSONBAgg('data', order_by=['weight','name'])` aggregate at `extras/querysets.py:96` deterministic.

The one predicted A-vs-A risk is `GET /search/`: `CachedValue.Meta.ordering = ('weight', 'object_type', 'value', 'object_id')` (`extras/models/search.py:55-56`) is *not* guaranteed total. If W5 fails A-vs-A it is excluded from the gate and remains **performance-only**; that fact must appear in the report.

**Gate corpus:** 12,000 request/response pairs per rung per repetition.

| Endpoint | Samples | Why in the corpus |
|---|---|---|
| `GET /api/dcim/devices/?limit=50&offset=k`, k over the W1 Zipf offset set | 3,000 | primary workload; 28 stmt/req; carries `config_context` |
| Same `&exclude=config_context` | 1,500 | isolates the 22-table subquery (`extras/api/mixins.py:48`) |
| `GET /api/dcim/devices/{id}/` | 3,000 | 27 stmt/req; per-id literal shapes |
| Same `&brief=true` | 500 | 9 stmt/req |
| `GET /api/ipam/prefixes/?limit=50&offset=k` | 2,000 | 11 stmt/req; carries `_depth`/`_children` |
| `GET /api/ipam/ip-addresses/?limit=50&offset=k` | 1,000 | second bulk table |
| `GET /api/dcim/devices/?limit=50&offset=k` as the **restricted** user | 1,000 | 32 stmt/req; the 4 extra ObjectPermission shapes |

Target: **100.000000%**. One divergence aborts the campaign and becomes a bug report (METHODOLOGY §2).

**Order of operations, per rung:** seed -> oracle validation -> cold-start PgCache -> A-vs-A pre-test -> A-vs-B differential -> only then any latency run.

### 4.3 Analytical oracle (seed validation + response invariants)

The seed is authored by us, so entities are indexed by construction: device `d` (0-based) has `name = f'dev-{d:07d}'`, and every FK is `d mod N_<thing>`. Zero-padding makes lexical order equal numeric order equal insertion order, which is what makes O1 closed-form under `ordering = ('name','pk')`.

**O1 — pagination identity.** For `GET /api/dcim/devices/?limit=L&offset=K`, `results[j].name` must equal `dev-{K+j:07d}` and `count` must equal the rung's device total. Catches the highest-severity cache failure class — right shape, wrong page — which a byte-compare only catches if path A happens to be correct.

**O2 — `config_context` (the real oracle, targeting the hardest CDC surface).**

`ConfigContextModel.get_config_context()` (`extras/models/configs.py:238-256`) folds the aggregated contexts with `deepmerge` (`utilities/data.py:42-52`: recursive on dict-vs-dict, otherwise replace), in `(weight, name)` order, then merges `local_context_data` last. The matching predicate (`extras/querysets.py:103-183`) is: for each of 13 assignment categories, *either* the context has no assignment in that category *or* the device's attribute is in it; region/site-group/role/platform/location use MPTT ancestor ranges (`extras/querysets.py:146-153`), so a context assigned to an ancestor matches.

Seed construction — all contexts assigned to exactly **one** object in exactly **one** category, all other categories empty (match-all):

| Family | Count | Assigned to | weight | `data` |
|---|---|---|---|---|
| `cc-000-global` | 1 | nothing | 100 | `{"g":1,"layer":{"g":1}}` |
| `cc-1xx-regionroot` | 4 | root region `R_i` | 200 | `{"rr":i,"layer":{"r":i}}` |
| `cc-2xx-regionleaf` | 16 | leaf region `R_ij` | 300 | `{"rl":[i,j],"layer":{"r":"leaf"}}` |
| `cc-3xx-role` | 8 | `DeviceRole k` | 400 | `{"role":k,"layer":{"role":k}}` |
| `cc-4xx-platform` | 8 | `Platform k` | 500 | … |
| `cc-5xx-tenant` | 8 | `Tenant k` | 600 | … |
| `cc-6xx-devicetype` | 8 | `DeviceType k` | 700 | … |
| `cc-9xx-inactive` | 11 | same objects | 800 | conflicting data, `is_active=False` |

Weights distinct per family and names unique, so `(weight,name)` fixes the fold order exactly. Prediction for device `d`, O(7), no query:

```
matched = [global, regionroot(i(d)), regionleaf(i(d),j(d)), role(d%8),
           platform(d%8), tenant(d%8), devicetype(d%8)]
expected = reduce(deepmerge, [c.data for c in matched], {})
if d % 7 == 0: expected = deepmerge(expected, {"local": d, "layer": {"local": True}})
```

Three properties make this a strong oracle rather than a formality:
- the shared `layer` key with nested dicts means a wrong *order* produces a wrong answer, not just a wrong set;
- the `cc-9xx-inactive` family is a negative control — if it ever appears, `is_active=True` was lost from the predicate;
- the region-root family matches only through the MPTT range predicate, so **O2 is simultaneously the validator for the nested-set COPY hazard in §6**. If `lft/rght/tree_id/level` are wrong, O2 fails on the seed before any measurement.

**O3 — prefix hierarchy.** Prefixes seeded as a strict 3-level tree: `10.a.0.0/16` contains 16 x `/20` contains 16 x `/24`. Closed form: `_depth` = 0/1/2 by level; `_children` = 272 for a `/16` (16 + 256 — `hierarchy_children` counts *all* strictly-contained prefixes, `ipam/querysets.py:239-245`), 16 for a `/20`, 0 for a `/24`. Cross-checked by running `annotate_hierarchy()` (`ipam/querysets.py:226-247`) against the seeded table — two independent implementations (that one and the stack walk in `rebuild_prefixes`, `ipam/utils.py:201-250`) must agree with the closed form. Surfaced via `Accessor('_depth')` / `Accessor('_children')` (`ipam/tables/ip.py:173`, `:177`).

**O4 — prefix utilization (UI path).** `_get_utilization_denominator()` (`ipam/models/ip.py:637-645`) returns `size - 2` for IPv4, `prefixlen < 31`, `is_pool=False`. With `status='active'`, no `IPRange` rows and exactly `K` child IPs per `/24`, `get_utilization()` (`ipam/models/ip.py:594-623`) reduces to `min(K/254*100, 100)`. Validates that the per-row utilization queries actually see the seeded IPs.

**O5 — counter fields.** `interface_count` etc. are `CounterCacheField`s (`dcim/models/devices.py:694+`) maintained by signals (`utilities/counters.py`). A fixed interface multiplier makes the expected value constant; a mismatch means §6's `calculate_cached_counts` step was skipped.

**Failure semantics:** any O1-O5 failure is a **seed defect** and blocks the rung. Any O1-O5 failure on path B but not path A is a **PgCache defect** and aborts the campaign.

### 4.4 Staleness probe

**Path assignment.** The write is issued **through path A** — the app wired directly to the origin. The measured interval is then exactly *origin commit -> CDC capture -> PgCache invalidation -> next path-B read reflects it*: pure CDC lag, with no help from the proxy having seen the write itself. A secondary variant issues the write **through path B** and is reported separately; the delta is the coherence PgCache gets from self-observation rather than from CDC — a different quantity, never published as CDC lag.

**No path C means no TTL to compare against.** NetBox's native coherence is trivially zero-lag. This probe measures the *cost* of CDC coherence in absolute terms, and any non-convergence is a hard correctness failure, not a slow number.

**Timing.** `T0` = wall-clock receipt of the write response on path A (post-commit by construction). `T1` = the *intended arrival instant* (open-loop, not send-completion) of the first converged poll on B. Polls scheduled every 10 ms independent of response time. Reported lag over-estimates by at most one poll interval; the interval is published alongside every number.

**Converged** = the polled response satisfies the assertion **and** every poll for the following 5 s also satisfies it. A single satisfying poll is not convergence — flapping between a stale and a fresh entry is a distinct and worse defect than lag, and this definition catches it. **Non-convergent** = not converged within 60 s; recorded as a correctness defect, never as "60 s of lag".

**P1 — direct row (baseline).** Write: `PATCH /api/dcim/devices/{d}/ {"serial": "<probe-token>"}`. Fans out to three tables: `dcim_device`, `core_objectchange`, and `extras_cachedvalue` (`serial` is indexed at weight 60, `dcim/search.py`; the index is maintained synchronously by `post_save.connect(search_backend.caching_handler)` at `netbox/search/backends.py:303`). Poll `GET /api/dcim/devices/{d}/?exclude=config_context` through B; assert the new serial.

**P2 — config-context invalidation surface (the hard case).** All four poll `GET /api/dcim/devices/{d}/` with `config_context` **included** and assert the returned value byte-equals O2 recomputed for the post-write state. Ordered by CDC difficulty:

- **P2a** `PATCH /api/extras/config-contexts/{c}/ {"data": {...}}`. Only `extras_configcontext` changes; `dcim_device` is untouched, yet the device response must change. Invalidation must be driven by a table appearing *only inside the correlated subquery*.
- **P2b** `PATCH /api/extras/config-contexts/{c}/ {"regions": [r2]}`. Only the link table `extras_configcontext_regions` changes. Hardest case: invalidation driven by a pure M2M link table.
- **P2c** `PATCH /api/dcim/sites/{s}/ {"region": r2}`. Only `dcim_site` changes; the device's effective context changes because the MPTT range predicate now matches a different set. Transitive invalidation two joins away.
- **P2d** `POST /api/dcim/regions/` creating a child region. MPTT rebalancing UPDATEs `lft/rght` on many rows at once. Bulk invalidation; most likely to expose a per-row CDC assumption.

**P3 — blast radius, not just lag.** Under a steady 200 rps W1 workload on path B, inject one write of each class and record: hit rate before, the trough after, and time-to-recover. This substantiates or refutes "a device-heavy write mix invalidates `config_context` constantly". Publish the four recovery curves side by side; the P1-vs-P2 ratio is the headline of this section.

**Known self-inflicted invalidation.** Token auth performs `Token.objects.filter(pk=...).update(last_used=...)` at most once per 60 s per token (`netbox/netbox/api/authentication.py:94-102`) — one `users_token` write per minute, invalidating 1 of the 28 statements. Left in place for the primary workload because it is real app behaviour; a control variant pre-sets `last_used` to a future timestamp to quantify its contribution.

### 4.5 Workload sketch

Open-loop, latency from intended arrival, overflow dropped and counted, warm-up to the hit-rate knee, cold PgCache per path-B repetition. Statements/request are the **measured** values at page size 50 with the FULL FK-density profile (§6); each is re-verified per rung as an amplification regression test, and a mismatch between paths voids the latency comparison (METHODOLOGY §4).

**Primary auth = v1 API token** (`version=1`, `plaintext` set, enforced by `enforce_version_dependent_fields` at `users/models/tokens.py:127`). Auth SQL is then a stable equality select on `users_token` (`netbox/api/authentication.py:68`), removing `django_session` and its per-request timestamp literal from the shape space.

| ID | Operation | Auth | Key distribution | Measured stmt/req | Role |
|---|---|---|---|---|---|
| **W1** | `GET /api/dcim/devices/?limit=50&offset={k}` | token, superuser | Zipf s=1.1 over **K=200** fixed offsets | **28** | **primary** |
| W1b | same `+ &exclude=config_context` | token, su | same | measure; expect much less than 28 | isolates the 22-table subquery |
| W1c | same `+ &brief=true` | token, su | same | **9** | low-amplification control |
| W1w | W1 with hot fraction widened to 20% of offsets | token, su | Zipf, wide | 28 | forces the S3 knee under skew |
| **W2** | `GET /api/dcim/devices/{id}/` | token, su | Zipf s=1.1, hot set = 1% of D | **27** | per-id literal shape family |
| W2r | W2 as the restricted user | token, restricted | same | **32** | +4 permission-prefix shapes |
| **W3** | `GET /api/dcim/devices/?limit=50&offset={k}` | token, su | **uniform** over all D/50 offsets | 28 | **no-locality control + shape-space experiment** |
| W4p | `GET /ipam/prefixes/?per_page=50` | session | Zipf over 200 pages | **120** (100 = 2 shapes x 50 rows) | **highest amplification** |
| W4d | `GET /dcim/devices/?per_page=50` | session | Zipf over 200 pages | **74** (50 = 1 shape x 50 rows) | UI, per-row `primary_ip` |
| W5 | `GET /search/?q={term}` | session | Zipf over a seed-derived term dictionary | **360** (350 = 7 shapes x 50) | GFK stress; needs `extras_cachedvalue` |
| W6 | 95/5 and 99/1 read:write | token | reads = W1; writes 80% device / 15% prefix+IP / 5% config-context PATCH | — | CDC cost under write pressure |
| W7 | `GET /` dashboard | session | single page | **68** | realism, low rate |

**W3 is the load-bearing control.** Because Django/psycopg3 has no `OPTIONS`, binding is client-side and the **simple** protocol inlines literals — so the shape space grows with the *values touched*, and every prefetch `WHERE id IN (...)` list is a distinct query text per page. W1 with K=200 fixed offsets bounds the space at roughly 200 x 22 = 4,400 shapes. W3 at S3 touches D/50 = 20,000 distinct offsets, roughly 440,000 shapes. Running W1 and W3 at the same rate, on the same rung, and reporting *PgCache resident memory, distinct query texts, hit rate and p99* for both is the direct measurement of METHODOLOGY §6's known confound. A Zipf locality regression test (openFGA D-01: the generator claimed Zipf and delivered 99.4% unique keys) asserts W1 delivers at most 200 distinct offsets and that the top 10 account for at least 40% of requests.

**W4/W5/W7 require session auth**, reintroducing the `django_session` lookup whose predicate carries a client-generated timestamp — a new query text every request. Two variants, both pinned identically across A and B:
- **W4-db** (default): session in Postgres. Report the observed distinct-query-text growth rate as a first-class number; it is the honest cost of session auth under the simple protocol.
- **W4-file**: set `SESSION_FILE_PATH` in `configuration.py`, switching `SESSION_ENGINE` to the file backend (`netbox/settings.py:447-448`) and removing `django_session` from SQL entirely. A supported NetBox knob, **not** a source modification; raises the session-auth cacheable fraction from 98.6-99.2% to 100%.

**Target rate derivation.** Not guessed. Ramp path A on W1 until drop ratio reaches 1% or p99 exceeds 1 s; set the campaign rate at 60% of that. All paths and workloads run at that rate on that rung. The ramp is published as its own artifact. Drop ratio above 10% voids that run's percentiles (D-04).

**Pinned across every path and workload:** `?exclude=config_context` present or absent (never mixed within an aggregation key), `PAGINATE_COUNT=50` / `MAX_PAGE_SIZE=1000` (`netbox/config/parameters.py:135-145`), token version, `Host` header, `ALLOWED_HOSTS`, `BASE_PATH`, `SESSION_FILE_PATH`, `LOGIN_PERSISTENCE`, `RELEASE_CHECK_URL` (must be unset — the dashboard reads `latest_release` from the Django cache and a miss triggers an outbound HTTP call, `netbox/views/misc.py:61-66`).

---

## 5. Comparability knobs

Byte-identical between path A and path B except the single row marked **THE DIFFERENCE**.

### 5.1 Driver protocol and connection — the openFGA-equivalent, and the good news

`netbox/netbox/settings.py:294-297` sets only `ENGINE`. NetBox ships **no** `OPTIONS` for psycopg, so Django 6.0.7's defaults govern (verified from pinned source: `Django==6.0.7`, `psycopg[c,pool]==3.3.4`):

| Knob | Default (with evidence) | Pinned value | Why it matters |
|---|---|---|---|
| `OPTIONS['server_side_binding']` | **absent → client-side binding.** `django/db/backends/postgresql/base.py:277-284` sets `cursor_factory` to `ServerBindingCursor` only `if … server_side_binding is True`; otherwise `Cursor` = `psycopg.ClientCursor` (`base.py:577`). `psycopg/client_cursor.py:47-77`: `ClientCursorMixin._execute_send()` merges params client-side and calls `send_query(query.query)` — **the simple query protocol**. Django never enables psycopg pipeline mode. | **leave unset** | The openFGA `default_query_exec_mode` landmine, and NetBox lands on the *right* side of it by default. Every read arrives at PgCache as a complete literal SQL string. **Unlike openFGA, path B needs no protocol change**, so the openFGA confound ("B is not A-plus-a-cache, it is A-without-prepared-statements plus a cache") does not apply. Setting this `True` would silently make traffic non-cacheable. |
| `OPTIONS['prepare_threshold']` | **`None` → prepared statements disabled**, deliberately: `base.py:298-302`, comment *"Disable prepared statements by default to keep connection poolers working."* Independently `psycopg/client_cursor.py:80-83` returns `(Prepare.NO, b"")`. | **leave unset** | Any value here is a defect. |
| `OPTIONS['pool']` | absent | **leave unset** | Changes connection lifetime semantics, mutually exclusive with `CONN_MAX_AGE`; set on one path only makes A and B incomparable. |
| `CONN_MAX_AGE` | Django default **`0`** (`django/db/utils.py:168`) — new TCP connection *per request*. NetBox's example sets **`300`** (`configuration_example.py:23`). | **`300`, both paths** | At `0`, per-request connection setup dominates and costs differently against origin:5432 vs pgcache:6432. That alone could produce or erase the whole measured effect. |
| `CONN_HEALTH_CHECKS` | `False` (`django/db/utils.py:169`) | **`False`** | A `SELECT 1` per request is extra work PgCache may or may not serve. |
| `ATOMIC_REQUESTS` / `AUTOCOMMIT` | `False` / `True` (`django/db/utils.py:163-164`) | **`False` / `True`** | `ATOMIC_REQUESTS=True` is the exact condition that disqualified the previous subject. |
| `DISABLE_SERVER_SIDE_CURSORS` | absent → `.iterator()` uses named cursors (`base.py:446`, `:580-593`) | **`True`** unless exports are deliberately in the workload | `DECLARE … CURSOR` / `FETCH` is a different SQL shape. NetBox uses `.iterator()` in CSV export (`netbox/utilities/export.py:75`). |
| **DATABASE `HOST`/`PORT`** | `localhost` / blank (`configuration_example.py:21-22`) | **A → `origin:5432`; B → `pgcache:6432`** | **THE DIFFERENCE.** The only setting permitted to differ. |
| DATABASE `NAME`/`USER`/`PASSWORD` | `configuration_example.py:18-20` | identical both paths | Different roles get different `search_path` behaviour and different `pg_stat_statements` attribution. |
| `TIME_ZONE` / `USE_TZ` | `'UTC'` (`settings.py:224`) / `True` hard-coded (`:594`) | **`UTC`** / `True` | `SET TIMEZONE` differences change timestamptz text output — a false correctness divergence. |

### 5.2 Django / NetBox application settings

| Knob | Default | Pinned value | Why it matters |
|---|---|---|---|
| `DEBUG` | `False` (`settings.py:106`) | **`False`** | `True` adds `debug_toolbar` (`:510-511`), retains every SQL query in memory, flushes Redis at startup (`core/apps.py:52-56`). |
| `SECRET_KEY` | required, 50+ chars enforced (`settings.py:201`, `:239-245`) | **one identical string across all paths** | A per-path key breaks session cookies and CSRF and looks exactly like a cache defect. Also derives `DEPLOYMENT_ID` (`:718`). |
| `ALLOWED_HOSTS` | required (`settings.py:78`) | **`['netbox.lab']`**, identical, generator sends that `Host` | §4.2 depends on `request.get_host()` returning the same string on both paths for byte-identical `url` fields. |
| `BASE_PATH` | `''` | identical | Prefixes every generated URL. |
| `API_TOKEN_PEPPERS` | `{}` (`settings.py:79`), disables v2 tokens (`users/models/tokens.py:220`) | **v1 tokens exclusively** (§4.5) | v1 auth is `get(version=1, plaintext=…)`; v2 is `get(version=2, key=…)` plus HMAC-SHA256. Different SQL, different CPU. |
| `LOGIN_REQUIRED` | `True` (`settings.py:158`) | **`True`** | `False` takes a different permission path. |
| Benchmark user's **superuser** status | n/a | **pinned per workload, identical across paths** | `restrict()` returns `self` for superusers (`netbox/utilities/querysets.py:51-53`); for a non-superuser it queries `users_objectpermission` and may wrap in a `pk__in` subquery (`:56-68`). 28 vs 32 statements. The single largest lever on §1/§2. |
| `EXEMPT_VIEW_PERMISSIONS` | `[]` (`settings.py:137`) | **`[]`** | Bypasses `restrict()`. |
| `SESSION_ENGINE` / `SESSION_FILE_PATH` | Django `db` default; overridden only if `SESSION_FILE_PATH` set (`settings.py:447-448`) | **pinned per workload variant** (§4.5), identical across paths | Sessions live in Postgres by default. Switching to file storage removes a DB read; on one path only it is a total confound. |
| `LOGIN_PERSISTENCE` → `SESSION_SAVE_EVERY_REQUEST` | `False` → `False` (`settings.py:157`, `:446`) | **`False`** | `True` adds a session `UPDATE` per request plus a `set_cookie` (`middleware.py:69-83`) — CDC churn on `django_session`. |
| `LOGGING` | `{}` (`settings.py:156`) | **explicit identical dict, level `WARNING`** | An empty dict inherits Django's defaults, which differ between runserver and gunicorn. |
| `SENTRY_ENABLED` | `False` (`settings.py:209`) | **`False`** | Adds an outbound HTTP client and per-request tracing. |
| `STREAMING_EXPORTS` | `False` (`settings.py:133`) | **`False`** | `True` routes exports through `.iterator()` → named cursors. |
| `PLUGINS` / `PLUGINS_CONFIG` | `[]` / `{}` (`settings.py:164-165`) | **`[]`** | A plugin adds models, middleware (`:1005-1007`) and RQ queues. Any plugin in one path only is a total confound. |

### 5.3 NetBox dynamic configuration (`ConfigRevision`, stored in Postgres)

**Not** in `configuration.py` by default — these live in `core_configrevision`, edited through the UI, so they are seeded data and will silently differ between paths unless hard-coded. `settings.py:233-236` honours a `configuration.py` value and makes the DB value inert. **Recommendation: hard-code all of them** — removes a class of drift and removes the config Redis read from the equation.

| Knob | Default | Pinned value | Why it matters |
|---|---|---|---|
| `PAGINATE_COUNT` | `50` (`netbox/netbox/config/parameters.py:134-139`) | **`50`** | Rows per list request. The biggest determinant of per-query cost. |
| `MAX_PAGE_SIZE` | `1000` (`parameters.py:140-145`) | **`1000`** | Caps `?limit=`; a differing cap truncates the workload in one path. |
| `CHANGELOG_RETENTION` | `90` days (`parameters.py:179-185`) | **`0`** | At 90, daily housekeeping issues a large `DELETE FROM core_objectchange` (`core/jobs.py:126-174`). `0` short-circuits it (`:132-134`). |
| `CHANGELOG_RETAIN_CREATE_LAST_UPDATE` | `False` (`parameters.py:186-195`) | **`False`** | `True` adds two correlated `Exists` subqueries plus `DISTINCT ON` to the prune. |
| `JOB_RETENTION` | `90` days (`parameters.py:221-227`) | **`0`** | Kills the `DELETE FROM core_job` branch (`core/jobs.py:182-184`). |
| `MAINTENANCE_MODE` | `False` (`parameters.py:198-204`) | **`False`** | `True` emits `SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY` per request (`middleware.py:258-276`) — a session-state mutation PgCache must pass through. |
| `COPILOT_ENABLED` | **`True`** (`parameters.py:205-213`) | **`False`** | On by default. Injects an outbound `<script>` into every rendered page (`templates/base/base.html:73`). NetBox's "experimentals is not empty by default". |
| `DEFAULT_DASHBOARD` | `None` → `extras/constants.py:93-153`, which **includes an `RSSFeedWidget`** | **RSS-free layout**, or `ISOLATED_DEPLOYMENT` | If the workload hits `/`, a cache miss makes an outbound HTTPS call with a 3 s timeout (`extras/dashboard/widgets.py:365-393`) — a 3 s outlier attributed to the wrong thing. |

**One write on the read path.** `netbox/netbox/api/authentication.py:94-102`: every token-authenticated request checks `token.last_used` and, if older than 60 s, issues `Token.objects.filter(pk=…).update(last_used=timezone.now())`. An ostensibly read-only API workload writes to `users_token` about once per minute per token. **Handling:** exclude `users_token` from `ALLOWED_TABLES` (recommended), or use session auth and accept the `django_session` read. The choice changes §1's cacheable fraction and must be documented.

### 5.4 Process topology — WSGI server, worker, scheduler

NetBox ships `contrib/gunicorn.py` and the docs say to copy it verbatim (`docs/installation/4a-gunicorn.md:11-15`), so these *are* the effective defaults:

| Knob | Shipped value | Pinned value | Why it matters |
|---|---|---|---|
| gunicorn `bind` | `127.0.0.1:8001` (`contrib/gunicorn.py:2`) | per-path, see §7 | Loopback-only would make the pod unreachable. |
| gunicorn `workers` / `threads` | `5` / `3` (`contrib/gunicorn.py:6`, `:9`) | **identical, sized to the node** | workers × threads = 15 persistent connections per instance — the openFGA `MAX_OPEN_CONNS` equivalent, and it is implicit, not a named setting. |
| gunicorn `timeout` | `120` s (`contrib/gunicorn.py:12`) | **`120` or higher, identical** | openFGA's 3 s timeout turned saturation into timeouts. If a saturated path A hits even 120 s, the run measures worker kills, not latency. |
| gunicorn **`max_requests`** | **`5000`**, jitter `500` (`contrib/gunicorn.py:15-16`) | **`0` (disabled), both paths** | **The sleeper.** A worker recycles after ~5000 requests: drops persistent DB connections, loses warm state, forks fresh. At benchmark rates that fires *repeatedly inside a measurement window*, at a rate depending on throughput — **so the faster path recycles more often.** Non-deterministic, path-correlated, and it looks exactly like p99 cache-miss cost. |
| `rqworker` process | `manage.py rqworker high default low` (`contrib/netbox-rq.service:14`) | **stop during windows**, or start identically in both paths well before t0 | `core/management/commands/rqworker.py:17-28`: on **every worker start** it enqueues all system jobs with `with_scheduler=True`. `SystemHousekeepingJob` (`core/jobs.py:61`) runs census HTTP, `clear_expired_sessions()`, the changelog prune, the job prune and the release check (`:69-82`) — all DB-heavy against the shared origin. A worker restarting inside a window injects a burst of `DELETE`s that invalidates PgCache and perturbs path A. |

### 5.5 Telemetry and outbound calls

| Knob | Default | Pinned value | Why it matters |
|---|---|---|---|
| **`ISOLATED_DEPLOYMENT`** | **`False`** (`settings.py:151`) | **`True`, both paths** | The single clean switch. Suppresses the census beacon (`core/jobs.py:89-90`), the GitHub release check (`:199-200`), the RSS widget fetch (`extras/dashboard/widgets.py:365-368`), the plugin catalog fetch (`core/plugins.py:136`), and the Copilot script tag. Uncontrolled network I/O inside a latency window is exactly the noise openFGA's `PGCACHE_TELEMETRY: off` was set to kill. |
| `CENSUS_REPORTING_ENABLED` | **`True`** (`settings.py:93`) | **`False`** | On by default; belt and braces. |
| `RELEASE_CHECK_URL` | `None` (`settings.py:170`) | **`None`** | Keep it off. |

### 5.6 Metrics — and a real measurement trap

| Knob | Default | Pinned value | Why it matters |
|---|---|---|---|
| `METRICS_ENABLED` | **`False`** (`settings.py:163`) | **identical in both paths — never split** | Enabling swaps the DB `ENGINE` to `django_prometheus.db.backends.postgresql` (`settings.py:294-297`), wraps the middleware stack (`:537-543`), and mounts `django_prometheus.urls` (`netbox/urls.py:86-88`). The engine swap is protocol-safe (verified: it wraps `conn.cursor_factory` in `ExportingCursorWrapper(… Cursor …)`, a subclass of `psycopg.ClientCursor`, so client-side binding and the simple protocol survive) — but it adds per-query timer instrumentation on the hot path. |
| `PROMETHEUS_METRICS_EXPORT_PORT` / `_PORT_RANGE` | **unset** (`django_prometheus/exports.py:102-104`) | **leave unset** | If set to a range, `prometheus_client` opens one port *per gunicorn worker* — five extra host ports per path under `hostNetwork`. The openFGA gRPC-collision failure mode, multiplied by five. |
| Per-request query counting | n/a | **use `pg_stat_statements` delta ÷ request count, not `/metrics`** | With 5 workers each holding its own registry and no multiprocess dir, a `/metrics` scrape returns whichever worker answered — roughly 1/5 of the truth, non-deterministically. METHODOLOGY's "check work before latency" rule cannot be satisfied from the app's own metrics here. |

### 5.7 Origin Postgres — the platform's pinned set

Reference: `openFGA/docker-compose.yml:69-84`.

| Knob | Value | Note for NetBox |
|---|---|---|
| `wal_level` | `logical` | Required for PgCache's CDC. Without it PgCache cannot create its slot and path B never invalidates. |
| `max_wal_senders` / `max_replication_slots` | `10` / `10` | Pinned with `wal_level` as one unit. Pre-flight: assert the slot exists and `pg_replication_slots.active = true`. |
| `shared_preload_libraries` | `pg_stat_statements` | The authoritative source for §1 truth and §2 amplification, and per §5.6 the only trustworthy per-request query count. |
| `pg_stat_statements.track` / `.max` | `all` / **raise well above 10000** | NetBox's ORM emits far more distinct shapes than openFGA's five; eviction silently loses rows from the §1 census. |
| `shared_buffers` / `effective_cache_size` / `work_mem` | `1GB` / `2GB` / `32MB` | The ladder knee (§6) is defined against `shared_buffers`; `effective_cache_size` decides index-scan vs seq-scan at the S2/S3 boundary; `work_mem` too low pushes sorts to disk asymmetrically. |
| `max_connections` | `400` | Must exceed (workers × threads) + rqworker + exporter + PgCache's upstream pool. |
| `random_page_cost` / `track_io_timing` | `1.1` / `on` | Plan stability; the S3 acceptance test is I/O-based. |
| Server version | 17 | `core/checks.py:47-69` warns below 15. No extensions required. |
| Container ceiling | **real `mem_limit` below host RAM**, identical A/B; **CPU requests, never limits** on k8s | METHODOLOGY §5. A ceiling above what the host can give is not a ceiling (D-06); CPU limits produce CFS-throttled p99 spikes indistinguishable from cache behaviour. |

### 5.8 PgCache (path B only)

| Knob | Value | Why |
|---|---|---|
| `UPSTREAM_URL` / `LISTEN_PORT` / `METRICS_PORT` | `postgres://…@origin:5432/netbox` / `6432` / `9090` | |
| `ALLOWED_TABLES` | **resolved and frozen before the first path-B run**; **exclude `users_token`** (§5.3) and `core_objectchange` | Not deferrable: it changes what §1's cacheable fraction means, and two path-B runs with different values are not comparable. |
| **`DISK_LIMIT`** | **DELIBERATELY UNSET** | `openFGA/docker-compose.yml:158-161`: PgCache compares this against *total filesystem* usage, not its own cache. A "reasonable" limit on a busy host makes the cache believe it is under permanent disk pressure and **silently refuse to register every query — 100% miss with no error**. Non-negotiable. |
| `MEMORY_LIMIT` / container ceiling | unset, with a **real cgroup ceiling below host RAM** | An `mem_limit` above physical RAM is not a limit; in the openFGA lab PgCache grew until the VM died mid-window, zeroing counters. |
| `CACHE_POLICY` / `ADMISSION_THRESHOLD` / `NUM_WORKERS` | `clock` / `1` / `4`, identical across every repetition and rung | Changing these between runs makes rungs incomparable; record in `summary.json` for every run. |
| `PUBLICATION` / `SLOT` | `pgcache_netbox_pub` / `pgcache_netbox_slot` | Distinct from the openFGA lab's names. |
| `PGCACHE_TELEMETRY` | **`off`** | Uncontrolled network noise. |
| CPU ceiling | **at least the origin's** | PgCache hosts a full Postgres plus the proxy. Capped below the origin it saturates and the collapse cannot be attributed to the cache mechanism. |
| Cold start | **restart before every path-B repetition and after every re-seed** | METHODOLOGY; D-15. |

### 5.9 Redis

Mandatory infrastructure for NetBox (two logical DBs, `tasks` and `caching`, on one server — `configuration_example.py:30-63`). It is **not** path C (§3). Each path needs its **own** instance so a path-B repetition can be flushed without touching path A's state, and both must be configured identically. A shared Redis means A and B share cached config state; separate instances mean a cold one adds one `core_configrevision` read per worker. Pin the choice and state it.

---

## 6. Seeding at scale

**The API cannot be used.** NetBox's REST API creates one object per request (bulk create exists but is serializer-validated and signal-driven). S3 is ~14.5 M rows; at even 500 objects/s that is ~8 hours per rung, with every object firing counter, search-index and MPTT signals. Direct SQL is mandatory.

**But pure `COPY` is not honest either.** Several NetBox columns are derived state maintained in Python by signals or `save()` overrides. A `COPY` bypasses all of them. The design is a **hybrid**: `COPY` the bulk, then rebuild derived state using **NetBox's own commands** where they exist, and where they do not, use SQL proven equivalent to NetBox's implementation at small scale.

### 6.1 COPY targets

Column lists are the exact `local_fields` in declaration order, introspected from the model meta at the audited commit.

| Table | Columns |
|---|---|
| `dcim_device` | `id, created, last_updated, custom_field_data, owner_id, description, comments, local_context_data, config_template_id, device_type_id, role_id, tenant_id, platform_id, name, serial, asset_tag, site_id, location_id, rack_id, position, face, status, airflow, primary_ip4_id, primary_ip6_id, oob_ip_id, cluster_id, virtual_chassis_id, vc_position, vc_priority, latitude, longitude, console_port_count, console_server_port_count, power_port_count, power_outlet_count, interface_count, front_port_count, rear_port_count, device_bay_count, module_bay_count, inventory_item_count` |
| `dcim_interface` | 44 cols incl. `_name`, `_site_id`, `_location_id`, `_rack_id`, `_path_id` |
| `ipam_prefix` | `id, created, last_updated, custom_field_data, owner_id, description, comments, scope_type_id, scope_id, _location_id, _site_id, _region_id, _site_group_id, prefix, vrf_id, tenant_id, vlan_id, status, role_id, is_pool, mark_utilized, _depth, _children` |
| `ipam_ipaddress` | `id, created, last_updated, custom_field_data, owner_id, description, comments, address, vrf_id, tenant_id, status, role, assigned_object_type_id, assigned_object_id, nat_inside_id, dns_name` |
| `dcim_site`, `dcim_rack` | as introspected |
| `dcim_region`, `dcim_location`, `dcim_sitegroup`, `dcim_devicerole`, `dcim_platform`, `tenancy_tenantgroup`, `tenancy_tenant` | nested-group tables: `…, parent_id, name, slug, description, comments, lft, rght, tree_id, level` |
| `extras_configcontext` | `id, created, last_updated, data_source_id, data_file_id, data_path, auto_sync_enabled, data_synced, owner_id, name, profile_id, weight, description, is_active, data` |
| `extras_configcontext_{regions,site_groups,sites,locations,device_types,roles,platforms,cluster_types,cluster_groups,clusters,tenant_groups,tenants,tags}` | 13 link tables, `id, configcontext_id, <target>_id` |
| `extras_cachedvalue` | `id (uuid), timestamp, object_type_id, object_id, field, type, value, weight` |
| `users_token` | `id, version, user_id, description, created, expires, last_used, enabled, write_enabled, plaintext, key, pepper_id, hmac_digest, allowed_ips` |

Load order: schema-owner tables → groups → sites/racks → device types/roles/platforms → devices → interfaces → prefixes → IPs → config contexts + link tables → `extras_cachedvalue`. For S2/S3, drop non-PK indexes before `COPY` and rebuild after; set `synchronous_commit=off` and a large `maintenance_work_mem` for the load only, then restore and record both in the run artifact.

### 6.2 Post-load: derived state (mandatory, in this order)

1. **`manage.py migrate`** — before any `COPY`. The seed writes into the migrated schema; it never substitutes for migrations.

2. **MPTT nested sets.** `Region`, `SiteGroup`, `Location`, `DeviceRole`, `Platform`, `TenantGroup`, `ContactGroup`, `WirelessLANGroup` all extend `NestedGroupModel` (`netbox/netbox/models/__init__.py:162`). django-mptt ships **no** management command; use `nbshell`:
   ```
   for M in (Region, SiteGroup, Location, DeviceRole, Platform, TenantGroup, ContactGroup, WirelessLANGroup):
       M.objects.rebuild()
   ```
   These tables hold hundreds of rows, so the real implementation is used — no equivalence proof needed. **Not optional:** the `config_context` subquery's range predicates (`extras/querysets.py:146-183`) return wrong answers on an invalid nested set, and §4.3 O2 is the test that proves it.

3. **`manage.py rebuild_prefixes`** (`ipam/management/commands/rebuild_prefixes.py`) — resets `_depth=0, _children=0`, then runs the O(n) stack walk in `ipam/utils.py:201-250` per VRF plus the global table. Cross-check against `annotate_hierarchy()` (`ipam/querysets.py:226-247`) and §4.3 O3's closed form; **three-way agreement is the acceptance criterion**. Expected cost at S3 (~500 K prefixes): single-digit minutes.

4. **Prefix cached scope columns.** `_site_id/_region_id/_site_group_id/_location_id` are set by `Prefix.save()` → `CachedScopeMixin.cache_related_objects()` (`ipam/models/ip.py:340`, `dcim/models/mixins.py:108-125`) and have **no** rebuild command. The mapping is a pure function of `(scope_type, scope_id)`: region → `_region` only; site group → `_site_group` only; site → `_region = site.region, _site_group = site.group, _site = site`; location → also `_location`. Implement as one `UPDATE … FROM` per scope type, and validate by round-tripping 1,000 sampled prefixes through `Prefix.save()` in `nbshell` and asserting no column changes. *This hazard silently breaks the `?site_id=` / `?region_id=` filtersets.*

5. **`manage.py calculate_cached_counts`** (`utilities/management/commands/calculate_cached_counts.py`) — server-side `UPDATE … FROM (subquery)` per counter field (`utilities/counters.py:27-41`). Cheap; use the real command. Validated by §4.3 O5.

6. **Search index (`extras_cachedvalue`).** `manage.py reindex` (`extras/management/commands/reindex.py`) is the reference implementation but runs in Python via `bulk_create` in 2,000-row batches (`netbox/search/backends.py:199-270`) — the most expensive post-load step at S3.
   - **S0/S1: run `manage.py reindex` for real** (no `--lazy`).
   - **S2/S3: `COPY` the index**, generated with the same rule as `SearchIndex.to_cache()` — one row per indexed field per object, **skipping empty values** (`netbox/search/__init__.py:114`), with per-field weights from the indexers (`DeviceIndex`: asset_tag 50, serial 60, name 100, virtual_chassis 200, description 500, comments 5000; `PrefixIndex`: prefix 110, description 500, comments 5000).
   - **Equivalence proof, re-run whenever the generator changes:** seed a 10,000-device scratch database identically, run `manage.py reindex dcim.device ipam.prefix`, and assert the resulting `(object_type_id, object_id, field, type, value, weight)` multiset equals the generator's output. `id` (uuid4 default) and `timestamp` (`auto_now_add`) are excluded — per-row random/clock values not read by the search path.
   - **Never use `--lazy` after a re-seed.** `manage.py upgrade` calls `reindex(lazy=True)`, which *skips any model that already has entries*, leaving the previous rung's index in place. Use `manage.py upgrade --skip-reindex` and reindex explicitly, or truncate `extras_cachedvalue` first.

7. **`dcim_interface._name`.** `NaturalOrderingField` with `naturalize_interface` (`dcim/models/device_components.py:839-842`, `utilities/ordering.py:51`). `manage.py renaturalize dcim.Interface` is per-row Python and infeasible at 8 M rows. Same treatment as (6): the generator computes `_name` with a port of `naturalize_interface`, proven equivalent against `renaturalize` on the 10 K scratch database.

8. **`ANALYZE`** every loaded table. Non-negotiable: a fresh `COPY` has no statistics and the planner will choose different plans on A and B, corrupting the amplification comparison before latency is measured.

9. **`manage.py trace_paths`** — no cables are seeded, so a no-op, but run it for parity and record that it found zero paths.

10. **Cache reset between rungs.** Flush Redis and restart both app deployments after every re-seed, and **restart PgCache** (D-15) — otherwise it serves the previous dataset.

### 6.3 FK density — pinned per rung

The same endpoint measured 19 statements with sparse FKs and 28 with all FKs populated, because `prefetch_related` skips a level entirely when the parent FK set is all-NULL. **FK density is part of the rung definition, not an incidental property of the generator.**

**Profile `FULL` (used by S0–S3, all paths):**

| Field | State |
|---|---|
| `dcim_device`: `device_type_id, role_id, tenant_id, platform_id, site_id, location_id, rack_id, cluster_id, primary_ip4_id` | **100% NOT NULL** |
| `dcim_device`: `primary_ip6_id, oob_ip_id, virtual_chassis_id, config_template_id, owner_id, asset_tag, airflow` | **100% NULL** |
| `dcim_device`: `local_context_data` | non-NULL on `d % 7 == 0` (drives §4.3 O2's last merge step) |
| `dcim_device`: `serial, description` | 100% populated (drives search-index row count) |
| `ipam_prefix`: `vrf_id, tenant_id, role_id, scope_type_id/scope_id` | **100% NOT NULL**; `scope` = a Site |
| `ipam_prefix`: `status` | **`'active'` for all** — the `'container'` branch of `get_utilization()` (`ipam/models/ip.py:601-608`) emits a *different query shape*; the measured 120 stmt/req is the non-container path, and mixing statuses silently changes amplification |
| `ipam_ipaddress`: `vrf_id, tenant_id` | 100% NOT NULL; `assigned_object` NULL |
| `dcim_interface`: `device_id, type, enabled` | populated; all other FKs NULL |

**Profile `SPARSE`** exists **only** as a named control run at S1, to reproduce and publish the 19-vs-28 delta as a finding. Never mixed into an aggregation key with `FULL`.

**Amplification regression test, per rung, per path:** issue 100 W1 requests, take the `pg_stat_statements` delta ÷ 100, assert it equals the profile's expected value (28 for `FULL`, 9 for `brief`, 11 for prefixes). A mismatch means the seed shape drifted and the rung is void.

### 6.4 Scale ladder

Fixed composition per device across all rungs: **8 interfaces, 2 IP addresses, 0.5 prefixes, 1/500 site, 1/20 rack, ~3 search-index rows**. Structural entities constant at all rungs: 20 regions (4 roots × 4 leaves), 8 device roles, 8 platforms, 8 tenants, 8 device types, 8 clusters, **64 config contexts** — constant so `config_context` cost does not vary with the rung.

Origin pinned at **`shared_buffers = 1 GB`**, verified live with `SHOW shared_buffers` and recorded in every run artifact.

| Rung | Devices | Interfaces | Prefixes | IPs | `extras_cachedvalue` | Est. rows | Est. on-disk | Purpose |
|---|---|---|---|---|---|---|---|---|
| **S0 smoke** | 1,000 | 8,000 | 500 | 2,000 | 3,000 | ~15 K | ~10 MB | pipeline works at all; A-vs-A and A-vs-B gates run here first |
| **S1** | 20,000 | 160,000 | 10,000 | 40,000 | 60,000 | ~290 K | **~190 MB** | fits in `shared_buffers` (~0.2×) |
| **S2** | 200,000 | 1,600,000 | 100,000 | 400,000 | 600,000 | ~2.9 M | **~1.9 GB** | 10× S1; ~2× `shared_buffers` — the crossing point |
| **S3** | 1,000,000 | 8,000,000 | 500,000 | 2,000,000 | 3,000,000 | ~14.5 M | **~9.5 GB** | ~10× `shared_buffers` — **the knee** |

Per-row estimates behind the sizes (heap + all indexes): `dcim_device` ~850 B (42 cols, 14 FK indexes); `dcim_interface` ~850 B (44 cols, 17 FK indexes); `ipam_prefix` ~630 B (incl. the GiST `inet_ops` index, `ipam/models/ip.py:81`); `ipam_ipaddress` ~430 B; `extras_cachedvalue` ~210 B. **These are estimates. The acceptance test is what decides whether a rung is valid.**

**Acceptance test — proving S3 actually exceeds the buffer pool.** S3 is accepted only if all four pass, and the outputs go into the infrastructure artifact:

1. **Total size:** `pg_database_size(current_database())` at least **8 GB**.
2. **Hot-set size:** `SUM(pg_total_relation_size(c))` over `dcim_device, dcim_interface, ipam_prefix, ipam_ipaddress, extras_cachedvalue, extras_configcontext` at least **4 × shared_buffers**.
3. **Empirical buffer miss (the one that matters):** `pg_stat_reset()`, then run **W3 (uniform, no locality)** for 10 minutes at the campaign rate against **path A**, then require `blks_hit / (blks_hit + blks_read)` from `pg_stat_database` below **0.95**, and `heap_blks_read > 0` on `dcim_device`. If the ratio stays at or above 0.99, the rung has **not** exceeded the buffer pool regardless of what the size query says — double the device count and re-run. This is the "prove it" gate.
4. **OS page cache is a confound and must be bounded:** the origin's memory ceiling must be set below the database size (real ceiling — D-06), and the cgroup memory figure captured at run time. Without this, Linux serves the whole 9.5 GB from page cache and test 3 passes for the wrong reason.

**Expected result to state up front, so it is not mistaken for a defect:** the *primary* workload W1 uses Zipf over 1% of keys, whose working set at S3 is ~95 MB and therefore **stays resident even at S3**. The knee under skew is produced by **W1w** (hot fraction widened to 20%, ~1.9 GB hot set) and by **W3**. The rung ladder alone does not produce a knee for a skewed workload; the workload does. Every S3 report must say which of the two produced the number it shows.

---

## 7. Ports and endpoints

NetBox is a Django app, so the listening surface is the WSGI server plus its infrastructure. Three things that surprise people, all verified:

1. **`/metrics` is on the application HTTP port, not a separate one.** `django_prometheus.urls` is included into the root urlconf (`netbox/netbox/urls.py:86-88`), gated on `METRICS_ENABLED`. `django_prometheus` opens a standalone server only when `PROMETHEUS_METRICS_EXPORT_PORT[_RANGE]` is set (`django_prometheus/exports.py:102-104`), and NetBox sets neither. **Keep it that way** — a port range would open one port per gunicorn worker.
2. **The RQ worker listens on nothing.** `manage.py rqworker` is a Redis consumer plus an in-process scheduler. No socket. It still needs a per-path identity because it connects to that path's Redis *and* that path's DATABASE endpoint.
3. **Redis is mandatory and needs one instance per path** (§5.9).

| Path | Role | NetBox HTTP (UI + REST + GraphQL + `/metrics`) | **DATABASE endpoint** | Redis | Redis exporter | RQ worker | Other |
|---|---|---|---|---|---|---|---|
| **A** | baseline | `0.0.0.0:8080` | **`origin:5432`** | `6379` | `9121` | none (no listener) | — |
| **B** | pgcache | `0.0.0.0:8090` | **`pgcache:6432`** | `6380` | `9122` | none (no listener) | PgCache SQL `6432`, PgCache metrics `9090` |
| **C** | app cache | — **does not exist** (§3) | — | — | — | — | `8100` / `6381` / `9123` reserved so the chart template stays shaped like openFGA's and a future C cannot collide |

**Shared, one instance for all paths:** origin PostgreSQL `5432` (own node), postgres_exporter `9187`, Prometheus `9090` (**system pool only**), Grafana `3000`, cAdvisor `8081`.

Two collision notes. **PgCache's metrics port (`9090`) is the same number as Prometheus's**; they do not collide only because Prometheus lives on the system pool and PgCache on a lab node. Co-scheduling them is a bind failure. Second, under `hostNetwork` the migrate Job and any `manage.py` one-shot open no ports but *do* hold DB connections — run them before the window, not during.

**hostNetwork uniqueness sweep (all distinct):** 8080, 8090, 8100, 5432, 6432, 6379, 6380, 6381, 9090, 9121, 9122, 9123, 9187, 8081, 3000.

### HTTP endpoint surface on the single app port

| Path prefix | Source | Notes for the workload |
|---|---|---|
| `/api/…` | `netbox/urls.py:44-56` | The REST workload. Per-app routers under `/api/{circuits,core,dcim,extras,ipam,tenancy,users,virtualization,vpn,wireless}/` |
| `/api/status/`, `/api/authentication-check/` | `netbox/urls.py:55-56` | Cheap liveness targets that still touch the DB — do not use as a zero-cost health probe |
| `/api/schema/` | `netbox/urls.py:59-65` | Wrapped in `cache_page(86400)`. **Exclude from the workload** — served from Redis, measures nothing |
| `/graphql/` | `netbox/urls.py:70` | Gated by `GRAPHQL_ENABLED` |
| `/search/`, `/` | `netbox/urls.py:17-18` | `/` renders the default dashboard, which without `ISOLATED_DEPLOYMENT` makes an outbound RSS call (§5.3) |
| `/metrics` | `netbox/urls.py:86-88` (only when `METRICS_ENABLED`) | Per-worker registry — advisory only (§5.6) |
| `/media/<path>` | `netbox/urls.py:73` | Served through Django. Keep out of the workload |
| `/__debug__/` | `netbox/urls.py:82-84` | Only when `DEBUG=True`. If this route ever responds, the run is invalid |

---

## Open risks

1. **`?exclude=config_context` uses a substring test.** `'config_context' in request.query_params.get('exclude', [])` (`extras/api/mixins.py:48`) compares against a *string*, not a list, so it matches by substring. It works for our usage, but the generator must send the value verbatim and never a comma list whose parsing we assume.
2. **`GET /search/` may fail the A-vs-A determinism pre-test.** `CachedValue.Meta.ordering` (`extras/models/search.py:55-56`) is not provably total. If it fails, W5 becomes performance-only and is excluded from the correctness gate — a documented weakening of the highest-amplification path.
3. **Session-auth paths (W4, W5, W7) grow the shape space without bound in path B** unless `SESSION_FILE_PATH` is set. The `-db` variant may exhaust PgCache memory before the window ends; a memory ceiling and an abort-on-OOM check are required, and an OOM there must be reported as a *finding about the simple protocol*, not as a failed run.
4. **Two derived-state columns are seeded by generator code, not by NetBox** (`extras_cachedvalue`, `dcim_interface._name`) at S2/S3. The equivalence proof at 10 K rows is strong but not a proof at 8 M rows. If search or interface-ordering results ever diverge between A and B, the generator is the first suspect, not PgCache.
5. **`ipam_prefix` cached-scope columns have no rebuild command.** The seeder's SQL is our own reimplementation of `cache_related_objects()`. The 1,000-row `save()` round-trip is the only check; a drift in NetBox's implementation across versions would not be caught automatically.
6. **`config_context` blast radius may make W6 unmeasurable at 5% writes.** If P3 shows the hit rate never recovers between config-context writes, the mixed workload degrades to a measurement of PgCache's invalidation throughput rather than of caching. A legitimate result, but the report must frame it as such and re-run the write mix at 1% and 0.1%.
7. **`last_updated` is written by the app on every PATCH**, so the staleness probe's own writes change a field appearing in every serialized response. Intended (it widens the assertion surface), but P2b/P2c — which deliberately do *not* touch `dcim_device` — must assert only on the `config_context` value, never on the whole body.
8. **gunicorn `max_requests` is path-correlated noise if left at its shipped value** (§5.4). Disable it, and verify it is disabled in both paths before every campaign.
9. **No path C.** Every report must state the comparison is A vs B only, and that the staleness numbers have no native-TTL counterpart.

## Not determined from code alone

1. **Production traffic mix.** Per-request costs are measured; the real-world ratio of API:UI:search and list:detail is not known. The cacheable fraction (100% vs 98.6%) hinges entirely on the token-auth vs session-auth split. Needs an operator's access logs, or an explicit modelling decision recorded in §5.
2. **Realistic FK density and cardinality.** Amplification swung 19→28 purely from populating FKs. Real NetBox instances vary enormously. §6.3 pins a profile; whether it is *representative* is a modelling decision.
3. **Whether `?brief=true` / `?exclude=config_context` are used in the field.** They change the API list from 28 statements to 9 and change the main query from a 22-table correlated subquery to a bare `SELECT`. Which is "the" device list is a modelling decision, not a code fact.
4. **Real plan costs.** All numbers here are *statement counts*, not time. The seed was 60 rows; the config-context subquery's 18 LEFT JOINs and the prefix-utilization `inet` range scans will behave differently at S2/S3. Cost profiling belongs to Phase 3.
5. **Redis behaviour under a real deployment.** `LocMemCache` was substituted for `django_redis` during the audit (no Redis available locally). Behaviourally equivalent for the config-revision read path, but a real deployment shares one Redis across workers, so a cold Redis produces one `core_configrevision` query per worker, not per request.
6. **GraphQL.** `netbox/netbox/graphql/` was not exercised. `extras/graphql/mixins.py:32-35` shows it also attaches `annotate_config_context_data()`, so it likely mirrors the REST profile — but the Strawberry resolver fan-out is unmeasured and could contain its own N+1s.
7. **Plugin-contributed SQL.** No plugins installed. Any campaign must declare `PLUGINS = []`.
8. **LDAP / remote-auth backends.** `NBLDAPBackend.get_permission_filter` widens the ObjectPermission query (`netbox/netbox/authentication/__init__.py:320-326`), and `TokenAuthentication` calls `ldap_backend.populate_user()` per request when `FIND_GROUP_PERMS` is set (`netbox/netbox/api/authentication.py:104-114`). Untested; keep remote auth off and pin it.

## Reproduction recipe (§1/§2 measurements)

Reproducible in ~10 minutes; nothing was written into the subject tree.

- Venv (Python 3.14, Django 6.0.7): `python3 -m venv /tmp/nbvenv && /tmp/nbvenv/bin/pip install -r _sources/netbox/requirements.txt`
- Config module: `/tmp/nbconf/nbcfg.py` (Postgres on `/tmp:55432`, `API_TOKEN_PEPPERS = {1: 'p'*60}`, dummy `SECRET_KEY`)
- Postgres scratch instance: `pg_ctl -D /tmp/nbpg-88658 -o "-p 55432 -k /tmp -c shared_preload_libraries=pg_stat_statements" start`; teardown `pg_ctl -D /tmp/nbpg-88658 stop && rm -rf /tmp/nbpg-88658`
- Probe scripts: `/tmp/nbprobe.py`, `/tmp/nbsql.py`, `/tmp/nbmigrate.py`, `/tmp/nbseed.py`, `/tmp/nbmeasure.py`, `/tmp/nbshapes.py`, `/tmp/nbc2.py` (the C2 transaction check), `/tmp/nbtok.py`, `/tmp/nbfull.py`
- Invocation: `cd _sources/netbox/netbox && PYTHONPATH=/tmp/nbconf:$PWD NETBOX_CONFIGURATION=nbcfg /tmp/nbvenv/bin/python <script>`

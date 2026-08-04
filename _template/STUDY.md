# API Study — <PROJECT NAME>

> Fill this in **before writing any code**. Sections map 1:1 to
> `docs/ADDING-A-PROJECT.md` Phase 1. A study that disqualifies the subject is
> a success, not a failure — it cost a day instead of a month.

**Subject:** <app name, version/tag audited, link>
**Storage layer audited:** <file paths in the app's repo, commit hash>
**Author / date:** <who, when>

---

## 1. SQL profile of the read path

<For each read operation the app performs, one row. Get truth from
`pg_stat_statements`, intent from the code.>

| App operation | SQL emitted (shape) | Cacheable? | Why / why not |
|---|---|---|---|
| | | | |

**Cacheable fraction of read traffic:** <n%>
**Volatile exceptions** (NOW(), random(), locking reads) and the exclusion plan
for each:

- <table/query → excluded via ALLOWED_TABLES? rewritten? documented as gap?>

**Verdict:** <good subject / marginal / disqualified — one paragraph>

## 2. Query amplification

**Datastore queries per user-facing operation:** <n, and how measured — app
metric name, or pg_stat_statements delta / request count>
**Does the app batch reads, or issue them one by one?** <...>

## 3. Native cache (path C)

| Question | Answer |
|---|---|
| Does the app ship a cache? | |
| Coherence model (TTL / invalidation / none) | |
| Default state (on/off) and flags to control it | |
| What it covers / does not cover | |

**If no native cache:** comparison is A vs B only — noted in every report.

## 4. Correctness check design

**Chosen approach:** <analytical oracle / differential / none — and why>
**Sketch:** <how answers will be verified; for an oracle, how the data model is
synthesized so answers are derivable>
**Staleness probe:** <which write, which read polls it, what "converged" means>

## 5. Comparability knobs

<Every default that must be pinned identically across A/B/C. Driver protocol
mode, timeouts, pool ceilings, feature flags, experimental features. The ones
you miss here become defects later.>

| Knob | Default | Pinned value | Why it matters |
|---|---|---|---|
| | | | |

## 6. Seeding at scale

**Schema / COPY targets:** <tables, columns>
**Post-load steps:** <migrations, ANALYZE, app cache resets>
**Scale ladder:**

| Rung | Rows/entities | Approx size | Purpose |
|---|---|---|---|
| S0 smoke | | | pipeline works at all |
| S1 | | | fits in shared_buffers |
| S2 | | | ~10× S1 |
| S3 | | | exceeds shared_buffers — the knee |

## 7. Ports and endpoints

<With hostNetwork, every port must be unique per path. List ALL of them — HTTP,
metrics, gRPC, admin, anything the binary opens.>

| Path | HTTP | Metrics | Other |
|---|---|---|---|
| A | | | |
| B | | | |
| C | | | |

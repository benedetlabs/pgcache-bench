# Query-shape probe — what is cacheable, and what it is worth

**Ran:** 2026-08-04, AKS `eks-1`. Application-shaped schema: 7 tables, 463 MB,
fitting entirely inside the origin's 1 GB `shared_buffers` — the hardest case for
a cache, because the origin is already serving everything from memory. 8 clients,
`-M simple`, read-only, two paths that differ only in the host they point at.

---

## Nothing here is uncacheable

Ten query shapes, each measured on its own. **All ten reached a 100% hit ratio
and all ten beat the origin** — primary-key lookup, foreign-key listing,
`COUNT(*)`, `GROUP BY` with an aggregate, `IN (...)`, `ORDER BY … LIMIT`,
`LEFT JOIN`, a two-table join, a three-table join and a correlated `EXISTS`. The
narrowest margin was **+38%** on the primary-key lookup; the widest was **+163%**
on the foreign-key listing.

This was the cheap first step of a plan to simulate a real application: measure
each shape in isolation before composing them into requests, so that a shape the
cache cannot serve is found in twenty minutes rather than three hours into a
campaign.

| shape | origin | PgCache | gain |
|---|---:|---:|---:|
| `q01` PK lookup | 0.228 ms · 35,137 tps | 0.165 ms · 48,409 tps | +38% |
| `q03` `COUNT(*)` with WHERE | 0.241 · 33,262 | 0.162 · 49,245 | +48% |
| `q08` `ORDER BY … LIMIT 20` | 0.258 · 31,039 | 0.174 · 45,990 | +48% |
| `q06` `GROUP BY` + aggregate | 0.259 · 30,874 | 0.168 · 47,507 | +54% |
| `q07` `IN (10 ids)` | 0.261 · 30,606 | 0.182 · 44,022 | +44% |
| `q09` `LEFT JOIN` | 0.292 · 27,386 | 0.173 · 46,247 | +69% |
| `q04` 2-table join | 0.330 · 24,266 | 0.171 · 46,681 | **+92%** |
| `q10` correlated `EXISTS` | 0.392 · 20,426 | 0.176 · 45,467 | **+123%** |
| `q05` 3-table join | 0.431 · 18,560 | 0.179 · 44,709 | **+141%** |
| `q02` FK listing + LIMIT | 0.429 · 18,643 | 0.163 · 49,097 | **+163%** |

The table is ordered by the origin's throughput, cheapest at the top. Read the
gain column top to bottom: it rises all the way down. The last two rows are a tie
on the origin — 18,643 against 18,560 tps, 0.4% apart — so their relative order
means nothing, and the 22-point difference in their gains comes from PgCache's
side rather than the origin's.

---

## The mechanism, and why it lets you predict your own case

Read the PgCache latency column top to bottom: **0.162 to 0.182 ms**. Twelve per
cent between the cheapest query in the set and the most expensive one. In
throughput terms the same thing: 44,022 to 49,245 tps, a 12% spread.

Now the origin's column: **0.228 to 0.431 ms**. Eighty-nine per cent. In
throughput, 18,560 to 35,137 tps — also 89%.

> PgCache charges an essentially fixed price per answer, whether that answer is a
> primary-key lookup or a three-table join. The origin charges for the work. The
> gain is, almost exactly, how much work the query costs the origin.

That is C9 in `docs/TRIAGE-CRITERIA.md` observed directly, one shape at a time,
for the first time in this platform. It is also the useful form of the finding:
a reader does not need this table to predict their own result, only the cost of
their own query on their own origin.

**What it implies for scenario design.** A real request is built out of the
expensive shapes — joins, pagination, a `COUNT(*)` for the total. The primary-key
point select, which is where nearly every campaign this lab has run has lived, is
the **worst** case in the set, not a neutral one. Any headline drawn from a PK
lookup understates what a composed request would show.

---

## Two defects of ours that this probe exposed

Both are here because the numbers above are only worth reading if the two runs
that produced wrong numbers first are on the record too.

### 1. `helm --set` swallowed the table list, silently

The first execution returned **0.0% hit ratio on all ten shapes**, including the
primary-key lookup. That was not a result; it was configuration.

`helm --set` and `--set-string` split unescaped commas into separate assignments,
so `pgcache.allowedTables=product,category,...` was discarded whole: `helm get
values` came back empty and the deployment kept the pgbench tables. PgCache was
working perfectly and correctly refusing to cache tables nobody had allowed.

This was **already recorded as a defect** in the openFGA lab, where the same
comma parsing aborted a `helm --set` meant to apply `PINNED_TABLES` and produced
a run labelled pinned that was in fact stock. Stepping in it a second time says
the durable fix is not escaping the comma: the list belongs in `values.yaml`,
where it is recorded in the release and cannot be mistyped on a command line.

### 2. Insufficient warm-up, for the third time (D-24)

The second execution, with the tables allowed, produced a table that was
plausible and wrong: `q01` at 19.5% hit ratio and losing 42%, while `q02` reached
100% and gained 164%. The natural reading would have been "some shapes cache
better than others", and it would have been a conclusion about nothing.

It was warm-up. A single 15 s pass does not warm 20,000 distinct entries. With
two 20 s passes, the same `q01` over the same keyspace reaches **100% hit ratio
and +38%** — the row in the table above.

Capacity was ruled out rather than assumed: zero evictions, 8.4 GB used of a
26.9 GB budget. Sweeping the keyspace on its own confirms the pattern —
500, 5,000 and 20,000 keys all reach 100%, and only at 100,000 does the hit ratio
fall, to 44%.

So the second execution's ranking measured **warm-up speed**, not cacheability.
This is the defect that retracted nine cells of the openFGA r5 campaign, and it
is the third time it has appeared here.

**The rule that survives it:** no path B cell is worth anything without evidence
that its hit ratio has stabilised. A fixed warm-up pass is a guess wearing the
costume of a protocol.

---

## What this does not say

- **These are not requests yet.** They are isolated shapes. Composition — ten to
  fifteen statements of mixed shape, timed as one latency — is the next step, and
  it is where amplification multiplies the effect for both paths at once. That
  campaign is [`RESULTS-multitenant.md`](RESULTS-multitenant.md).
- **Zero writes.** This probe is pure read. Campaign
  [s4](RESULTS-aks-s4.md) measured a mix where the throughput advantage was gone
  at a 10% write ratio, and this table has to be read alongside it.
- **No correctness gate is reported here.** The shapes above — joins, `GROUP BY`,
  pagination — are precisely the ones no differential gate in this platform has
  yet exercised. The multi-tenant campaign that reuses this schema records the
  same gap and treats it as the next thing to do.
- **There is no application-level cache to compare against.** There is no
  application here at all, so this is two paths, origin and PgCache. It measures
  what PgCache can do, not whether adopting it beats what an application already
  has.

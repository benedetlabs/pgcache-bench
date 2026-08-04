# NetBox — Probe 0 (the C9 gate)

**Ran:** 2026-08-04, local Docker stack, rungs S0 (1,000 devices) and S1
(20,000 devices, 98 MB database).

**Verdict: NetBox does not pass the C9 gate.** PgCache is slower than the
uncached origin on every endpoint measured, at both rungs, stock *and* with
pinned tables and a warm cache. The full ladder is not worth running until the
caveat in "What could still overturn this" is closed.

This is the answer the gate exists to produce, and it cost one evening on a
laptop instead of three days of cluster time.

---

## What was measured

Both paths run the same NetBox image, the same code, against the same seeded
data. They differ in `DB_HOST`/`DB_PORT` and nothing else — the one difference
the protocol permits.

Method is the study's own (`STUDY.md` §2): `django.test.Client` driving the real
view stack, `CaptureQueriesContext` counting statements and summing the driver's
per-query time. Not HTTP+token, because NetBox 4.6's v2 token would not validate
through the `Authorization` header, and authentication is not what C9 asks about.

Query counts came out close to the study's: the UI prefix list issues **117**
statements here against the 120 it measured.

---

## The numbers

S1, 20,000 devices, path B with `PINNED_TABLES` set and warmed by three prior
passes. Path A measured in the same round, so the machine is the same machine.

| endpoint | path | wall | DB time | DB share | µs/query |
|---|---|---:|---:|---:|---:|
| `/api/dcim/devices/?limit=50` (13 q) | A | 91.49 ms | 25 ms | 27.3% | **1923** |
| | B | 90.34 ms | 27 ms | 29.9% | **2077** |
| `/ipam/prefixes/?per_page=50` (117 q) | A | 190.34 ms | 6 ms | 3.2% | **51.3** |
| | B | 194.41 ms | 21 ms | 10.8% | **179.5** |

PgCache's hit ratio at this point: **40.2%** (848 hits of 2,108 queries), with
pinning on and after warming.

At S0 the picture was the same shape, with the origin even cheaper — 34.2
µs/query on the UI prefix path.

---

## Two independent reasons it fails

**1. Amdahl's ceiling. The database is 3–27% of request wall time.**

Even a cache that answered instantly could not move `/ipam/prefixes/` by more
than 3%, or the device list by more than 27%. NetBox spends the rest of the
request inside Django — serialization, permission evaluation, table rendering.
No cache placed at the datastore layer reaches that time.

**2. The origin is at or below PgCache's serving floor on the cheap path.**

51.3 µs/query on the UI prefix list. PgCache's measured floor on dedicated
hardware is ~45 µs, and here it served the same queries at 179.5 µs. This is the
openFGA failure mode exactly: many cheap queries against a warm buffer pool,
where a proxy with an embedded PostgreSQL cannot be the faster path.

The 117-statement endpoint is the one the study identified as the cache's *best*
case — 100 of those statements are two repeated shapes. It is the best case, and
it still loses.

---

## Why this is the mirror image of openFGA, and what that teaches

openFGA failed C9 because its origin was **too fast to beat**: a Go service where
the datastore was most of the request, serving each query in ~40 µs.

NetBox fails it because its **application is too slow for the datastore to
matter**: the queries are more expensive in absolute terms (1923 µs on the API
path), but they are a minority of a request dominated by Python.

C9 as written only catches the first case. It needs a second half, and that is
the lesson of this probe:

> A datastore cache can only return the share of request latency that the
> datastore actually owns. Measure that share before believing any projection.
> If the application spends 90% of a request in its own code, the ceiling is 10%
> no matter how good the cache is.

---

## What could still overturn this

Stated plainly, because the conclusion above is only as good as the harness.

**The harness inflates the Python side.** `django.test.Client` runs in-process
and includes template rendering; this ran under Docker Desktop on macOS, where
the container's CPU is virtualised. A real deployment — gunicorn, on Linux, on
the AKS nodes — would make Django faster and the DB share correspondingly larger.
**This is the main threat to reason 1**, and it is testable: run the same probe
through real HTTP on the cluster.

Reason 2 does not depend on the harness. Both paths ran in the same process on
the same machine, so the A-vs-B per-query comparison holds regardless of how much
overhead the harness adds to both sides.

**Both rungs fit in `shared_buffers`.** S1 is 98 MB against 1 GB. The origin is
serving entirely from its buffer pool, which is precisely the condition that made
openFGA unwinnable. S2 (~1.9 GB) and S3 (~9.5 GB) would raise the origin's
per-query cost and could flip reason 2. They would not change reason 1.

**The hit ratio never got above 40%.** Django generates a wide space of filtered
query shapes, and pinning did not close it. If that is a configuration problem
rather than a structural one the picture changes — but nothing in the openFGA
experience suggests another knob is hiding.

---

## Recommended next step

One test, cheap, and it settles the harness question that reason 1 rests on:

Run the probe against S2 (~1.9 GB, exceeds `shared_buffers`) through **real HTTP
with gunicorn**, not the test client. That simultaneously raises the origin's
per-query cost and removes the harness's inflation of the Python side. If the DB
share stays under ~30% and PgCache stays slower, NetBox is settled and the
platform should move to the next candidate — Plane and Saleor are the remaining
`promising` entries, and both should now be screened against the amended C9
before any integration work.

What should **not** happen is tuning PgCache further against this subject. That
is the D-24 mistake, and this probe already applied its remedy — pinned tables
and a warm cache — which moved the hit ratio from 35% to 40% and changed no
verdict.

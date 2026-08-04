'use strict';

/* ───────────────────────────────────────────────────────────────────────────
 * Strapi × PgCache — R1 / R1b protocol spike client
 *
 * WHY node-postgres and not psql
 * ------------------------------
 * STUDY §5 ("Driver protocol"): Strapi is knex 3.0.1 + node-postgres 8.20.0.
 * Knex parameterizes every query and `pg` sends parameterized statements over
 * the EXTENDED protocol as unnamed Parse/Bind/Describe/Execute/Sync. There is
 * no simple-protocol switch in `pg` and knex never names statements. psql would
 * exercise the simple protocol, i.e. the wrong wire behaviour, and would answer
 * a question nobody asked.
 *
 * In this file:
 *   client.query({ text, values })  -> EXTENDED protocol (what Strapi emits)
 *   client.query('BEGIN')           -> SIMPLE protocol   (what knex emits for
 *                                      transaction control, `database/src/
 *                                      index.ts:177-183`)
 *
 * WHAT IS BEING DECIDED
 * ---------------------
 * R1   Can PgCache parse the extended query protocol at all?
 *      FAIL => Strapi is DISQUALIFIED (STUDY "Open questions" #1).
 *
 * R1b  Can PgCache SERVE a read FROM CACHE inside an explicit BEGIN…COMMIT?
 *      Every Document Service read is wrapped in a real transaction
 *      (STUDY §1; common.ts:8-10 -> database/src/index.ts:177-217).
 *      FAIL => the cacheable surface collapses to the 5 out-of-transaction
 *      auth statements A1-A4 (1 for anonymous: A5), with the adverse expected
 *      sign argued in STUDY §1.
 *
 * THE TRAP THIS FILE IS BUILT AROUND
 * ----------------------------------
 * "Did the in-transaction query succeed?" is NOT the question. It always
 * succeeds — PgCache passes through what it cannot cache, so a pass-through and
 * a cache hit are byte-identical to the client. Latency cannot separate them
 * either (a PgCache hit is itself a round-trip to an embedded Postgres, per
 * METHODOLOGY §6, and these tables are warm in the origin's shared_buffers).
 *
 * So the verdict is taken from COUNTERS, never from bytes and never from time:
 *   PRIMARY      PgCache's own `pgcache_queries_cache_hit` counter, delta'd
 *                across a measurement window, scraped from METRICS_PORT.
 *   CORROBORATING origin-side pg_stat_statements `calls` delta for that exact
 *                statement shape. A read served from cache does not reach the
 *                origin; a pass-through does.
 * The two must agree. If they disagree the spike reports INCONCLUSIVE rather
 * than picking the convenient one.
 *
 * THE CONTROLS THAT MAKE A FAIL ATTRIBUTABLE
 * ------------------------------------------
 * A bare "no hits inside the transaction" is worthless: it is equally
 * consistent with "transactions block caching" and with "this shape/config
 * never caches anything" (bad ALLOWED_TABLES, DISK_LIMIT footgun, admission
 * threshold, ...). Phases are therefore ordered so every outcome is
 * attributable:
 *
 *   P1  Q_OUT, out-of-transaction   -> R1 (correctness + extended protocol)
 *                                      and R1a (this config caches at all)
 *   P2  Q_TX,  INSIDE transaction   -> R1b-1, against a VIRGIN cache entry.
 *                                      This is the operative Strapi case: L1,
 *                                      R1, R3 only ever execute in-transaction,
 *                                      so nothing ever warms them from outside.
 *   P3  Q_TX,  out-of-transaction   -> control. Same statement, fresh
 *                                      connection, no transaction. If THIS
 *                                      caches and P2 did not, the transaction
 *                                      is the only remaining difference.
 *   P4  Q_TX,  INSIDE transaction   -> now that P3 has warmed the entry, can an
 *                                      in-transaction read SERVE it? Separates
 *                                      "cannot register in-trx" (warmable by a
 *                                      side channel — a different, higher
 *                                      ceiling) from "cannot serve in-trx"
 *                                      (total pass-through).
 *
 * Q_OUT and Q_TX are different statement TEXTS on purpose: pg_stat_statements
 * normalizes to the same queryid otherwise, and the origin-side corroboration
 * could not be attributed to a phase.
 * ─────────────────────────────────────────────────────────────────────────── */

const { Client } = require('pg');
const fs = require('fs');
const path = require('path');

// ── Config ───────────────────────────────────────────────────────────────────
const CFG = {
  user: process.env.SPIKE_PGUSER || 'spike',
  password: process.env.SPIKE_PGPASSWORD || 'spikepass',
  database: process.env.SPIKE_PGDATABASE || 'spike',
  originHost: process.env.SPIKE_ORIGIN_HOST || 'origin',
  originPort: Number(process.env.SPIKE_ORIGIN_PORT || 5432),
  pgcacheHost: process.env.SPIKE_PGCACHE_HOST || 'pgcache',
  pgcachePort: Number(process.env.SPIKE_PGCACHE_PORT || 6432),
  metricsUrl: process.env.SPIKE_METRICS_URL || 'http://pgcache:9090/metrics',
  warmTimeoutMs: Number(process.env.SPIKE_WARM_TIMEOUT_MS || 30000),
  windowIters: Number(process.env.SPIKE_WINDOW_ITERS || 25),
  iterDelayMs: Number(process.env.SPIKE_ITER_DELAY_MS || 120),
  resultPath: process.env.SPIKE_RESULT_PATH || '/out/result.json',
};

// Probe key: any id in the seeded range 1..60 (schema.sql).
const PROBE_ID = Number(process.env.SPIKE_PROBE_ID || 17);

// Two equivalent single-table PK-equality shapes, distinct TEXTS so
// pg_stat_statements can attribute origin calls per phase.
const Q_OUT = 'SELECT id, title, views FROM articles WHERE id = $1';
const Q_TX = 'SELECT id, title, slug FROM articles WHERE id = $1';
// Context only: the join shape Strapi's populate emits (STUDY §1 R5).
const Q_JOIN =
  'SELECT a.id, a.title, l.tag_id FROM articles a ' +
  'JOIN articles_tags_lnk l ON l.article_id = a.id ' +
  'WHERE a.id = $1 ORDER BY l.tag_id';

// Distinctive fragments for pg_stat_statements attribution. PgCache may deparse
// and reshape the statement it sends upstream, so we match on column names
// rather than on exact normalized text.
const FRAG_OUT = ['articles', 'views'];
const FRAG_TX = ['articles', 'slug'];

// ── Exit codes (see README) ──────────────────────────────────────────────────
const EXIT = {
  OK: 0,          // R1 PASS and R1b PASS
  R1_FAIL: 1,     // R1 FAIL  -> subject DISQUALIFIED
  INCONCLUSIVE: 2,// harness error, or evidence insufficient to decide
  R1B_FAIL: 3,    // R1 PASS, R1b FAIL -> ceiling drops; human decision required
};

// ── Small utilities ──────────────────────────────────────────────────────────
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function line(ch = '─', n = 78) { return ch.repeat(n); }

function say(...a) { console.log(...a); }

function verdictLine(id, verdict, msg) {
  const pad = id.padEnd(9);
  say(`${verdict === 'PASS' ? 'PASS' : verdict === 'FAIL' ? 'FAIL' : verdict.padEnd(4)}  ${pad}  ${msg}`);
}

// ── Prometheus scrape ────────────────────────────────────────────────────────
// metrics-exporter-prometheus 0.18 sanitizes metric names: the dots in
// `pgcache.queries.cache_hit` become underscores. Label sets are summed, which
// is correct for counters (the only thing we interpret).
const METRIC_LINE = /^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+([^\s]+)\s*$/;

async function scrapeMetrics() {
  const res = await fetch(CFG.metricsUrl, { signal: AbortSignal.timeout(5000) });
  if (!res.ok) throw new Error(`metrics scrape HTTP ${res.status} from ${CFG.metricsUrl}`);
  const text = await res.text();
  const out = new Map();
  for (const raw of text.split('\n')) {
    const l = raw.trim();
    if (!l || l.startsWith('#')) continue;
    const m = METRIC_LINE.exec(l);
    if (!m) continue;
    const v = Number(m[3]);
    if (!Number.isFinite(v)) continue;
    out.set(m[1], (out.get(m[1]) || 0) + v);
  }
  if (out.size === 0) throw new Error(`metrics endpoint returned no parsable samples (${CFG.metricsUrl})`);
  return out;
}

function metricDelta(before, after) {
  const d = new Map();
  for (const [k, v] of after) d.set(k, v - (before.get(k) || 0));
  return d;
}

function changedPgcacheCounters(delta) {
  const o = {};
  for (const [k, v] of delta) {
    if (v !== 0 && k.startsWith('pgcache_')) o[k] = v;
  }
  return o;
}

// Candidate names per logical counter, resolved against the live scrape so a
// rename in a future PgCache build degrades to INCONCLUSIVE rather than to a
// silently wrong verdict.
const CANDIDATES = {
  cacheHit: ['pgcache_queries_cache_hit', 'pgcache_queries_cache_hit_total', 'pgcache_queries_cache_hits'],
  cacheMiss: ['pgcache_queries_cache_miss', 'pgcache_queries_cache_miss_total', 'pgcache_queries_cache_misses'],
  cacheError: ['pgcache_queries_cache_error', 'pgcache_queries_cache_error_total'],
  queriesTotal: ['pgcache_queries_total', 'pgcache_queries_total_total'],
  cacheable: ['pgcache_queries_cacheable', 'pgcache_queries_cacheable_total'],
  uncacheable: ['pgcache_queries_uncacheable', 'pgcache_queries_uncacheable_total'],
  unsupported: ['pgcache_queries_unsupported', 'pgcache_queries_unsupported_total'],
  invalid: ['pgcache_queries_invalid', 'pgcache_queries_invalid_total'],
  allowlistSkipped: ['pgcache_queries_allowlist_skipped', 'pgcache_queries_allowlist_skipped_total'],
  extended: ['pgcache_protocol_extended_queries', 'pgcache_protocol_extended_queries_total'],
  simple: ['pgcache_protocol_simple_queries', 'pgcache_protocol_simple_queries_total'],
  prepared: ['pgcache_protocol_prepared_statements', 'pgcache_protocol_prepared_statements_total'],
};

function resolveNames(snapshot) {
  const resolved = {};
  const missing = [];
  for (const [logical, names] of Object.entries(CANDIDATES)) {
    const found = names.find((n) => snapshot.has(n));
    resolved[logical] = found || null;
    if (!found) missing.push(logical);
  }
  return { resolved, missing };
}

function d(delta, name) {
  return name && delta.has(name) ? delta.get(name) : 0;
}

// ── pg_stat_statements corroboration ─────────────────────────────────────────
async function pssCalls(adminClient, frags) {
  const r = await adminClient.query({
    text:
      'SELECT coalesce(sum(calls), 0)::bigint AS calls FROM pg_stat_statements ' +
      'WHERE query ILIKE \'%\' || $1 || \'%\' AND query ILIKE \'%\' || $2 || \'%\'',
    values: [frags[0], frags[1]],
  });
  return Number(r.rows[0].calls);
}

async function pssDump(adminClient) {
  const r = await adminClient.query(
    "SELECT calls, left(query, 120) AS query FROM pg_stat_statements " +
    "WHERE query ILIKE '%articles%' ORDER BY calls DESC LIMIT 12"
  );
  return r.rows;
}

// ── Query execution ──────────────────────────────────────────────────────────
async function runOnce(client, text, values, inTransaction) {
  if (!inTransaction) {
    // EXTENDED protocol: unnamed Parse/Bind/Describe/Execute/Sync.
    const r = await client.query({ text, values });
    return r.rows;
  }
  // Simple-protocol BEGIN/COMMIT wrapping an extended-protocol read — exactly
  // the shape `strapi.db.transaction()` produces (STUDY §1).
  await client.query('BEGIN');
  try {
    const r = await client.query({ text, values });
    await client.query('COMMIT');
    return r.rows;
  } catch (e) {
    try { await client.query('ROLLBACK'); } catch (_) { /* ignore */ }
    throw e;
  }
}

async function connect(host, port, label) {
  const c = new Client({
    host, port,
    user: CFG.user, password: CFG.password, database: CFG.database,
    ssl: false,
    application_name: `strapi-spike-${label}`,
    // Explicitly NOT setting `statement_timeout`/`options` so nothing extra is
    // negotiated on connect that PgCache would have to intercept.
  });
  await c.connect();
  return c;
}

// ── Phase runner ─────────────────────────────────────────────────────────────
/**
 * Warm phase: issue the statement until PgCache reports a cache hit, or until
 * the timeout. Registration + population is asynchronous inside PgCache, so a
 * single execution proves nothing either way.
 */
async function warmUntilHit(client, text, values, inTransaction, hitName) {
  const deadline = Date.now() + CFG.warmTimeoutMs;
  let iterations = 0;
  let firstRows = null;
  let firstHitAfter = null;
  while (Date.now() < deadline) {
    const before = await scrapeMetrics();
    const rows = await runOnce(client, text, values, inTransaction);
    if (firstRows === null) firstRows = rows;
    iterations += 1;
    await sleep(CFG.iterDelayMs);
    const after = await scrapeMetrics();
    if (d(metricDelta(before, after), hitName) > 0) {
      firstHitAfter = iterations;
      break;
    }
  }
  return { iterations, firstHitAfter, rows: firstRows };
}

/**
 * Measurement window: N executions, one metrics snapshot and one
 * pg_stat_statements snapshot on each side. This is where the verdict comes
 * from.
 */
async function measure(name, { client, adminClient, text, values, inTransaction, frags }) {
  const m0 = await scrapeMetrics();
  const p0 = await pssCalls(adminClient, frags);
  let rows = null;
  let error = null;
  const t0 = Date.now();
  try {
    for (let i = 0; i < CFG.windowIters; i += 1) {
      rows = await runOnce(client, text, values, inTransaction);
      await sleep(CFG.iterDelayMs);
    }
  } catch (e) {
    error = e;
  }
  const elapsedMs = Date.now() - t0;
  // CDC/metrics are updated asynchronously; give the exporter a beat.
  await sleep(500);
  const m1 = await scrapeMetrics();
  const p1 = await pssCalls(adminClient, frags);
  const delta = metricDelta(m0, m1);
  return {
    name,
    inTransaction,
    text,
    iterations: CFG.windowIters,
    elapsedMs,
    error: error ? String(error.message || error) : null,
    rows,
    originCallsDelta: p1 - p0,
    metricsDelta: delta,
    changed: changedPgcacheCounters(delta),
  };
}

function summarize(phase, N) {
  return {
    name: phase.name,
    inTransaction: phase.inTransaction,
    statement: phase.text,
    iterations: phase.iterations,
    error: phase.error,
    cacheHit: d(phase.metricsDelta, N.cacheHit),
    cacheMiss: d(phase.metricsDelta, N.cacheMiss),
    cacheError: d(phase.metricsDelta, N.cacheError),
    cacheable: d(phase.metricsDelta, N.cacheable),
    uncacheable: d(phase.metricsDelta, N.uncacheable),
    unsupported: d(phase.metricsDelta, N.unsupported),
    invalid: d(phase.metricsDelta, N.invalid),
    allowlistSkipped: d(phase.metricsDelta, N.allowlistSkipped),
    extendedProtocol: d(phase.metricsDelta, N.extended),
    simpleProtocol: d(phase.metricsDelta, N.simple),
    preparedStatements: d(phase.metricsDelta, N.prepared),
    originCallsDelta: phase.originCallsDelta,
    allChangedPgcacheCounters: phase.changed,
  };
}

function printPhase(s) {
  say(`  ${s.name}${s.inTransaction ? '  [inside BEGIN…COMMIT]' : '  [no transaction]'}`);
  say(`    statement            : ${s.statement}`);
  say(`    iterations           : ${s.iterations}${s.error ? `  (ABORTED: ${s.error})` : ''}`);
  say(`    cache_hit  delta     : ${s.cacheHit}`);
  say(`    cache_miss delta     : ${s.cacheMiss}`);
  say(`    cacheable/uncacheable: ${s.cacheable} / ${s.uncacheable}`);
  say(`    unsupported/invalid  : ${s.unsupported} / ${s.invalid}`);
  say(`    allowlist_skipped    : ${s.allowlistSkipped}`);
  say(`    extended/simple proto: ${s.extendedProtocol} / ${s.simpleProtocol}`);
  say(`    origin pg_stat calls : ${s.originCallsDelta}`);
}

const EXPECTED_OUT = [{ id: PROBE_ID, title: `art-${PROBE_ID}`, views: (PROBE_ID * 2654435761) % 1000000 }];
const EXPECTED_TX = [{ id: PROBE_ID, title: `art-${PROBE_ID}`, slug: `art-${PROBE_ID}` }];

function rowsEqual(a, b) {
  return JSON.stringify(a) === JSON.stringify(b);
}

// ── Main ─────────────────────────────────────────────────────────────────────
async function main() {
  const result = {
    spike: 'strapi/R1+R1b',
    subject: 'strapi',
    startedAt: new Date().toISOString(),
    finishedAt: null,
    config: {
      pgcacheEndpoint: `${CFG.pgcacheHost}:${CFG.pgcachePort}`,
      originEndpoint: `${CFG.originHost}:${CFG.originPort}`,
      metricsUrl: CFG.metricsUrl,
      driver: 'node-postgres (pg) 8.20.0 — the version in Strapi yarn.lock, STUDY §5',
      probeId: PROBE_ID,
      windowIters: CFG.windowIters,
      warmTimeoutMs: CFG.warmTimeoutMs,
    },
    metricNames: null,
    checks: {},
    phases: [],
    originTopStatements: null,
    exitCode: null,
    notes: [],
  };

  let origin = null, admin = null, cacheA = null, cacheB = null, cacheC = null, cacheD = null;
  let exitCode = EXIT.INCONCLUSIVE;

  try {
    say(line('═'));
    say('Strapi × PgCache — R1 / R1b spike');
    say(`  client   : node-postgres (pg) ${require('pg/package.json').version}  [extended protocol]`);
    say(`  pgcache  : ${CFG.pgcacheHost}:${CFG.pgcachePort}   metrics ${CFG.metricsUrl}`);
    say(`  origin   : ${CFG.originHost}:${CFG.originPort}`);
    say(line('═'));

    // ── Metric name resolution ───────────────────────────────────────────────
    let snap0 = null;
    let metricsUsable = true;
    try {
      snap0 = await scrapeMetrics();
    } catch (e) {
      metricsUsable = false;
      result.notes.push(`metrics endpoint unusable: ${e.message}`);
      say(`WARN  metrics endpoint unusable: ${e.message}`);
      say('      R1 can still be judged on correctness; R1b CANNOT be judged without counters.');
    }

    let N = {};
    if (metricsUsable) {
      const r = resolveNames(snap0);
      N = r.resolved;
      result.metricNames = { resolved: r.resolved, missing: r.missing };
      say('Resolved PgCache counters:');
      for (const [k, v] of Object.entries(r.resolved)) say(`  ${k.padEnd(18)} -> ${v || '(ABSENT)'}`);
      if (!r.resolved.cacheHit) {
        metricsUsable = false;
        result.notes.push(
          'No cache-hit counter found under any known name. PgCache metric naming has drifted; ' +
          'R1b cannot be decided. Scrape ' + CFG.metricsUrl + ' by hand and update CANDIDATES in check.js.'
        );
        say('WARN  no cache-hit counter present — R1b will be INCONCLUSIVE.');
      }
      say('');
    }

    // ── Connections ──────────────────────────────────────────────────────────
    origin = await connect(CFG.originHost, CFG.originPort, 'origin');
    admin = await connect(CFG.originHost, CFG.originPort, 'admin');   // pg_stat_statements reader
    cacheA = await connect(CFG.pgcacheHost, CFG.pgcachePort, 'p1');

    // ── R0 context: origin-direct behaviour ──────────────────────────────────
    // Runs FIRST so that any failure downstream can be attributed to PgCache
    // rather than to the schema, the seed, or the client.
    say(line());
    say('CONTEXT — the same two checks against the ORIGIN directly');
    say(line());
    const originOutRows = await runOnce(origin, Q_OUT, [PROBE_ID], false);
    const originTxRows = await runOnce(origin, Q_TX, [PROBE_ID], true);
    const originJoinRows = await runOnce(origin, Q_JOIN, [PROBE_ID], true);
    const originOutOk = rowsEqual(originOutRows, EXPECTED_OUT);
    const originTxOk = rowsEqual(originTxRows, EXPECTED_TX);
    verdictLine('ORIGIN-1', originOutOk ? 'PASS' : 'FAIL',
      `parameterized SELECT direct to origin returned ${originOutOk ? 'the expected row' : 'UNEXPECTED rows: ' + JSON.stringify(originOutRows)}`);
    verdictLine('ORIGIN-1b', originTxOk ? 'PASS' : 'FAIL',
      `same SELECT inside BEGIN…COMMIT direct to origin returned ${originTxOk ? 'the expected row' : 'UNEXPECTED rows: ' + JSON.stringify(originTxRows)}`);
    verdictLine('ORIGIN-J', originJoinRows.length === 3 ? 'PASS' : 'FAIL',
      `join-table SELECT direct to origin returned ${originJoinRows.length} rows (want 3)`);
    result.checks.originDirect = {
      verdict: originOutOk && originTxOk ? 'PASS' : 'FAIL',
      outOfTransactionRows: originOutRows,
      inTransactionRows: originTxRows,
      joinRowCount: originJoinRows.length,
    };
    if (!originOutOk || !originTxOk) {
      result.notes.push('Origin-direct checks failed. The schema, the seed or the client is wrong — this says NOTHING about PgCache. Fix the harness before interpreting R1/R1b.');
      say('');
      say('ABORT: the origin itself did not return the expected rows. Harness fault, not a PgCache result.');
      exitCode = EXIT.INCONCLUSIVE;
      return { exitCode, result };
    }
    say('');

    // ── R1 — extended query protocol through PgCache ─────────────────────────
    say(line());
    say('R1 — extended query protocol through PgCache');
    say(line());

    // Negative control for the protocol counters: one SIMPLE-protocol query
    // (no values array). If only `simple_queries` moves here and only
    // `extended_queries` moves below, the counters genuinely distinguish the
    // two and the R1 evidence is not an artefact.
    let simpleControl = null;
    if (metricsUsable) {
      const s0 = await scrapeMetrics();
      await cacheA.query('SELECT 1');
      await sleep(400);
      const s1 = await scrapeMetrics();
      const sd = metricDelta(s0, s1);
      simpleControl = { simple: d(sd, N.simple), extended: d(sd, N.extended) };
      say(`  protocol-counter control (one simple-protocol 'SELECT 1'):` +
          ` simple +${simpleControl.simple}, extended +${simpleControl.extended}`);
      result.checks.protocolCounterControl = simpleControl;
    }

    let r1Rows = null;
    let r1Error = null;
    try {
      r1Rows = await runOnce(cacheA, Q_OUT, [PROBE_ID], false);
    } catch (e) {
      r1Error = e;
    }

    const r1RowsOk = !r1Error && rowsEqual(r1Rows, EXPECTED_OUT);
    if (r1Error) {
      verdictLine('R1', 'FAIL', `parameterized SELECT through PgCache ERRORED: ${r1Error.message}`);
    } else if (!r1RowsOk) {
      verdictLine('R1', 'FAIL', `parameterized SELECT through PgCache returned WRONG rows: ${JSON.stringify(r1Rows)} (expected ${JSON.stringify(EXPECTED_OUT)})`);
    } else {
      verdictLine('R1', 'PASS', 'parameterized SELECT ($1) through PgCache returned the correct row over the extended protocol');
    }

    result.checks.R1 = {
      verdict: r1RowsOk ? 'PASS' : 'FAIL',
      error: r1Error ? String(r1Error.message || r1Error) : null,
      rows: r1Rows,
      expected: EXPECTED_OUT,
      meaningIfFail: 'PgCache cannot parse the extended query protocol. node-postgres has no simple-protocol switch and knex parameterizes every query (STUDY §5), so ~100% of Strapi\'s SQL would be invisible to the cache. SUBJECT DISQUALIFIED.',
    };

    if (!r1RowsOk) {
      say('');
      say('R1 FAILED — Strapi is DISQUALIFIED. Nothing downstream is worth measuring.');
      exitCode = EXIT.R1_FAIL;
      return { exitCode, result };
    }

    if (!metricsUsable) {
      say('');
      say('R1 passed on correctness, but the metrics endpoint is unusable, so R1b cannot be decided.');
      result.checks.R1b = { verdict: 'INCONCLUSIVE', reason: 'metrics endpoint unusable — no counter evidence available' };
      exitCode = EXIT.INCONCLUSIVE;
      return { exitCode, result };
    }

    // ── P1 — R1a: does this configuration cache anything at all? ─────────────
    say('');
    say(line());
    say('P1 / R1a — baseline: is this shape cacheable OUT of transaction?');
    say(line());
    const warm1 = await warmUntilHit(cacheA, Q_OUT, [PROBE_ID], false, N.cacheHit);
    say(`  warm-up: ${warm1.iterations} executions, first cache hit after ${warm1.firstHitAfter ?? 'NEVER'} `);
    const p1 = await measure('P1 Q_OUT out-of-transaction', {
      client: cacheA, adminClient: admin, text: Q_OUT, values: [PROBE_ID], inTransaction: false, frags: FRAG_OUT,
    });
    const s1 = summarize(p1, N);
    printPhase(s1);
    result.phases.push(s1);

    const r1aPass = s1.cacheHit > 0;
    verdictLine('R1a', r1aPass ? 'PASS' : 'FAIL',
      r1aPass
        ? `PgCache served ${s1.cacheHit}/${s1.iterations} out-of-transaction reads FROM CACHE — the config caches`
        : 'PgCache served ZERO reads from cache even OUT of transaction — this config caches nothing');
    result.checks.R1a = {
      verdict: r1aPass ? 'PASS' : 'FAIL',
      cacheHit: s1.cacheHit,
      cacheMiss: s1.cacheMiss,
      allowlistSkipped: s1.allowlistSkipped,
      originCallsDelta: s1.originCallsDelta,
      meaningIfFail: 'The transaction question cannot be asked yet. Suspect ALLOWED_TABLES (must list `articles`), a DISK_LIMIT that was set (openFGA/docker-compose.yml:158-161 — it is compared against TOTAL filesystem usage and silently refuses every registration), ADMISSION_THRESHOLD, or a failed replication slot (STUDY risk R1c).',
    };

    if (!r1aPass) {
      say('');
      say('R1a FAILED — cannot attribute anything to transactions. R1b is INCONCLUSIVE.');
      result.checks.R1b = {
        verdict: 'INCONCLUSIVE',
        reason: 'baseline out-of-transaction caching did not work, so an in-transaction miss is not attributable to the transaction',
      };
      exitCode = EXIT.INCONCLUSIVE;
      return { exitCode, result };
    }

    // ── P2 — R1b-1: in-transaction, VIRGIN cache entry ───────────────────────
    say('');
    say(line());
    say('P2 / R1b-1 — read INSIDE BEGIN…COMMIT, virgin cache entry (the Strapi case)');
    say(line());
    cacheB = await connect(CFG.pgcacheHost, CFG.pgcachePort, 'p2');
    const warm2 = await warmUntilHit(cacheB, Q_TX, [PROBE_ID], true, N.cacheHit);
    say(`  warm-up: ${warm2.iterations} transactions, first cache hit after ${warm2.firstHitAfter ?? 'NEVER'} `);
    const p2 = await measure('P2 Q_TX inside transaction (virgin)', {
      client: cacheB, adminClient: admin, text: Q_TX, values: [PROBE_ID], inTransaction: true, frags: FRAG_TX,
    });
    const s2 = summarize(p2, N);
    printPhase(s2);
    result.phases.push(s2);

    const txRowsOk = !p2.error && rowsEqual(p2.rows, EXPECTED_TX);
    verdictLine('R1b-rows', txRowsOk ? 'PASS' : 'FAIL',
      txRowsOk
        ? 'read inside BEGIN…COMMIT through PgCache returned the correct row'
        : `read inside BEGIN…COMMIT returned WRONG rows or errored: ${p2.error || JSON.stringify(p2.rows)}`);

    // ── P3 — control: same statement, no transaction, fresh connection ───────
    say('');
    say(line());
    say('P3 / R1b-control — the SAME statement OUT of transaction, fresh connection');
    say(line());
    cacheC = await connect(CFG.pgcacheHost, CFG.pgcachePort, 'p3');
    const warm3 = await warmUntilHit(cacheC, Q_TX, [PROBE_ID], false, N.cacheHit);
    say(`  warm-up: ${warm3.iterations} executions, first cache hit after ${warm3.firstHitAfter ?? 'NEVER'} `);
    const p3 = await measure('P3 Q_TX out-of-transaction (control)', {
      client: cacheC, adminClient: admin, text: Q_TX, values: [PROBE_ID], inTransaction: false, frags: FRAG_TX,
    });
    const s3 = summarize(p3, N);
    printPhase(s3);
    result.phases.push(s3);

    // ── P4 — R1b-2: in-transaction against a now-WARM entry ──────────────────
    say('');
    say(line());
    say('P4 / R1b-2 — read INSIDE BEGIN…COMMIT against the now-WARM entry');
    say(line());
    cacheD = await connect(CFG.pgcacheHost, CFG.pgcachePort, 'p4');
    const p4 = await measure('P4 Q_TX inside transaction (pre-warmed)', {
      client: cacheD, adminClient: admin, text: Q_TX, values: [PROBE_ID], inTransaction: true, frags: FRAG_TX,
    });
    const s4 = summarize(p4, N);
    printPhase(s4);
    result.phases.push(s4);

    // ── Verdict for R1b ──────────────────────────────────────────────────────
    say('');
    say(line('═'));
    say('R1b VERDICT');
    say(line('═'));

    const inTrxVirginHits = s2.cacheHit;
    const outTrxCtlHits = s3.cacheHit;
    const inTrxWarmHits = s4.cacheHit;

    // Corroboration: a read served from cache does not reach the origin.
    // Non-zero origin calls during a window with hits is normal (population,
    // subsumption, keepalives), so the test is directional, not exact.
    const corrobIn = s2.originCallsDelta;
    const corrobWarmIn = s4.originCallsDelta;
    const corrobOut = s3.originCallsDelta;

    let r1bVerdict, r1bMode, r1bMsg;

    if (!txRowsOk) {
      r1bVerdict = 'FAIL';
      r1bMode = 'incorrect_or_error_in_transaction';
      r1bMsg = 'PgCache did not return the correct row for a read inside BEGIN…COMMIT. This is worse than a pass-through — it is a correctness failure.';
    } else if (inTrxVirginHits > 0) {
      r1bVerdict = 'PASS';
      r1bMode = 'register_and_serve_in_transaction';
      r1bMsg = `PgCache SERVED ${inTrxVirginHits}/${s2.iterations} in-transaction reads FROM CACHE, having registered the entry entirely from inside a transaction. Strapi's whole Document Service read body is cacheable; coverage approaches 100% of read statements (STUDY §1).`;
    } else if (outTrxCtlHits > 0 && inTrxWarmHits > 0) {
      r1bVerdict = 'FAIL';
      r1bMode = 'serve_only_cannot_register_in_transaction';
      r1bMsg = `PgCache did NOT cache the statement when it was first seen inside a transaction (0/${s2.iterations} hits), but DID serve it in-transaction (${inTrxWarmHits}/${s4.iterations}) once the same statement had been registered from OUTSIDE a transaction. In-transaction reads are therefore warmable by a side channel but never self-warming. Strapi never issues L1/R1/R3 outside a transaction (STUDY §4.4), so without a deliberate out-of-band warm-up path the effective ceiling is still the 5 out-of-transaction auth statements.`;
    } else if (outTrxCtlHits > 0) {
      r1bVerdict = 'FAIL';
      r1bMode = 'passthrough_in_transaction';
      r1bMsg = `PgCache PASSED THROUGH every read inside BEGIN…COMMIT (0/${s2.iterations} virgin, 0/${s4.iterations} pre-warmed) while caching the IDENTICAL statement out of transaction (${outTrxCtlHits}/${s3.iterations}). The transaction is the only difference, so the attribution is clean.`;
    } else {
      r1bVerdict = 'INCONCLUSIVE';
      r1bMode = 'shape_not_cacheable';
      r1bMsg = `Neither the in-transaction nor the out-of-transaction execution of ${Q_TX} produced a cache hit, although ${Q_OUT} did (R1a). The statement shape, not the transaction, is the confound. Re-run with a different Q_TX before drawing any conclusion.`;
    }

    verdictLine('R1b', r1bVerdict, r1bMsg);
    say('');
    say('  Evidence:');
    say(`    in-transaction, virgin entry     : cache_hit +${inTrxVirginHits} / ${s2.iterations}   origin calls +${corrobIn}`);
    say(`    out-of-transaction, same stmt    : cache_hit +${outTrxCtlHits} / ${s3.iterations}   origin calls +${corrobOut}`);
    say(`    in-transaction, pre-warmed entry : cache_hit +${inTrxWarmHits} / ${s4.iterations}   origin calls +${corrobWarmIn}`);
    say('');
    say('  Read this as: PgCache cache_hit is the PRIMARY evidence; origin pg_stat_statements');
    say('  calls are the CORROBORATION. A read served from cache does not reach the origin.');

    // Flag disagreement between the two evidence sources rather than papering
    // over it.
    const disagreement =
      (inTrxVirginHits > 0 && corrobIn >= s2.iterations) ||
      (inTrxVirginHits === 0 && corrobIn === 0 && !p2.error);
    if (disagreement) {
      result.notes.push(
        `Evidence sources disagree in P2: cache_hit=${inTrxVirginHits} but origin calls delta=${corrobIn} ` +
        `over ${s2.iterations} iterations. Do not publish this verdict — re-run with LOG_LEVEL=debug and ` +
        'inspect PgCache logs before deciding.'
      );
      say('');
      say('  WARNING: primary and corroborating evidence disagree — see notes in result.json.');
      r1bVerdict = 'INCONCLUSIVE';
    }

    result.checks.R1b = {
      verdict: r1bVerdict,
      mode: r1bMode,
      message: r1bMsg,
      rowsCorrect: txRowsOk,
      inTransactionVirgin: { cacheHit: inTrxVirginHits, cacheMiss: s2.cacheMiss, originCallsDelta: corrobIn, iterations: s2.iterations },
      outOfTransactionControl: { cacheHit: outTrxCtlHits, cacheMiss: s3.cacheMiss, originCallsDelta: corrobOut, iterations: s3.iterations },
      inTransactionPreWarmed: { cacheHit: inTrxWarmHits, cacheMiss: s4.cacheMiss, originCallsDelta: corrobWarmIn, iterations: s4.iterations },
      ceilingIfFail: {
        statement: 'If in-transaction reads are not served from cache, the unconditionally cacheable surface is exactly the 5 out-of-transaction auth statements A1, A2, A3, A4a, A4b (1 statement — A5 — for anonymous requests). STUDY §4 and §1.',
        fractions: {
          'W1 authed findOne (7 stmts)': '5/7 = 71%',
          'W1L locale-pinned (6 stmts)': '5/6 = 83%',
          'W1a anonymous (3 stmts)': '1/3 = 33%',
          'authed list, no populate (9 stmts)': '5/9 = 56%',
          'W3 populated list (14 stmts)': '5/14 = 36%',
        },
        expectedSign: 'STUDY §1: those 5 statements are warm-buffer PK lookups on tiny near-static tables. Substituting an embedded-Postgres round-trip for a shared_buffers hit is the mechanism that sank the openFGA campaign, so the expected sign is plausibly NEGATIVE, not merely small. It can only go positive if PgCache is co-located with the app and the origin carries meaningful RTT. HUMAN DECISION REQUIRED.',
      },
    };

    result.originTopStatements = await pssDump(admin);

    exitCode = r1bVerdict === 'PASS' ? EXIT.OK
      : r1bVerdict === 'FAIL' ? EXIT.R1B_FAIL
      : EXIT.INCONCLUSIVE;

    return { exitCode, result };
  } catch (e) {
    result.notes.push(`harness error: ${String(e && e.stack || e)}`);
    say('');
    say(`HARNESS ERROR: ${String(e && e.message || e)}`);
    exitCode = EXIT.INCONCLUSIVE;
    return { exitCode, result };
  } finally {
    for (const c of [origin, admin, cacheA, cacheB, cacheC, cacheD]) {
      if (c) { try { await c.end(); } catch (_) { /* ignore */ } }
    }
  }
}

main().then(({ exitCode, result }) => {
  result.finishedAt = new Date().toISOString();
  result.exitCode = exitCode;
  try {
    fs.mkdirSync(path.dirname(CFG.resultPath), { recursive: true });
    fs.writeFileSync(CFG.resultPath, JSON.stringify(result, null, 2));
    say('');
    say(`Machine-readable result written to ${CFG.resultPath}`);
  } catch (e) {
    say(`WARN  could not write ${CFG.resultPath}: ${e.message}`);
  }
  say('');
  say(line('═'));
  say('SUMMARY');
  for (const [k, v] of Object.entries(result.checks)) {
    if (v && v.verdict) say(`  ${k.padEnd(22)} ${v.verdict}`);
  }
  say(`  exit code              ${exitCode}` +
      (exitCode === EXIT.OK ? '  (R1 PASS, R1b PASS)'
        : exitCode === EXIT.R1_FAIL ? '  (R1 FAIL — SUBJECT DISQUALIFIED)'
        : exitCode === EXIT.R1B_FAIL ? '  (R1 PASS, R1b FAIL — cacheable ceiling drops; human decision required)'
        : '  (INCONCLUSIVE — evidence insufficient; do not build on this)'));
  say(line('═'));
  process.exit(exitCode);
}).catch((e) => {
  console.error('FATAL', e);
  process.exit(EXIT.INCONCLUSIVE);
});

#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# Strapi × PgCache — R1 / R1b spike runner
#
# Brings up origin + PgCache, applies the schema, runs the node-postgres client,
# tears down, and maps the client's verdict onto an exit code.
#
#   exit 0  R1 PASS.  (R1b may be PASS or FAIL — see the banner and result.json;
#                      R1b is a scoping decision for a human, not a build gate.)
#   exit 1  R1 FAIL.  PgCache cannot parse the extended query protocol.
#                     SUBJECT DISQUALIFIED. Nothing else in Phase 2 is worth
#                     writing.
#   exit 2  INCONCLUSIVE / harness error. Evidence insufficient to decide.
#                     Do NOT build on this; fix the harness and re-run.
#
# Usage:
#   ./run.sh              run and tear down
#   ./run.sh --keep       run and leave the stack up for manual poking
#   ./run.sh --down       tear down only
#
# Runtime: ~2-4 minutes (four measurement phases, each with a bounded warm-up).
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE=(docker compose -f "${SCRIPT_DIR}/docker-compose.yml")

KEEP=0
case "${1:-}" in
  --keep) KEEP=1 ;;
  --down) "${COMPOSE[@]}" down -v --remove-orphans; exit 0 ;;
  "") ;;
  *) echo "unknown argument: $1"; exit 2 ;;
esac

ORIGIN_CT="strapi-spike-origin"
PGCACHE_CT="strapi-spike-pgcache"

log()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
warn() { printf '\033[33mWARN\033[0m %s\n' "$*"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$*"; }

dump_logs() {
  log "PgCache logs (last 120 lines)"
  "${COMPOSE[@]}" logs --tail 120 pgcache 2>&1 || true
  log "Origin logs (last 40 lines)"
  "${COMPOSE[@]}" logs --tail 40 origin 2>&1 || true
}

cleanup() {
  if [[ "${KEEP}" -eq 1 ]]; then
    log "--keep given: leaving the stack up. Tear down with: $0 --down"
  else
    log "Tearing down"
    "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

wait_healthy() {
  local ct="$1" tries="${2:-120}" status=""
  for ((i = 0; i < tries; i++)); do
    status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}nohealth{{end}}' "$ct" 2>/dev/null || echo missing)"
    case "$status" in
      healthy) return 0 ;;
      missing) sleep 2 ;;
      *) sleep 2 ;;
    esac
  done
  fail "container ${ct} never became healthy (last status: ${status})"
  return 1
}

# ── 0. Clean slate ───────────────────────────────────────────────────────────
log "Pre-clean (removing any previous spike stack and its volumes)"
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
mkdir -p "${SCRIPT_DIR}/out"
rm -f "${SCRIPT_DIR}/out/result.json"

# ── 1. Origin first, alone ───────────────────────────────────────────────────
# The schema is applied BEFORE PgCache exists, so no DDL ever lands on a table
# that is already inside PgCache's publication.
log "Starting origin Postgres 17 (wal_level=logical, slots, pg_stat_statements)"
"${COMPOSE[@]}" up -d origin
wait_healthy "${ORIGIN_CT}" 60 || { dump_logs; exit 2; }

log "Applying schema.sql"
if ! "${COMPOSE[@]}" exec -T origin psql -v ON_ERROR_STOP=1 -U spike -d spike < "${SCRIPT_DIR}/schema.sql"; then
  fail "schema application failed"
  dump_logs
  exit 2
fi

log "Origin CDC prerequisites (STUDY risk R1c)"
"${COMPOSE[@]}" exec -T origin psql -U spike -d spike -c "SHOW wal_level;" -c "SHOW max_replication_slots;" -c "SHOW max_wal_senders;"

# ── 2. PgCache ───────────────────────────────────────────────────────────────
log "Starting PgCache"
"${COMPOSE[@]}" up -d pgcache
if ! wait_healthy "${PGCACHE_CT}" 120; then
  dump_logs
  exit 2
fi

log "Replication slot / publication check (path B cannot exist without these)"
"${COMPOSE[@]}" exec -T origin psql -U spike -d spike \
  -c "SELECT slot_name, plugin, active, temporary FROM pg_replication_slots;" \
  -c "SELECT pubname FROM pg_publication;"

SLOT_ACTIVE="$("${COMPOSE[@]}" exec -T origin psql -U spike -d spike -tAc \
  "SELECT count(*) FROM pg_replication_slots WHERE slot_name = 'pgcache_spike_slot' AND active;" | tr -d '[:space:]')"
if [[ "${SLOT_ACTIVE}" != "1" ]]; then
  warn "replication slot pgcache_spike_slot is not active. CDC invalidation will not work."
  warn "R1/R1b may still be answerable, but treat any cache result with suspicion."
fi

# ── 3. Client ────────────────────────────────────────────────────────────────
log "Running the node-postgres client (R1 / R1b)"
set +e
"${COMPOSE[@]}" run --rm client
CLIENT_RC=$?
set -e

# ── 4. Verdict ───────────────────────────────────────────────────────────────
RESULT="${SCRIPT_DIR}/out/result.json"
if [[ -f "${RESULT}" ]]; then
  log "Result written to ${RESULT}"
else
  warn "no result.json produced — the client did not reach its write step"
fi

echo
case "${CLIENT_RC}" in
  0)
    printf '\033[32m'
    cat <<'BANNER'
════════════════════════════════════════════════════════════════════════════════
  R1 PASS  —  PgCache parses the extended query protocol.
  R1b PASS —  PgCache SERVES reads from cache inside explicit BEGIN…COMMIT.

  Both blocking questions resolved favourably. Strapi's whole Document Service
  read body is cacheable; coverage approaches 100% of read statements
  (strapi/STUDY.md §1). Proceed to CONTRACT.md and scenarios/.
════════════════════════════════════════════════════════════════════════════════
BANNER
    printf '\033[0m'
    exit 0
    ;;
  3)
    printf '\033[33m'
    cat <<'BANNER'
════════════════════════════════════════════════════════════════════════════════
  R1 PASS  —  PgCache parses the extended query protocol.
  R1b FAIL —  in-transaction reads are NOT served from cache.

  DECISION REQUIRED BY A HUMAN — do not proceed on autopilot.

  Every Document Service read is wrapped in a real transaction
  (STUDY §1: common.ts:8-10 -> database/src/index.ts:177-217), so the
  unconditionally cacheable surface collapses to the 5 out-of-transaction auth
  statements A1-A4 (1 statement, A5, for anonymous requests):

      W1  authed findOne  5/7  = 71%      W1L locale-pinned  5/6  = 83%
      list, no populate   5/9  = 56%      W3  populated list 5/14 = 36%
      W1a anonymous       1/3  = 33%

  Those 5 statements are warm-buffer PK lookups on tiny near-static tables.
  STUDY §1 argues the expected sign of caching them is plausibly NEGATIVE, not
  merely small — it is the exact mechanism that sank the openFGA campaign.
  It can only go positive under a high-RTT origin topology.

  See out/result.json -> checks.R1b.ceilingIfFail.
════════════════════════════════════════════════════════════════════════════════
BANNER
    printf '\033[0m'
    exit 0
    ;;
  1)
    printf '\033[31m'
    cat <<'BANNER'
════════════════════════════════════════════════════════════════════════════════
  R1 FAIL  —  PgCache cannot parse the extended query protocol.

  SUBJECT DISQUALIFIED.

  Strapi is knex 3.0.1 + node-postgres 8.20.0 (STUDY §5). node-postgres has no
  simple-protocol switch and knex parameterizes every query, so there is no
  escape hatch — ~100% of Strapi's SQL would be invisible to the cache.

  Do not write CONTRACT.md. Record the disqualification; a study that
  disqualifies its subject is a success, not a failure.
════════════════════════════════════════════════════════════════════════════════
BANNER
    printf '\033[0m'
    dump_logs
    exit 1
    ;;
  *)
    printf '\033[33m'
    cat <<'BANNER'
════════════════════════════════════════════════════════════════════════════════
  INCONCLUSIVE — the spike could not decide.

  The evidence was insufficient (metrics endpoint unusable, counter names
  drifted, the baseline out-of-transaction cache never worked, or the harness
  itself errored). Nothing may be built on this outcome.

  Read out/result.json -> notes, then the PgCache logs below.
  First suspects, in order:
    1. ALLOWED_TABLES does not list `articles` -> allowlist_skipped > 0
    2. DISK_LIMIT got set somewhere -> silent 100% miss
       (openFGA/docker-compose.yml:158-161)
    3. replication slot not active -> STUDY risk R1c
    4. PgCache metric names changed -> update CANDIDATES in client/check.js
════════════════════════════════════════════════════════════════════════════════
BANNER
    printf '\033[0m'
    dump_logs
    exit 2
    ;;
esac

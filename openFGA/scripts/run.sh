#!/usr/bin/env bash
# Executa um (degrau, workload, path). Uso:
#   scripts/run.sh E2 w1 A [rate] [duration] [warmup]
set -euo pipefail
cd "$(dirname "$0")/.."

RUNG="${1:-E0}"; WL="${2:-w1}"; PATH_ID="${3:-A}"
RATE="${4:-300}"; DUR="${5:-90s}"; WARM="${6:-30s}"; WORKERS="${WORKERS:-32}"
RUN_ID="${RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)}"

P_A="${PORT_FGA_A:-18080}"; P_B="${PORT_FGA_B:-18090}"; P_C="${PORT_FGA_C:-18100}"
M_A="${PORT_FGA_A_METRICS:-21120}"; M_B="${PORT_FGA_B_METRICS:-21121}"; M_C="${PORT_FGA_C_METRICS:-21122}"
M_PGC="${PORT_PGCACHE_METRICS:-19091}"

case "$PATH_ID" in
  A) TARGET=http://localhost:$P_A; MET=http://localhost:$M_A/metrics; PGC="" ;;
  B) TARGET=http://localhost:$P_B; MET=http://localhost:$M_B/metrics; PGC=http://localhost:$M_PGC/metrics ;;
  C) TARGET=http://localhost:$P_C; MET=http://localhost:$M_C/metrics; PGC="" ;;
  *) echo "path invalido: $PATH_ID (use A|B|C)"; exit 2 ;;
esac

OUT="results/$RUNG/$WL/$PATH_ID-$RUN_ID"
mkdir -p "$OUT"

# Digest das imagens realmente em uso. `OPENFGA_TAG=latest` nao identifica nada
# seis meses depois; o digest identifica. Gravado por execucao, para que cada
# resultado diga contra qual build ele foi produzido.
{
  echo "# imagem@digest em $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  for svc in origin pgcache openfga-a openfga-b openfga-c; do
    img="$(docker compose images -q "$svc" 2>/dev/null)" || continue
    [ -n "$img" ] || continue
    printf '%s\t%s\t%s\n' "$svc" \
      "$(docker image inspect "$img" --format '{{index .RepoTags 0}}' 2>/dev/null || echo '-')" \
      "$(docker image inspect "$img" --format '{{index .RepoDigests 0}}' 2>/dev/null || echo "sha256-local:$img")"
  done
} > "$OUT/versions.txt" 2>/dev/null || true

ARGS=(-target "$TARGET" -manifest "results/$RUNG/manifest.json" -workload "$WL"
      -rate "$RATE" -duration "$DUR" -warmup "$WARM" -path "$PATH_ID"
      -run-id "$RUN_ID" -out results -fga-metrics "$MET" -workers "$WORKERS"
      -doc-dist "${DOC_DIST:-zipf}")
[ -n "$PGC" ] && ARGS+=(-pgcache-metrics "$PGC")
# Aquecimento adaptativo: espera o joelho do hit ratio em vez de confiar num
# tempo fixo (PLAN.md secao 8.2). So' tem efeito no path B, o unico com metricas
# de cache; nos outros o fgabench cai no -warmup fixo sozinho.
[ -n "${WARMUP_MAX:-}" ] && ARGS+=(-warmup-max "$WARMUP_MAX")

# O reset do pg_stat_statements roda ENTRE o aquecimento e a medicao, via gancho
# do proprio fgabench. Zerar aqui fora, antes de chamar o binario, faz a janela
# do servidor cobrir aquecimento + medicao enquanto a do cliente cobre so' a
# medicao — as duas deixam de ser comparaveis.
ARGS+=(-post-warmup-exec
       "docker compose exec -T origin psql -U fga -d openfga -c 'SELECT pg_stat_statements_reset();' >/dev/null 2>&1 || true")

# Amostra de CPU/memoria por container DURANTE a run, com timestamp, para que a
# analise possa recortar a mesma janela que o cliente mediu (PLAN.md secao 8.5).
# A versao anterior chamava `docker stats --no-stream` uma unica vez DEPOIS do
# fgabench terminar, ou seja, media o sistema ocioso: nenhum arquivo commitado
# registra o pico de CPU que o relatorio cita.
STATS_INTERVAL="${STATS_INTERVAL:-2}"
echo "ts,name,cpu_perc,mem_usage" > "$OUT/docker-stats.csv"
(
  while :; do
    TS="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    docker stats --no-stream --format "$TS,{{.Name}},{{.CPUPerc}},{{.MemUsage}}" \
      >> "$OUT/docker-stats.csv" 2>/dev/null || true
    sleep "$STATS_INTERVAL"
  done
) &
STATS_PID=$!
trap 'kill "$STATS_PID" 2>/dev/null || true' EXIT INT TERM

./bin/fgabench "${ARGS[@]}"

kill "$STATS_PID" 2>/dev/null || true
trap - EXIT INT TERM

# Snapshot das consultas mais caras na origem, dentro da janela.
# Separador \x01 e query ate' 1000 chars: o SQL do OpenFGA passa de 160 chars e
# tem virgulas na SELECT list, entao `-F','` + left(query,160) produzia CSV
# corrompido e predicados WHERE cortados no meio.
docker compose exec -T origin psql -U fga -d openfga -A -F$'\x01' -c \
  "SELECT calls, round(total_exec_time::numeric,1) AS total_ms, round(mean_exec_time::numeric,3) AS mean_ms, rows, shared_blks_hit, shared_blks_read, regexp_replace(left(query,1000), '\s+', ' ', 'g') AS query
     FROM pg_stat_statements WHERE calls > 5 ORDER BY total_exec_time DESC LIMIT 25" \
  > "$OUT/pg_stat_statements.psv" 2>/dev/null || true

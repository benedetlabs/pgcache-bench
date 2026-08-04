#!/usr/bin/env bash
# Suite completa de um degrau: W7 (gate de correcao) -> W1/W2/W3 nos 3 paths, N repeticoes.
# Uso: scripts/suite.sh E2 [reps] [rate] [duration]
set -euo pipefail
cd "$(dirname "$0")/.."

RUNG="${1:-E0}"; REPS="${2:-3}"; RATE="${3:-300}"; DUR="${4:-90s}"
export DOC_DIST="${DOC_DIST:-zipf}"
export WARMUP_MAX="${WARMUP_MAX:-10m}"

# Containers fora do lab disputam os mesmos vCPU e contaminam a comparacao
# (PLAN.md secao 9). Na run de 25/07/2026 havia keycloak, um postgres de dev e um
# conector CDC ligados durante a medicao — visiveis no docker-stats.csv daquelas
# execucoes.
FOREIGN="$(docker ps --format '{{.Names}}' | grep -v '^fga-' || true)"
if [ -n "$FOREIGN" ]; then
  echo "AVISO: containers fora do lab rodando e competindo por CPU:"
  echo "$FOREIGN" | sed 's/^/  - /'
  echo "  Pare-os antes de medir, ou registre a contaminacao no relatorio."
  [ -n "${ALLOW_FOREIGN:-}" ] || { echo "  (ALLOW_FOREIGN=1 para prosseguir mesmo assim)"; exit 3; }
fi

echo "############ GATE DE CORRECAO (W7): A vs B ############"
./bin/fgabench -workload w7 -manifest "results/$RUNG/manifest.json" \
  -target "http://localhost:${PORT_FGA_A:-18080}" -diff-target "http://localhost:${PORT_FGA_B:-18090}" \
  -consistency MINIMIZE_LATENCY -workers 32

echo
echo "############ MEDICAO: $RUNG, $REPS repeticoes ############"
for rep in $(seq 1 "$REPS"); do
  for WL in w1 w2 w3; do
    for P in A B C; do
      echo "--- rep $rep · $RUNG · $WL · path $P ---"
      # Cada execucao do path B parte de cache FRIO. Sem isto a repeticao 2
      # herda as shapes registradas na 1 e mede um aquecimento que nao esta na
      # janela — vies a favor do PgCache, e as repeticoes deixam de ser
      # independentes (PLAN.md secao 8.1).
      if [ "$P" = "B" ]; then
        docker compose restart pgcache >/dev/null
        for _ in $(seq 1 60); do
          docker compose exec -T pgcache pg_isready -h 127.0.0.1 -p 6432 -U fga -d openfga >/dev/null 2>&1 && break
          sleep 2
        done
      fi
      RUN_ID="r${rep}-$(date -u +%H%M%S)" ./scripts/run.sh "$RUNG" "$WL" "$P" "$RATE" "$DUR" 30s
      sleep 5
    done
  done
done
echo "############ CONCLUIDO: results/all-runs.csv ############"

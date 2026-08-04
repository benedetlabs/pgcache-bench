#!/usr/bin/env bash
# Semeia um degrau da escada. Uso: scripts/seed.sh E2
#
# Ordem importa: paramos o PgCache ANTES do COPY. Um COPY de 10M linhas gera um
# volume de WAL que o consumidor logico teria de digerir sem necessidade — e o
# cache precisa comecar vazio de qualquer forma. Subimos o PgCache depois.
set -euo pipefail
cd "$(dirname "$0")/.."

RUNG="${1:-E0}"
PSQL="docker compose exec -T origin psql -v ON_ERROR_STOP=1 -U fga -d openfga"
FGA_A="${FGA_A:-http://localhost:${PORT_FGA_A:-18080}}"

echo "==> [$RUNG] parando PgCache e paths dependentes"
docker compose stop pgcache openfga-b >/dev/null 2>&1 || true

echo "==> [$RUNG] limpando massa anterior"
$PSQL -c "DELETE FROM tuple; DELETE FROM changelog; DELETE FROM assertion; DELETE FROM authorization_model; DELETE FROM store;" >/dev/null

echo "==> [$RUNG] criando store + authorization model"
mkdir -p results/"$RUNG"
./bin/fgaseed bootstrap -rung "$RUNG" -fga "$FGA_A" -model model/model.json \
  -manifest "results/$RUNG/manifest.json"

STORE=$(python3 -c "import json;print(json.load(open('results/$RUNG/manifest.json'))['store_id'])")
NTUP=$(python3 -c "import json;print(json.load(open('results/$RUNG/manifest.json'))['tuples'])")
echo "==> [$RUNG] store=$STORE  tuplas=$NTUP"

echo "==> [$RUNG] gerando e carregando tuplas via COPY"
START=$(date +%s)
./bin/fgaseed tuples -rung "$RUNG" -store "$STORE" \
  | $PSQL -c "\copy tuple(store,object_type,object_id,relation,_user,user_type,ulid,inserted_at,condition_name,condition_context) FROM STDIN"
echo "==> [$RUNG] COPY concluido em $(( $(date +%s) - START ))s"

echo "==> [$RUNG] ANALYZE + estatisticas"
$PSQL -c "VACUUM (ANALYZE) tuple;" >/dev/null
$PSQL -c "SELECT count(*) AS tuplas, pg_size_pretty(pg_total_relation_size('tuple')) AS tamanho FROM tuple;"
$PSQL -c "SELECT user_type, count(*) FROM tuple GROUP BY 1 ORDER BY 2 DESC;"

echo "==> [$RUNG] subindo PgCache (cache frio, por construcao)"
docker compose up -d pgcache >/dev/null
for i in $(seq 1 60); do
  if docker compose exec -T pgcache pg_isready -h 127.0.0.1 -p 6432 -U fga -d openfga >/dev/null 2>&1; then
    echo "==> [$RUNG] PgCache pronto"; break
  fi
  sleep 2
done
docker compose up -d openfga-b >/dev/null
sleep 5

echo "==> [$RUNG] validando a massa contra o oraculo analitico (path A)"
./bin/fgaseed verify -manifest "results/$RUNG/manifest.json" -fga "$FGA_A" -samples 300

echo "==> [$RUNG] pronto."

#!/usr/bin/env bash
# Campanha autonoma de benchmark no AKS.
#
#   ./scripts/campaign-aks.sh            roda tudo
#   ./scripts/campaign-aks.sh phase1     roda so' uma fase
#
# Dirige o pod `loadgen` de fora, via kubectl exec. Roda de fora e nao dentro do
# pod porque precisa reiniciar o Deployment do PgCache entre repeticoes (cache
# frio por repeticao) e trocar o networkMode via helm — duas coisas que exigiriam
# RBAC dentro do pod.
#
# AS MEDICOES SAO SEQUENCIAIS POR CONSTRUCAO. A regra de ouro do lab e' um path
# sob carga por vez (PLAN.md secao 8): dois caminhos medidos ao mesmo tempo
# competem por CPU e nenhum dos dois numeros vale. Paralelizar aqui nao acelera
# nada — corrompe.
set -uo pipefail
cd "$(dirname "$0")/.."

NS="${NS:-pgcache-lab}"
CHART="${CHART:-./infra/aks/chart}"
RATE="${RATE:-40}"
DUR="${DUR:-60s}"
WARM="${WARM:-45s}"
REPS="${REPS:-3}"
WORKERS="${WORKERS:-32}"

DOCS="benchmark-docs"
RUNS="$DOCS/runs"
mkdir -p "$RUNS"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
LOG="$RUNS/campaign-$STAMP.log"

say() { echo "[$(date -u +%H:%M:%SZ)] $*" | tee -a "$LOG"; }
pod() { kubectl -n "$NS" exec deploy/loadgen -- bash -c "$1" 2>&1; }

# ── Deteccao de contaminacao ────────────────────────────────────────────────
#
# O userpool e' Spot com eviccao Deallocate. Uma eviccao no meio de uma janela
# invalida a run, do mesmo jeito que o reinicio do PgCache zerou os contadores no
# lab local. Registramos o que aconteceu; a analise decide o que descartar.
check_health() {
  local tag="$1"
  local ev notready
  ev=$(kubectl -n "$NS" get events --sort-by=.lastTimestamp 2>/dev/null \
        | grep -iE 'preempt|evict|oomkill|NodeNotReady' | tail -5)
  if [ -n "$ev" ]; then
    say "!! CONTAMINACAO POSSIVEL apos $tag:"
    echo "$ev" | tee -a "$LOG"
  fi
  notready=$(kubectl -n "$NS" get pods --no-headers 2>/dev/null | grep -cvE '1/1 *Running')
  [ "${notready:-0}" -gt 0 ] && say "!! $notready pod(s) fora de 1/1 Running apos $tag"
  return 0
}

# PgCache frio a cada repeticao do path B: sem isto a repeticao 2 herda as shapes
# registradas na 1 e mede um aquecimento que nao esta na janela.
cold_pgcache() {
  kubectl -n "$NS" rollout restart deploy/pgcache >/dev/null 2>&1
  kubectl -n "$NS" rollout status deploy/pgcache --timeout=180s >/dev/null 2>&1
  sleep 10
}

# Os resultados vivem num emptyDir do pod loadgen: somem se o pod for recriado.
# A primeira campanha perdeu 40 execucoes exatamente assim — a fase 3 deleta o
# pod para trocar o networkMode, e levou junto tudo das fases 1 e 2.
# Chamado ao fim de cada fase E antes de qualquer coisa que recrie o pod.
snapshot() {
  local tag="$1" dest="$DOCS/results-aks-$tag"
  mkdir -p "$dest"
  local lp
  lp=$(kubectl -n "$NS" get pod -l app=loadgen -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  [ -n "$lp" ] && kubectl -n "$NS" cp "$lp:/lab/results" "$dest" >/dev/null 2>&1
  local n
  n=$(find "$dest" -name summary.json 2>/dev/null | wc -l | tr -d ' ')
  say "-- snapshot '$tag': $n execucoes salvas em $dest"
}

# Repovoa o pod do gerador: o repositorio vive num emptyDir e some sempre que o
# pod e' recriado (helm upgrade, perda de no' Spot). Os RESULTADOS ficam no PVC e
# sobrevivem — e' so' o codigo que precisa voltar. Idempotente: barato chamar
# antes de cada fase.
bootstrap_pod() {
  kubectl -n "$NS" rollout status deploy/loadgen --timeout=300s >/dev/null 2>&1
  if pod "test -x /lab/bin/fgabench && command -v psql" >/dev/null 2>&1; then
    return 0
  fi
  say "-- repovoando o pod do gerador"
  tar --exclude=results --exclude=bin --exclude=report --exclude=.git \
      --exclude=benchmark-docs -czf /tmp/lab-boot.tgz \
      Makefile PLAN.md README.md docker-compose.yml model scripts tools docs 2>/dev/null
  kubectl -n "$NS" exec -i deploy/loadgen -- tar xzf - -C /lab < /tmp/lab-boot.tgz 2>/dev/null
  pod "cd /lab/tools && go build -o ../bin/fgabench ./cmd/fgabench && go build -o ../bin/fgaseed ./cmd/fgaseed" >/dev/null 2>&1
  pod "command -v psql >/dev/null || (apt-get update -qq && apt-get install -y -qq postgresql-client)" >/dev/null 2>&1
  pod "test -x /lab/bin/fgabench && command -v psql >/dev/null && echo BOOTSTRAP_OK" | tail -1 | tee -a "$LOG"
}

wait_ready() {
  kubectl -n "$NS" wait --for=condition=ready pod -l app=origin --timeout=600s >/dev/null 2>&1
  for d in pgcache openfga-a openfga-b openfga-c; do
    kubectl -n "$NS" rollout status deploy/$d --timeout=300s >/dev/null 2>&1
  done
}

one_run() {
  local rung="$1" wl="$2" p="$3" runid="$4" dist="${5:-zipf}"
  local tvar mvar pgflag
  case "$p" in
    A) tvar='$FGA_A'; mvar='$FGA_A_METRICS'; pgflag='' ;;
    B) tvar='$FGA_B'; mvar='$FGA_B_METRICS'; pgflag='-pgcache-metrics $PGCACHE_METRICS' ;;
    C) tvar='$FGA_C'; mvar='$FGA_C_METRICS'; pgflag='' ;;
  esac

  [ "$p" = B ] && cold_pgcache

  say "--- $rung/$wl/$p run=$runid dist=$dist rate=$RATE"
  pod "cd /lab && ./bin/fgabench -target $tvar -manifest results/$rung/manifest.json \
       -workload $wl -rate $RATE -duration $DUR -warmup $WARM -path $p \
       -run-id $runid -out results -fga-metrics $mvar $pgflag \
       -workers $WORKERS -doc-dist $dist" | grep -vE '^\[' | tee -a "$LOG"
  check_health "$rung/$wl/$p/$runid"
  # Snapshot por RUN, nao por fase: com PVC os dados ja' sobrevivem ao pod,
  # mas isto tira uma copia para fora do cluster a cada execucao.
  snapshot corrente >/dev/null 2>&1
}

seed_rung() {
  local rung="$1"
  say "==== SEED $rung ===="
  pod "cd /lab && export PGPASSWORD=fgapass && \
    PSQL='psql -v ON_ERROR_STOP=1 -h origin.$NS.svc.cluster.local -U fga -d openfga' && \
    \$PSQL -c 'DELETE FROM tuple; DELETE FROM changelog; DELETE FROM assertion; DELETE FROM authorization_model; DELETE FROM store;' >/dev/null && \
    mkdir -p results/$rung && \
    ./bin/fgaseed bootstrap -rung $rung -fga \$FGA_A -model model/model.json -manifest results/$rung/manifest.json && \
    STORE=\$(grep -o '\"store_id\": \"[^\"]*\"' results/$rung/manifest.json | cut -d'\"' -f4) && \
    ./bin/fgaseed tuples -rung $rung -store \$STORE | \$PSQL -c '\copy tuple(store,object_type,object_id,relation,_user,user_type,ulid,inserted_at,condition_name,condition_context) FROM STDIN' && \
    \$PSQL -c 'VACUUM (ANALYZE) tuple;' >/dev/null && echo SEED_OK" | tail -6 | tee -a "$LOG"

  # O seed.sh original PARA o PgCache antes do COPY: o consumidor logico nao tem
  # por que digerir o WAL de uma carga em massa, e o cache precisa comecar vazio
  # de qualquer forma. Portar sem este passo deixou o PgCache servindo a massa
  # ANTERIOR — todo Check pelo path B estourava o deadline de 60 s.
  say "-- reiniciando PgCache (cache frio pos-seed) e openfga-b"
  cold_pgcache
  kubectl -n "$NS" rollout restart deploy/openfga-b >/dev/null 2>&1
  kubectl -n "$NS" rollout status deploy/openfga-b --timeout=300s >/dev/null 2>&1
  sleep 15

  say "-- validando massa contra o oraculo"
  pod "cd /lab && ./bin/fgaseed verify -manifest results/$rung/manifest.json -fga \$FGA_A -samples 300" | tail -5 | tee -a "$LOG"

  say "-- gate de correcao W7 (A vs B)"
  pod "cd /lab && ./bin/fgabench -workload w7 -manifest results/$rung/manifest.json \
       -target \$FGA_A -diff-target \$FGA_B -consistency MINIMIZE_LATENCY -workers 32" | tail -8 | tee -a "$LOG"
}

matrix() {                                  # rung "wl1 wl2" dist sufixo
  local rung="$1" wls="$2" dist="$3" sfx="$4"
  for rep in $(seq 1 "$REPS"); do
    for wl in $wls; do
      for p in A B C; do
        one_run "$rung" "$wl" "$p" "${sfx}r${rep}" "$dist"
        sleep 5
      done
    done
  done
}

phase1() {   # E1, rede bare, os tres workloads de Check
  say "######## FASE 1 — E1 · networkMode=bare · W1/W2/W3 · $REPS reps ########"
  helm upgrade lab "$CHART" -n "$NS" --set networkMode=bare >/dev/null 2>&1
  wait_ready
  bootstrap_pod
  seed_rung E1
  matrix E1 "w1 w2 w3" zipf "bare-"
  snapshot fase1-e1-bare
}

phase2() {   # controle: eixo documento sem localidade, reproduz o modo legado
  say "######## FASE 2 — E1 · bare · doc-dist=uniform (controle) ########"
  bootstrap_pod
  matrix E1 "w1 w3" uniform "unif-"
  snapshot fase2-e1-uniform
}

phase3() {   # o custo da camada de rede do Kubernetes
  say "######## FASE 3 — E1 · networkMode=cni ########"
  snapshot antes-de-recriar-loadgen     # o delete abaixo apaga o emptyDir
  helm upgrade lab "$CHART" -n "$NS" --set networkMode=cni >/dev/null 2>&1
  kubectl -n "$NS" rollout restart deploy/loadgen >/dev/null 2>&1
  kubectl -n "$NS" rollout status deploy/loadgen --timeout=300s >/dev/null 2>&1
  helm upgrade lab "$CHART" -n "$NS" --set networkMode=cni >/dev/null 2>&1
  sleep 30; wait_ready
  bootstrap_pod
  seed_rung E1
  matrix E1 "w1 w3" zipf "cni-"
  snapshot fase3-e1-cni
  say "-- voltando para bare"
  helm upgrade lab "$CHART" -n "$NS" --set networkMode=bare >/dev/null 2>&1
  sleep 30; wait_ready
}

phase4() {   # o degrau seguinte da escada
  say "######## FASE 4 — E2 (1,0 M tuplas) · bare ########"
  seed_rung E2
  matrix E2 "w1 w3" zipf "bare-"
  snapshot fase4-e2-bare
}

phase5() {   # rampa: onde cada caminho para de sustentar a taxa
  say "######## FASE 5 — rampa de taxa em E1 (W1) ########"
  bootstrap_pod
  for r in 20 80 160 320; do
    for p in A B C; do
      RATE=$r one_run E1 w1 "$p" "ramp${r}" zipf
      sleep 5
    done
  done
  snapshot fase5-rampa
}

say "CAMPANHA INICIADA — namespace $NS, reps=$REPS, rate=$RATE, dur=$DUR"
kubectl -n "$NS" get pods -o wide 2>&1 | tee -a "$LOG"

case "${1:-all}" in
  all)    phase1; phase2; phase3; phase4; phase5 ;;
  faltantes) phase1; phase2; phase5 ;;
  phase1) phase1 ;;
  phase2) phase2 ;;
  phase3) phase3 ;;
  phase4) phase4 ;;
  phase5) phase5 ;;
  *) echo "uso: $0 [all|faltantes|phase1..phase5]"; exit 2 ;;
esac

say "==== CAMPANHA CONCLUIDA ===="
say "-- extraindo resultados do pod"
kubectl -n "$NS" cp "loadgen:/lab/results" "$DOCS/results-aks" 2>/dev/null
kubectl -n "$NS" get events --sort-by=.lastTimestamp > "$RUNS/events-$STAMP.txt" 2>&1
say "-- artefatos em $DOCS/results-aks e $RUNS"

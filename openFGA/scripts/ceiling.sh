#!/usr/bin/env bash
# W6 — rampa de taxa ate' o p99 estourar o SLO. Reporta o teto por path.
# Uso: scripts/ceiling.sh E2 [slo_ms]
set -euo pipefail
cd "$(dirname "$0")/.."
RUNG="${1:-E0}"; SLO="${2:-200}"
RATES="${RATES:-100 200 400 800 1600 3200 6400}"

for P in A B C; do
  echo "===== teto do path $P (SLO p99 <= ${SLO}ms) ====="
  CEIL=0
  for R in $RATES; do
    RUN_ID="ceil-$R" ./scripts/run.sh "$RUNG" w1 "$P" "$R" 45s 15s >/dev/null 2>&1 || true
    S="results/$RUNG/w1/$P-ceil-$R/summary.json"
    [ -f "$S" ] || { echo "  $R rps -> sem resultado"; continue; }
    P99=$(python3 -c "import json;print(json.load(open('$S'))['p99_ms'])")
    ACH=$(python3 -c "import json;print(round(json.load(open('$S'))['rate_achieved_rps']))")
    ERR=$(python3 -c "import json;print(json.load(open('$S'))['errors'])")
    printf "  alvo %5s rps -> alcancado %5s rps, p99 %8.2f ms, erros %s\n" "$R" "$ACH" "$P99" "$ERR"
    if python3 -c "import sys;sys.exit(0 if $P99 <= $SLO else 1)"; then CEIL=$R; else
      echo "  >>> SLO estourado em $R rps. Teto do path $P: $CEIL rps"; break
    fi
  done
done

SHELL := /bin/bash
.PHONY: help build up down seed verify smoke suite ceiling report clean logs

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "};{printf "  \033[36m%-14s\033[0m %s\n",$$1,$$2}'

build:   ## compila fgaseed e fgabench para ./bin
	cd tools && go build -o ../bin/fgaseed ./cmd/fgaseed && go build -o ../bin/fgabench ./cmd/fgabench
	@echo "binarios em ./bin"

test:    ## roda os testes do oraculo (obrigatorio antes de medir)
	cd tools && go test ./... -count=1

up:      ## sobe a stack (origin, migrate, pgcache, 3x openfga, observabilidade)
	docker compose up -d
	@echo "Grafana http://localhost:$(or $(PORT_GRAFANA),13001) · Prometheus http://localhost:$(or $(PORT_PROM),19090)"

down:    ## derruba a stack (mantem volumes)
	docker compose down

nuke:    ## derruba e apaga volumes
	docker compose down -v

seed:    ## make seed RUNG=E2
	./scripts/seed.sh $(or $(RUNG),E0)

verify:  ## make verify RUNG=E2 — confere a massa contra o oraculo
	./bin/fgaseed verify -manifest results/$(or $(RUNG),E0)/manifest.json -fga http://localhost:$(or $(PORT_FGA_A),18080) -samples 500

smoke:   ## gate rapido: E0 semeado + W7 + W1 curto nos 3 paths
	$(MAKE) build && $(MAKE) test && $(MAKE) seed RUNG=E0
	./bin/fgabench -workload w7 -manifest results/E0/manifest.json \
	  -target http://localhost:$(or $(PORT_FGA_A),18080) -diff-target http://localhost:$(or $(PORT_FGA_B),18090) -workers 16
	for P in A B C; do ./scripts/run.sh E0 w1 $$P 200 30s 10s; done

suite:   ## make suite RUNG=E2 REPS=3 RATE=300 DUR=90s
	./scripts/suite.sh $(or $(RUNG),E0) $(or $(REPS),3) $(or $(RATE),300) $(or $(DUR),90s)

ceiling: ## make ceiling RUNG=E3 — rampa ate' quebrar o SLO p99<=200ms
	./scripts/ceiling.sh $(or $(RUNG),E2)

report:  ## gera HTML + Markdown + CSV consolidado a partir de results/
	python3 scripts/report.py --results results --out report

logs:
	docker compose logs -f --tail=80 pgcache openfga-b

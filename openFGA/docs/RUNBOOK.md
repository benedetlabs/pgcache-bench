# Runbook — arquitetura, execução manual e leitura dos resultados

Este é o documento **operacional**: como o benchmark é montado, como executá-lo
passo a passo e como interpretar o que ele produz.

Divisão com os outros documentos, para não procurar no lugar errado:

| Documento | Responde |
|---|---|
| `PLAN.md` | **Por quê.** Hipóteses, escolha do sujeito de teste, ameaças à validade, protocolo. |
| `README.md` | **Tour rápido.** Meia dúzia de comandos e o mapa do repositório. |
| `docs/DESCOBERTAS-INTEGRACAO.md` | **Pegadinhas do PgCache.** O que custou tempo na integração. |
| **este arquivo** | **Como.** Executar, coletar, medir, interpretar — e saber quando o número não vale. |

---

## 1. O que está sendo medido

A pergunta é estreita de propósito:

> Um cache **no nível do banco**, coerente por CDC, ganha de um cache **no nível
> da aplicação**, coerente por TTL — em latência e em correção ao mesmo tempo?

O OpenFGA é o sujeito porque todo caminho de leitura dele é `SELECT` de tabela
única com predicados de igualdade, e porque ele resolve o grafo de autorização em
Go, não em SQL. Um único `Check` vira **124 a 480 consultas** pequenas e
repetitivas ao datastore. É o perfil em que um cache de leitura deveria brilhar, e
onde dá para medir isso sem ambiguidade.

---

## 2. Arquitetura

### Os três caminhos

```
                fgabench  (gerador open-loop, fora do caminho de dados)
                     │  rotula toda métrica com path ∈ {A,B,C}
     ┌───────────────┼───────────────┐
     ▼               ▼               ▼
 openfga-a:18080  openfga-b:18090  openfga-c:18100
 caches OFF       caches OFF       caches ON (in-process, TTL 10 s)
     │               │               │
     │          pgcache:16432        │
     │        (PostgreSQL 18         │
     │         embutido na imagem)   │
     │               │               │
     └──────────► origin:15432 ◄─────┘
                Postgres 17, wal_level=logical
                shared_buffers FIXO em 1 GB
```

| Path | Nome | O que isola |
|---|---|---|
| **A** | `baseline` | Custo real do datastore, sem cache nenhum. |
| **B** | `pgcache` | Ganho atribuível **só** ao PgCache. |
| **C** | `appcache` | O competidor honesto — cache que já vem no OpenFGA, desligado por padrão. |

**Regra de ouro:** os três apontam para a **mesma origem**, com a **mesma massa**,
na **mesma janela**, com o **mesmo gerador**, na **mesma taxa alvo**. Números de
execuções diferentes nunca são comparados. O `report.py` impõe isso — ver §7.7.

Por que o path C existe: sem ele a pergunta viraria "cache ajuda?", cuja resposta é
obviamente sim. Com ele, a pergunta é *qual* dos dois caches — e aí há resultado.

### Componentes de apoio

| Serviço | Porta host | Papel |
|---|---|---|
| `prometheus` | 19090 | Séries temporais durante a run |
| `grafana` | 13001 | Visualização ao vivo |
| `pgexp-origin` | 19187 | `postgres_exporter` na origem |
| `cadvisor` | 18081 | CPU/memória por container |
| métricas OpenFGA | 21120 / 21121 / 21122 | `datastore_query_count`, `dispatch_count` |
| métricas PgCache | 19091 | hit/miss, shapes, lag de CDC |

Portas em faixa alta para não colidir com Postgres/Keycloak/apps já em execução na
máquina. Todas sobrescritíveis via `.env` (`PORT_*`).

### Limites de recurso

Definidos em `docker-compose.yml`, e **eles importam para o resultado**:

| Serviço | cpus | memória |
|---|---|---|
| `origin` | 3.0 | 4 GiB |
| `pgcache` | `${PGCACHE_CPUS:-4.0}` | `${PGCACHE_MEM:-5g}` |
| `openfga-{a,b,c}` | 2.0 | 2 GiB cada |

> **Armadilha já paga.** O `mem_limit` do PgCache precisa ficar **abaixo da RAM
> física do host**. Limite acima da RAM é um cgroup que nunca corta: o processo
> cresce até esgotar a máquina e reinicia no meio da janela, zerando os contadores.
> Aconteceu com `8g` num host de 7,75 GiB. Confira antes:
> ```bash
> docker info --format '{{.MemTotal}}' | awk '{printf "%.2f GiB\n",$1/1073741824}'
> ```

---

## 3. A massa e o oráculo

`tools/internal/universe` define a massa de forma **puramente estrutural**: dadas as
contagens, a autorização de qualquer par (usuário, documento) é derivável em
O(profundidade), **sem consultar nada**.

Isso é o que separa este benchmark de um gerador de ruído:

1. Semear 10 M+ tuplas via `COPY` sem guardar estado — a API do OpenFGA limita 100
   tuplas por chamada, seriam ~97 mil requisições.
2. O gerador construir mixes **exatos**: W1 com ~85 % de positivos, W3 com 100 % de
   negativos. Negativo é o caso que força o fan-out completo, porque `union` faz
   short-circuit no primeiro `Allowed: true`.
3. Verificar que o OpenFGA responde a coisa **certa**, não só rápido.

### A escada

| Degrau | Users | Groups | Folders | Docs | Tuplas |
|---|---|---|---|---|---|
| E0 | 200 | 50 | 100 | 500 | 3 453 |
| E1 | 2 000 | 500 | 1 000 | 10 000 | 84 598 |
| E2 | 10 000 | 2 500 | 5 000 | 100 000 | 1 023 498 |
| E3 | 50 000 | 12 500 | 20 000 | 800 000 | 9 710 498 |
| E4 | 100 000 | 25 000 | 50 000 | 2 500 000 | 30 249 998 |

A escada existe para achar **o joelho da curva** — o ponto em que a massa deixa de
caber no `shared_buffers` da origem. Um ponto de medição não prova nada; a curva
prova. Por isso `shared_buffers` fica fixo em 1 GB em todos os degraus: sem esse
controle, a escada mede "quanta RAM eu dei ao Postgres".

> **Um degrau por vez.** `scripts/seed.sh` faz `DELETE FROM tuple … DELETE FROM
> store` antes de carregar. Semear E1 apaga o E3. Os `results/` e o `manifest.json`
> de cada degrau sobrevivem, mas o banco só comporta um degrau por vez.

---

## 4. Os workloads

| ID | Nome | Mix | Distribuição | O que responde |
|---|---|---|---|---|
| **W1** | `check-hot` | 100 % Check, ~85 % positivo | Zipf α=1,1 | Estado estacionário cache-friendly. |
| **W2** | `check-cold` | 100 % Check | uniforme | Pior caso do cache: sem localidade. Testa se o proxy *atrapalha*. |
| **W3** | `check-deny` | 100 % Check, 100 % negativo | Zipf α=1,1 | Fan-out máximo, sem short-circuit. |
| **W4** | `list-objects` | 100 % ListObjects | Zipf | Amplificação de leitura extrema. |
| **W5** | `mixed-write` | 95 % Check / 5 % Write | Zipf | Lag de CDC, invalidação, obsolescência. |
| **W6** | `ceiling` | como W1, rampa aberta | Zipf | Teto de throughput sob SLO p99 ≤ 200 ms. |
| **W7** | `differential` | pares idênticos em A e B | uniforme | **Correção.** Não mede tempo — mede acordo. |

### Gerador open-loop

`fgabench` dispara em taxa fixa a partir de um agendador de chegadas e **não espera
a resposta anterior**. A latência é medida a partir do **instante pretendido de
chegada**, não de quando um worker ficou livre.

Isso evita *coordinated omission*: o erro clássico em que um sistema saturado parece
rápido porque o cliente parou de enviar carga. O preço é que, sob saturação, a fila
do cliente enche e o excedente é contabilizado como **descarte** — ver §7.2.

### O eixo de localidade do documento

```bash
-doc-dist zipf      # default — localidade de objeto, o comportamento pretendido
-doc-dist uniform   # legado — sem localidade, reproduz a run de 25/07/2026
```

Isto não é detalhe. `PositiveDocFor`/`NegativeDocFor` mapeiam a semente por
aritmética modular; semeadas com o número de sequência da requisição, produziam
**99,4 % de documentos distintos** em 1600 requisições. Sem `object_id` repetido não
existe `WHERE` repetido, e o benchmark deixa de exercitar a única coisa que um cache
de leitura faz.

`tools/cmd/fgabench/generator_test.go` trava essa regressão. Rode `make test`.

---

## 5. Execução manual, do zero

### 5.1 Pré-voo

```bash
cp .env.example .env          # ajuste PGCACHE_MEM para caber na RAM do host
make build                    # compila fgaseed e fgabench em ./bin
make test                     # oráculo vs força bruta + localidade. OBRIGATÓRIO
```

`make test` não é formalidade: já pegou dois erros de modelagem reais — a pasta raiz
concedendo acesso ao grupo raiz, e a existência de usuários oniscientes.

Pare o que não é do lab. Containers estrangeiros disputam os mesmos vCPU:

```bash
docker ps --format '{{.Names}}' | grep -v '^fga-'
```

`scripts/suite.sh` aborta se encontrar algum. `ALLOW_FOREIGN=1` prossegue mesmo
assim — e aí a contaminação precisa entrar no relatório.

### 5.2 Subir e semear

```bash
make up                       # origin, pgcache, 3× openfga, prometheus, grafana
make seed RUNG=E1             # semeia e valida a massa contra o oráculo
```

O seed termina com a verificação:

```
  positivos confirmados: 300   divergentes: 0
  negativos confirmados: 300   divergentes: 0
  OK — oraculo e OpenFGA concordam em 100% da amostra.
```

Se divergir, **pare**. A massa está errada e nenhuma medição de tempo importa.

### 5.3 Gate de correção, antes de qualquer medida de tempo

```bash
./bin/fgabench -workload w7 -manifest results/E1/manifest.json \
  -target http://localhost:18080 -diff-target http://localhost:18090 \
  -consistency MINIMIZE_LATENCY -workers 32
```

Saída esperada:

```
  amostras comparadas : 3969
  concordancia A==B   : 3969 (100.000000%)
  divergencias        : 0
  A bate com oraculo  : 3969 (100.000000%)
  B bate com oraculo  : 3969 (100.000000%)
```

Para um sistema de autorização, **uma resposta cacheada errada é um incidente de
segurança, não uma métrica de performance**. Se divergir, o benchmark vira bug
report e a suíte aborta.

### 5.4 Uma execução isolada

```bash
./scripts/run.sh <DEGRAU> <WORKLOAD> <PATH> [taxa] [duração] [aquecimento]

# exemplo
DOC_DIST=zipf WARMUP_MAX=3m RUN_ID=teste1 \
  ./scripts/run.sh E1 w1 B 40 60s 30s
```

Variáveis de ambiente reconhecidas:

| Variável | Default | Efeito |
|---|---|---|
| `DOC_DIST` | `zipf` | Localidade do eixo documento |
| `WARMUP_MAX` | — | Liga o aquecimento adaptativo; o 6º argumento vira o tamanho da fatia |
| `STATS_INTERVAL` | `2` | Intervalo de amostragem do `docker stats`, em segundos |
| `WORKERS` | `32` | Goroutines do gerador |
| `RUN_ID` | timestamp | Identificador da execução |

**Aquecimento adaptativo.** Com `WARMUP_MAX`, o gerador aquece em fatias e mede o
hit ratio **da fatia**, parando quando estabiliza:

```
[B/w3] aquecendo ate' o joelho do hit ratio (fatias de 30s, teto 3m0s)...
  30s: hit ratio da fatia 77.0%
  1m0s: hit ratio da fatia 87.8%
  1m30s: hit ratio da fatia 89.4%
  2m0s: hit ratio da fatia 90.6%
  2m30s: hit ratio da fatia 91.4%
[B/w3] joelho em 2m30s (delta 0.0088 < 0.0100)
```

Se aparecer `AVISO: teto de aquecimento atingido sem estabilizar`, a janela de
medição pegou cache ainda em formação — **medir cache frio e chamar de PgCache é
fraude**. Suba o teto ou registre a limitação.

### 5.5 A suíte completa de um degrau

```bash
make suite RUNG=E1 REPS=3 RATE=40 DUR=60s
```

Roda o gate W7, depois W1/W2/W3 × paths A/B/C × N repetições. Antes de cada execução
do path B, reinicia o PgCache — assim toda repetição parte de **cache frio** e as
repetições são independentes.

Ordem recomendada (`PLAN.md` §11):

```
W7 correção (E0)  →  E0 smoke  →  E1 → E2 → E3  →  W4/W5 em E2  →  W6 em E3
```

### 5.6 Gerar o relatório

```bash
make report
```

Produz `report/RELATORIO.md`, `report/relatorio.html` e
`report/dados-consolidados.csv`.

---

## 6. Onde ficam os artefatos

### Por execução — `results/<degrau>/<workload>/<path>-<run_id>/`

| Arquivo | Conteúdo |
|---|---|
| `summary.json` | Resumo estruturado da execução |
| `latencies.csv` | **Uma linha por requisição**: `latency_ms,ok,allowed` |
| `docker-stats.csv` | `ts,name,cpu_perc,mem_usage`, amostrado **durante** a janela |
| `versions.txt` | `serviço → imagem → digest`, para reprodutibilidade |
| `pg_stat_statements.psv` | SQL mais caro na origem, separador `\x01` |
| `openfga-metrics-{before,after}.txt` | `/metrics` cru, delimitando a janela |
| `pgcache-metrics-{before,after}.txt` | idem, só no path B |

`latencies.csv` é o dado bruto — se quiser um percentil que o relatório não calcula,
ele está ali.

### Consolidado — `results/all-runs.csv`

Uma linha por execução, 26 colunas:

```
run_id,path,path_name,rung,workload,rate_target,rate_achieved,requests,errors,
dropped,allowed,denied,p50_ms,p90_ms,p95_ms,p99_ms,p999_ms,max_ms,mean_ms,
queries_per_request,pgcache_hit_ratio,consistency,started_at,ended_at,
doc_dist,warmup_s
```

Exemplo sintético:

```
r1-120000,A,baseline,E1,w1,40,40.00,2400,0,1,2018,382,11.17,19.39,23.06,42.33,
110.30,120.76,12.85,128.56,0.000000,MINIMIZE_LATENCY,
2026-01-01T00:00:00Z,2026-01-01T00:01:00Z,zipf,30
```

Datas em RFC3339 UTC. Colunas novas entram sempre no **fim** — o `fgabench`
reescreve o cabeçalho sozinho se detectar que está defasado, senão o `DictReader` do
`report.py` descartaria as colunas extras em silêncio.

---

## 7. Como medir e interpretar

### 7.1 A métrica que explica tudo — amplificação

`queries_per_request`, derivada do `datastore_query_count` do **próprio OpenFGA**
(delta dentro da janela, não estimativa).

Valores observados: **124 a 480** consultas ao datastore por requisição.

É a alavanca que transforma diferença pequena em colapso. Uma sobrecarga de 0,3 ms
por consulta vira 60 ms por requisição a 200 consultas. Ao comparar dois caminhos,
**olhe a amplificação antes da latência**: se ela mudou, você não está comparando a
mesma quantidade de trabalho.

### 7.2 Descarte — quando o percentil não significa nada

```
drop_ratio = dropped / (requests + dropped)
```

Sob saturação a fila do cliente enche e o excedente é descartado. **O descarte não é
aleatório**: some exatamente o que teria demorado mais. Um p99 calculado sobre os
sobreviventes é um número sem referente.

O `report.py` marca com ⚠ tudo acima de **10 %** e recusa a comparação:

> **PgCache vs baseline:** não comparável — pelo menos um dos caminhos colapsou e
> seus percentis não descrevem a carga oferecida.

Caso real: em E3 o path B completou **26** requisições e descartou 1 575. O "p99 de
39 455 ms" vem de 26 amostras. Leia como *não sustentou a taxa*, não como latência.

### 7.3 Hit ratio alto não é ganho

Medido em E1: **82 % de hit** e o p99 ainda 7× pior que o baseline.

A assinatura está no p50 contra o p99:

| | p50 | p99 |
|---|---:|---:|
| A baseline | 10,07 | 43,42 |
| B pgcache | 11,36 | **301,32** |

Mediana praticamente empatada, cauda explodindo. Isso é **custo de miss**, não de
hit. Confirme nas métricas do PgCache:

```
pgcache_cache_queries_registered                29.300    <- shapes distintas
pgcache_query_registration_latency_seconds_sum   1.569,8   <- 26 min acumulados
pgcache_cache_registration_throttled_total      57.425
```

~19,7 ms por registro de *shape*. Com 124 consultas por `Check`, a chance de topar
pelo menos uma shape nova é alta — e cada uma paga o registro.

### 7.4 Onde está o gargalo

Antes de atribuir qualquer coisa ao cache, olhe o `docker-stats.csv` **da janela**:

```bash
awk -F, '$2=="fga-pgcache"{gsub("%","",$3); if($3+0>m)m=$3+0} END{print m"%"}' \
  results/E1/w3/B-r3-*/docker-stats.csv
```

Container no teto do próprio limite não mede o software — mede o limite. `cpu_perc`
é relativo a **um** núcleo: `140%` num container com `cpus: 4.0` é 35 % de
utilização; `202%` com `cpus: 2.0` é saturação total.

### 7.5 Métricas do PgCache que valem ler

| Série | Significado |
|---|---|
| `pgcache_queries_total` | Consultas que passaram pelo proxy |
| `pgcache_queries_cacheable` / `_uncacheable` | Classificação. Com protocolo estendido do `pgx`, ~99,97 % vira *uncacheable* |
| `pgcache_queries_cache_hit` / `_miss` | Numerador e denominador do hit ratio |
| `pgcache_cache_queries_registered` | Shapes distintas registradas — cresce com o produto cartesiano da massa |
| `pgcache_cache_queries_loading` | Shapes ainda populando. Alto e estável = cache nunca converge |
| `pgcache_query_registration_latency_seconds_*` | O custo do miss |
| `pgcache_cdc_lag_seconds` | Janela de obsolescência. Compare com o TTL de 10 s do path C |

> Os contadores são cumulativos e **zeram no restart**. Se o delta `after - before`
> der negativo, o PgCache reiniciou no meio da janela: execução contaminada,
> descarte.

### 7.6 SQL na origem

`pg_stat_statements.psv` mostra o que a origem realmente executou:

```
calls   mean_ms  rows    query
140337  0.011    60712   SELECT … FROM tuple WHERE store=$1 AND user_type=$2 AND …
119383  0.009    687     SELECT … FROM tuple WHERE _user=$1 AND object_id=$2 AND …
```

Duas leituras importantes: `mean_ms` diz quanto o cache economiza de fato por
consulta — na casa dos **microssegundos** quando a massa cabe em `shared_buffers`; e
`shared_blks_read = 0` confirma que nada veio do disco.

O reset do `pg_stat_statements` roda **entre** o aquecimento e a medição, via
`-post-warmup-exec`, para que a janela do servidor coincida com a do cliente.

### 7.7 Agregação

O `report.py` agrupa por `(degrau, workload, taxa_alvo, doc_dist, path)` e reporta a
**mediana** entre repetições. Taxa alvo e `doc_dist` entram na chave porque:

- misturar uma sonda a 5 rps com a medição a 40 rps produz uma "mediana" que não
  corresponde a nenhuma condição executada;
- `zipf` e `uniform` são workloads diferentes — agregar os dois é tirar a média de
  dois experimentos.

Empates são resolvidos pela execução **mais recente**, para que uma reexecução não
seja ignorada em silêncio. Execuções fora do grupo escolhido aparecem no stdout:

```
aviso: 4 execucoes fora da taxa alvo comparavel, nao agregadas:
  E1/w1/A run=probe5 alvo=5 rps alcancado=5.0 rps
```

Execuções com mais de 50 % de erro são descartadas — falha inteira é incidente, não
repetição.

---

## 8. Checklist antes de publicar qualquer número

- [ ] `make test` passou — oráculo e localidade
- [ ] Massa validada pós-seed: 600/600 sem divergência
- [ ] Gate W7 passou: 100,000000 % de concordância
- [ ] `PGCACHE_MEM` abaixo da RAM física do host
- [ ] Nenhum container estrangeiro durante a medição, ou contaminação registrada
- [ ] Aquecimento atingiu o joelho (sem `AVISO: teto … sem estabilizar`)
- [ ] `drop_ratio < 10 %` em todos os caminhos comparados
- [ ] Delta dos contadores do PgCache positivo (sem restart no meio)
- [ ] Os três caminhos na **mesma taxa alvo** e no mesmo `doc_dist`
- [ ] ≥ 3 repetições por combinação
- [ ] `versions.txt` com digests — `latest` não identifica build
- [ ] `docker-stats.csv` da janela conferido: ninguém no teto do próprio limite

Falhou algum? O número não vale. Registre a limitação em vez de publicar.

---

## 9. Limitações conhecidas

**Confundimento do protocolo.** Para o PgCache enxergar as consultas é preciso
`PGX_EXEC_MODE=simple_protocol`. Ou seja: o path B não é "o path A mais um cache", e
sim "o path A **sem prepared statements**, mais um cache". Rodar o path A em
`simple_protocol` isola essa parcela — o botão existe no `.env` e a medição ainda não
foi feita.

**Uma máquina só.** Todos os containers competem pelos mesmos vCPU, incluindo o
gerador. Limites fixos por serviço mitigam, mas não eliminam. Rodar cada papel em
máquina separada é a correção real.

**Origem local.** Entre containers no mesmo host, o round-trip evitado por um hit é
~0,1 ms — a condição em que o PgCache tem menos a ganhar, por construção. O teste com
origem remota (1 a 3 ms, multiplicados por 124-480 consultas por `Check`) é o que
pode inverter o sinal, e não é executável nesta topologia.

**Cobertura.** W4, W5 e W6 ainda não foram executados em nenhum degrau.

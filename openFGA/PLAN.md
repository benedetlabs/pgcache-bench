# Plano de Benchmark — PgCache × OpenFGA

**Versão:** 1.0 · **Autor:** BenedetLabs · **Status:** executável
**Lab irmão:** `../pgCache-bench` (Mattermost)

---

## 1. Por que OpenFGA é o sujeito de teste certo para o PgCache

O lab anterior (`pgCache-bench`) usa Mattermost. Mattermost é realista, mas seu tráfego SQL
é heterogêneo: mistura escrita, `LATERAL`, views e consultas que o PgCache não consegue
cachear — o que dilui o sinal.

O OpenFGA é o oposto. Auditamos o adaptador Postgres dele
(`pkg/storage/postgres/postgres.go`, `main`, release `v1.18.x`) e o resultado é inequívoco:

> **Todo caminho de leitura do OpenFGA é um `SELECT` de tabela única com predicados de
> igualdade. Não existe um único CTE, `JOIN`, `LATERAL`, view, window function ou
> `RECURSIVE` no adaptador Postgres. `FOR UPDATE` aparece apenas na transação de escrita.**

Isso importa porque a lista de *não-cacheável* do PgCache é exatamente: views, RLS, tabelas
sem PK, `LATERAL`, CTEs `RECURSIVE`, `FULL`/`CROSS JOIN`, cláusulas de lock e funções
voláteis fora da `SELECT` list. **O OpenFGA não emite nenhuma delas no caminho de leitura.**

| Método de storage | SQL emitido | Cacheável? |
|---|---|---|
| `ReadUserTuple` | `SELECT … FROM tuple WHERE _user=$1 AND object_id=$2 AND object_type=$3 AND relation=$4 AND store=$5 AND user_type=$6` | ✅ ponto, cobre a PK |
| `ReadUsersetTuples` | `SELECT … FROM tuple WHERE store=$1 AND user_type='userset' AND object_type=$2 AND relation=$3 AND (_user LIKE 'group:%#member' OR …)` | ✅ |
| `ReadStartingWithUser` | `SELECT … FROM tuple WHERE _user IN (…) AND object_type=$2 AND relation=$3 AND store=$4 ORDER BY object_id collate "C"` | ✅ |
| `Read` (iterator do TTU) | `SELECT … FROM tuple WHERE store=$1 AND object_type=$2 AND object_id=$3 AND relation=$4` | ✅ |
| `ReadPage` | idem + `ORDER BY ulid LIMIT n+1` | ✅ |
| `ReadAuthorizationModel` | `SELECT … FROM authorization_model WHERE authorization_model_id=$1 AND store=$2` | ✅ (irrelevante — já tem LRU in-process) |
| `ReadChanges` | `… FROM changelog WHERE store=$1 AND inserted_at < NOW() - interval '0ms' …` | ❌ **volátil** (`NOW()` interpolado como texto literal) |
| Write path | `… IN ((…)) FOR UPDATE`, `INSERT`, `DELETE` | ❌ (por design) |

**Consequência metodológica:** configuramos `ALLOWED_TABLES=tuple,authorization_model` no
PgCache. Isso exclui `changelog` — a única consulta de leitura cuja resposta muda com o
relógio de parede. Sem isso, o benchmark teria um risco de correção difícil de auditar.
Com isso, **toda consulta admitida no cache é determinística**.

### O segundo motivo: amplificação de consultas

A resolução do OpenFGA acontece **inteiramente em Go** (`internal/graph/check.go`), não em
SQL. Um `Check` é uma travessia BFS na árvore de reescrita do modelo, e **cada tupla
retornada vira um dispatch filho**. Não há batching: nenhuma coalescência de `IN` entre
sub-problemas irmãos.

- `Check` direto trivial: **1 query**
- Relação typed `[user, user:*, group#member]`: **3 queries concorrentes** no nó
- Userset com fan-out F: `1 + F` sub-resoluções
- TTU (`viewer from parent`) com P pais: `1 + P`
- Profundidade 3 × fan-out 10 ≈ **~1000 queries para uma resposta negativa**

E há uma assimetria decisiva: `union` faz short-circuit no primeiro `Allowed: true` e
cancela os irmãos (`check.go:207`). **Uma resposta positiva termina em poucas queries; uma
negativa precisa esgotar todo o fan-out.** É exatamente o
[issue #727](https://github.com/openfga/openfga/issues/727) — fechado como *stale*, nunca
corrigido no motor; a solução oficial foi "remodele seus dados".

Ou seja: o OpenFGA gera **muitas consultas pequenas, repetitivas e cacheáveis por
requisição**, e o pior caso dele é justamente o caso de maior repetição. É o perfil de carga
em que um cache de leitura coerente deveria produzir o maior ganho mensurável.

### O terceiro motivo: existe um competidor honesto

O OpenFGA tem cache próprio (`--check-query-cache-enabled`, `--check-iterator-cache-enabled`,
`--cache-controller-enabled`). **Todos vêm desligados por default** — o binário de fábrica é
cache-free no caminho de tuplas. Mas eles existem, e um benchmark que os ignorasse seria
desonesto.

Então medimos os três, e a pergunta real do relatório fica:

> Um cache **no nível do banco**, coerente por CDC, ganha de um cache **no nível da
> aplicação**, coerente por TTL?

O cache do OpenFGA é TTL (10s default): pode devolver decisão de autorização obsoleta por
até 10 segundos, e cobre `Check` e apenas parcialmente `ListObjects`. O PgCache invalida por
*change data capture* e cobre qualquer leitura, inclusive `ListObjects` e `ListUsers`. Se a
hipótese estiver certa, o PgCache ganha **em latência e em correção ao mesmo tempo** — e
esse é o resultado que vale publicar.

---

## 2. Hipóteses

**H1 (latência).** Sob localidade Zipf realista, o PgCache reduz o p95 de `Check` em ≥40% e
o p99 em ≥50% frente ao baseline, a partir de 1M tuplas.

**H2 (amplificação).** O ganho cresce monotonicamente com a amplificação de consultas por
requisição. Checks negativos (fan-out completo) ganham mais que positivos (short-circuit).
Métrica de controle: `datastore_query_count` do próprio OpenFGA.

**H3 (teto).** O throughput sustentado sob SLO p99 ≤ 200 ms é ≥2× maior no caminho PgCache,
porque o gargalo do baseline é CPU/IO da origem, não o motor de resolução.

**H4 (correção) — a hipótese que mais importa.** Para um sistema de autorização, uma
resposta cacheada errada é um incidente de segurança, não uma métrica de performance.
Afirmamos: com escritas quiescentes, o PgCache devolve **decisão idêntica** ao baseline em
100% de um universo amostrado de checks; e sob escrita contínua, a janela de divergência é
limitada pelo lag de CDC (sub-segundo) e **nunca é ilimitada**.

**H5 (competidor).** O PgCache entrega latência comparável ou melhor que o cache in-process
do OpenFGA, com janela de obsolescência menor (lag de CDC < TTL de 10s) e cobertura mais
ampla (`ListObjects`/`ListUsers` inclusos).

**H0 (a hipótese nula que também publicamos).** Se o *working set* de tuplas couber no
`shared_buffers` da origem, o baseline já serve tudo da RAM e o PgCache adiciona um hop de
rede sem ganho — ou com perda. Prevemos isso em E0/E1, e é por isso que a escada vai até 10M
tuplas. **Publicamos as derrotas.**

---

## 3. Caminhos medidos

```
                     fgabench (gerador de carga, open-loop)
                              |  label: path in {A,B,C}
        +---------------------+---------------------+
        v                     v                     v
   openfga-a:8080        openfga-b:8090        openfga-c:8100
   caches OFF            caches OFF            caches ON (app-level)
        |                     |                     |
        |                pgcache:6432               |
        |            (cache PG 18 embutido)         |
        |                     |                     |
        +--------------> origin:5432 <--------------+
                    Postgres 17, wal_level=logical
```

| Path | Nome | OpenFGA → | Caches do OpenFGA | O que isola |
|---|---|---|---|---|
| **A** | `baseline` | origin direto | todos OFF | custo real do datastore |
| **B** | `pgcache` | pgcache → origin | todos OFF | ganho atribuível **só** ao PgCache |
| **C** | `appcache` | origin direto | check+iterator+controller ON | o competidor honesto |

Regra de ouro: **os três apontam para a mesma origem, com a mesma massa, na mesma janela, com
o mesmo gerador.** Nunca comparamos números de runs diferentes.

---

## 4. Escada de escala

Cinco degraus. Cada degrau é uma massa completa, semeada do zero, com snapshot próprio.

| Degrau | Users | Groups | Folders | Docs | Profundidade | Tuplas | ~Tamanho |
|---|---|---|---|---|---|---|---|
| **E0** smoke | 200 | 50 | 100 | 500 | 3 | ~10 K | ~3 MB |
| **E1** | 2 000 | 500 | 1 000 | 10 000 | 4 | ~100 K | ~30 MB |
| **E2** | 10 000 | 2 500 | 5 000 | 100 000 | 5 | ~1 M | ~300 MB |
| **E3** | 50 000 | 12 500 | 20 000 | 800 000 | 6 | ~10 M | ~3 GB |
| **E4** opcional | 100 000 | 25 000 | 50 000 | 2 500 000 | 6 | ~30 M | ~9 GB |

A escada existe para achar **o joelho da curva**: o ponto em que a massa deixa de caber no
`shared_buffers` da origem e o cache passa a valer. Um único ponto de medição não prova
nada; a curva prova.

Controle deliberado: `shared_buffers` da origem **fixo em 1 GB** em todos os degraus. Sem
isso a escada mede "quanta RAM eu dei ao Postgres", não escalabilidade.

---

## 5. Modelo de autorização

Domínio Google-Drive-like, 4 tipos, com tuple-to-userset e união de usersets — a forma que
produz amplificação real.

```
model
  schema 1.1

type user

type group
  relations
    define member: [user, group#member]

type folder
  relations
    define parent: [folder]
    define owner:  [user, group#member]
    define editor: [user, group#member] or owner or editor from parent
    define viewer: [user, user:*, group#member] or editor or viewer from parent

type document
  relations
    define parent: [folder]
    define owner:  [user, group#member]
    define editor: [user, group#member] or owner or editor from parent
    define viewer: [user, user:*, group#member] or editor or viewer from parent
```

Dois botões de escala independentes — e é importante que sejam independentes:

- **Profundidade da árvore de pastas** → round-trips por `Check` (cadeia de TTU)
- **Largura de grupo (membros/usuário)** → fan-out em um único nó

`group#member` recursivo (`[user, group#member]`) é intencional: aciona o resolvedor
recursivo do OpenFGA e a `ReadStartingWithUser`, cuja `ORDER BY … collate "C"` não é coberta
pelo índice `idx_user_lookup` em todos os casos — sort node garantido na origem.

---

## 6. Workloads

Todos com distribuição Zipf(α) sobre usuários e objetos, exceto onde indicado. Zipf porque
tráfego de autorização real é fortemente enviesado: poucos usuários e poucos documentos
concentram a maior parte dos checks.

| ID | Nome | Mix | α (Zipf) | O que responde |
|---|---|---|---|---|
| **W1** | `check-hot` | 100% Check, ~85% positivo | 1.1 | Estado estacionário cache-friendly. Caso base de H1. |
| **W2** | `check-cold` | 100% Check | uniforme | Pior caso do cache: sem localidade. Testa se o PgCache *atrapalha*. |
| **W3** | `check-deny` | 100% Check, 100% negativo | 1.1 | Fan-out máximo. Maior ganho previsto (H2). |
| **W4** | `list-objects` | 100% ListObjects `document#viewer` | 1.1 | Amplificação de leitura extrema. |
| **W5** | `mixed-write` | 95% Check / 5% Write | 1.1 | Lag de CDC, invalidação, canário de obsolescência. |
| **W6** | `ceiling` | como W1, rampa aberta | 1.1 | Throughput máximo sob SLO p99 ≤ 200 ms (H3). |
| **W7** | `differential` | pares de Check idênticos em A e B | uniforme | **Correção (H4).** Não mede tempo — mede acordo. |

**Gerador open-loop.** `fgabench` dispara em taxa fixa a partir de um scheduler de chegada,
não espera a resposta anterior. Isso evita *coordinated omission* — o erro clássico que faz
um sistema saturado parecer rápido porque o cliente para de enviar carga.

---

## 7. Métricas coletadas

**Cliente (`fgabench` → CSV)**
`p50/p90/p95/p99/p99.9/max`, throughput alcançado vs. alvo, taxa de erro, timeouts,
distribuição allowed/denied (sanidade).

**OpenFGA (`:2112/metrics`, com `--metrics-enable-rpc-histograms --datastore-metrics-enabled`)**
`datastore_query_count` por requisição (**métrica-chave de H2**), `dispatch_count`,
`request_duration_ms`, `datastore_query_duration_ms`.

**PgCache (`:9090/metrics`)**
`pgcache_queries_cache_hit` / `pgcache_queries_cache_miss`, hit ratio por chamadas **e por
tempo**, `pgcache_cdc_lag_seconds`, `pgcache_cdc_lag_bytes`, shapes registrados, RSS.

**Origem (postgres_exporter + `pg_stat_statements`)**
`calls` e `total_exec_time` por queryid, `blks_read` vs `blks_hit`, `tup_returned`,
`xact_commit`, conexões ativas, CPU/IO do container.

**Correção (W7 + canário)**
Taxa de concordância A vs B (alvo: 100,000%) e tempo de convergência pós-escrita: escreve
tupla → poll de `Check` até virar → registra ms. Distribuição, não média.

Toda métrica, log e trace carrega `scenario`, `rung`, `path` e `run_id`. Sem exceção.

---

## 8. Protocolo de execução (regras de ouro)

1. **Restaurar snapshot antes de CADA path.** Nunca rodar B sobre o estado deixado por A.
2. **Warm-up de 5 min excluído da janela de análise.** No path B, esperar o *joelho* do
   hit-rate antes de abrir a janela — medir cache frio e chamar de "PgCache" é fraude.
3. **3 repetições por (degrau × workload × path).** Reportar mediana + dispersão (min/max),
   nunca uma execução única.
4. **Janelas de análise cortadas por timestamp**, idênticas entre paths.
5. **`docker stats` amostrado durante toda a run.** Se a origem satura CPU no baseline e o
   pgcache não, isso é o resultado — não um artefato.
6. **W7 (diferencial) roda antes de qualquer medição de tempo.** Se A e B discordam, o
   benchmark é abortado e vira bug report, não relatório de performance.

---

## 9. Ameaças à validade (e o que fazemos sobre cada uma)

| Ameaça | Por que é um risco | Mitigação |
|---|---|---|
| Máquina única, containers competindo | 8 vCPU divididos entre origin, pgcache, 3× OpenFGA e o gerador | `cpus:`/`mem_limit` fixos por serviço; **um path por vez**, nunca os três sob carga simultânea |
| Tudo cabe na RAM | Em E0/E1 a origem serve 100% de `shared_buffers` e o cache não tem o que ganhar | `shared_buffers` fixo em 1 GB; escada até 10M/30M tuplas; **reportamos o degrau onde o ganho é zero ou negativo** |
| Hop de rede extra no path B | PgCache adiciona latência de proxy que o baseline não tem | Medido explicitamente em W2 (sem localidade): se B perde em W2, publicamos o custo do hop |
| Semeadura via SQL ≠ via API | Escrevemos tuplas com `COPY` (a API limita a 100 tuplas/chamada — 10M tuplas seriam 100 mil chamadas) | Validação pós-seed: amostra conferida via API `Read`; `changelog` populado consistentemente; `ANALYZE` obrigatório |
| Cache do OpenFGA ligado sem querer | Contaminaria A e B | Flags explícitas `=false` **e** `--experimentals=""` (que **não** é vazio por default — vem `["pipeline_list_objects"]`) |
| `--request-timeout=3s` default | Sob saturação mediríamos timeout, não latência | Elevado para 60s em todas as runs |
| `--datastore-max-open-conns=30` default | É o teto real de concorrência, independente de qualquer outra flag | Fixado explicitamente e igual nos três paths |
| Telemetria anônima do PgCache | Ruído de rede não controlado | `PGCACHE_TELEMETRY=off` |
| Zipf mal calibrado | α alto demais → cabe em qualquer cache; α=∞ vira microbenchmark | α=1.1 (lei de Zipf clássica), com W2 uniforme como controle |

---

## 10. Entregáveis

1. `report/relatorio.html` — dashboard interativo autocontido: curvas de latência por degrau,
   hit ratio, amplificação de consultas, teto de throughput, matriz de correção.
2. `report/RELATORIO.md` — relatório técnico versionável.
3. `results/**/*.csv` — dados brutos, para reanálise independente.
4. `report/relatorio-executivo.pdf` — versão apresentável.
5. Este plano + `README.md` operacional.

---

## 11. Ordem de execução

```
W7 (correção, E0)   <- gate: se falhar, para tudo
    v
E0 smoke (W1)       <- gate funcional: os 3 paths respondem igual?
    v
E1 -> E2 -> E3      <- W1, W2, W3 em cada degrau, 3 reps, 3 paths
    v
E2 (W4, W5)         <- ListObjects e pressão de escrita
    v
E3 (W6)             <- teto de throughput
    v
relatório
```

Tempo estimado: E0 ~10 min · E1 ~25 min · E2 ~1 h · E3 ~3–4 h (dominado pela semeadura de
10M tuplas e pelo warm-up de cache).

---

## 12. Resultado da primeira execução (25/07/2026)

Executado. Ver `report/RELATORIO.md`, `report/relatorio.html` e
`docs/DESCOBERTAS-INTEGRACAO.md`.

Resumo: **H4 (correção) confirmada** — 3 657 decisões verificadas, zero divergências.
**H1, H2, H3 e H5 refutadas** — o PgCache perdeu em todos os degraus e workloads.
**H0 (a hipótese nula) confirmada e ampliada**: não só o cache não ganha quando a massa
cabe em RAM, como perde por três mecanismos compostos (protocolo, espaço de shapes,
custo real de um hit) multiplicados pela amplificação de 150 a 480 consultas por Check
do OpenFGA.

Achado que reorienta o lab: **o gargalo não era o banco.** A origem ficou a 4,8 % de CPU
enquanto o OpenFGA saturava 2 vCPU. Antes de medir cache, medir onde está o gargalo.

> **Errata.** Uma auditoria posterior dos artefatos invalidou parte disto. Bloco ERRATA
> no topo de `report/RELATORIO.md`. Em resumo: (1) os números de CPU acima não estão em
> nenhum arquivo — o `docker stats` rodava depois da run, com o sistema ocioso; (2) o
> eixo documento do gerador era uniforme, não Zipf, então W3 não exercitou repetição de
> `WHERE`; (3) o container do PgCache tinha teto de CPU menor que o da origem e saturou
> em E3; (4) percentis de runs com descarte alto saíram só das requisições que
> completaram.
>
> Correções aplicadas no código. **H1/H2/H3/H5 voltam a "não medidas"** — a refutação
> anterior não se sustenta sem regerar os números. **H4 (correção) permanece
> confirmada:** não depende de nenhum dos quatro pontos.

---

## 13. Sequência de reexecução

Cada passo é barato e cada um pode inverter a conclusão do anterior. Nesta ordem:

```bash
make build && make test                              # o teste novo trava a regressão de localidade
make suite RUNG=E2 REPS=3 RATE=40 DUR=120s           # doc-dist=zipf é o default
DOC_DIST=uniform make suite RUNG=E2 REPS=3 RATE=40   # controle: reproduz o modo antigo
PGX_EXEC_MODE=simple_protocol make suite RUNG=E1     # isola o custo do protocolo no path A
```

O quarto passo é o confundimento deixado em aberto na §12: o path B nunca foi "o path A
mais um cache", e sim "o path A sem prepared statements, mais um cache". Rodar o path A
em `simple_protocol` mede exatamente essa parcela.

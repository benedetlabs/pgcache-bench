# openFGA — lab de benchmark PgCache × OpenFGA

Mede o ganho (e as perdas) do [PgCache](https://pgcache.com) como camada de cache de
leitura na frente do PostgreSQL do [OpenFGA](https://openfga.dev).

**Leia `PLAN.md` antes de rodar qualquer coisa.** Ele é o desenho experimental: as
hipóteses, os três caminhos, a escada de escala, os workloads e — mais importante —
as ameaças à validade e o que fazemos sobre cada uma.

## Por que este sujeito de teste

Todo caminho de leitura do OpenFGA é um `SELECT` de tabela única com predicados de
igualdade — nenhum CTE, `JOIN`, `LATERAL`, view ou `RECURSIVE` no adaptador Postgres.
Isso é exatamente o complemento da lista de *não-cacheável* do PgCache. E como o
OpenFGA resolve o grafo em Go, cada `Check` vira dezenas ou centenas de consultas
pequenas e repetitivas. É o perfil de carga em que um cache de leitura coerente
deveria brilhar — e onde dá pra medir isso sem ambiguidade.

## Os três caminhos

```
        openfga-a:18080       openfga-b:18090       openfga-c:18100
        caches OFF            caches OFF            caches ON (in-process)
            │                     │                     │
            │                pgcache:6432               │
            │                     │                     │
            └──────────────► origin:5432 ◄──────────────┘
```

| Path | Mede |
|---|---|
| **A** baseline | custo real do datastore, sem cache nenhum |
| **B** pgcache | ganho atribuível **só** ao PgCache |
| **C** appcache | o competidor honesto — cache in-process do próprio OpenFGA (TTL 10 s) |

O path C existe porque a pergunta interessante não é "cache ajuda?" (ajuda), e sim:
um cache **no nível do banco**, coerente por CDC, ganha de um cache **no nível da
aplicação**, coerente por TTL? O do OpenFGA pode devolver decisão obsoleta por até
10 s e cobre só `Check` e parte de `ListObjects`; o PgCache invalida por change data
capture e cobre qualquer leitura.

## Uso

```bash
make build            # compila fgaseed e fgabench para ./bin
make test             # valida o ORÁCULO contra força bruta — obrigatório
make up               # sobe a stack completa
make seed RUNG=E0     # semeia um degrau (E0..E4) e valida a massa
make smoke            # gate: E0 + correção diferencial + W1 nos 3 paths
make suite RUNG=E2 REPS=3 RATE=400 DUR=120s
make ceiling RUNG=E2  # rampa até estourar o SLO p99 ≤ 200 ms
make report           # HTML interativo + Markdown + CSV consolidado
```

Grafana em <http://localhost:13001> · Prometheus em <http://localhost:19090>

As portas do host ficam numa faixa alta (15432, 16432, 18080/18090/18100, 19090, 13001)
para nao colidir com Postgres/Keycloak/apps que ja rodam na maquina. Todas sao
sobrescritiveis via `.env` (`PORT_*`).

## Escada de escala

| Degrau | Users | Groups | Folders | Docs | Tuplas |
|---|---|---|---|---|---|
| E0 | 200 | 50 | 100 | 500 | 3 453 |
| E1 | 2 000 | 500 | 1 000 | 10 000 | 84 598 |
| E2 | 10 000 | 2 500 | 5 000 | 100 000 | 1 023 498 |
| E3 | 50 000 | 12 500 | 20 000 | 800 000 | 9 710 498 |
| E4 | 100 000 | 25 000 | 50 000 | 2 500 000 | 30 249 998 |

## O oráculo — a parte que mais importa

`internal/universe` define a massa de forma **puramente estrutural**: dadas as
contagens, a autorização de qualquer par (usuário, documento) é derivável em
O(profundidade), sem consultar nada. Isso permite três coisas que um benchmark
comum não consegue:

1. semear 10M+ tuplas via `COPY` sem guardar estado (a API do OpenFGA limita 100
   tuplas por chamada — seriam ~97 mil requisições);
2. o gerador de carga construir mixes **exatos**: W1 com ~85 % de positivos, W3 com
   100 % de negativos — e negativo é o caso que força o fan-out completo;
3. verificar que o OpenFGA (com e sem PgCache) responde a coisa **certa**, não só
   rápido.

`make test` roda o oráculo contra uma resolução por força bruta sobre o conjunto de
tuplas realmente emitido. Esse teste já pegou dois erros de modelagem reais: a pasta
raiz concedendo acesso ao grupo raiz (100 % dos pares positivos, massa inútil para
W3) e a existência de usuários oniscientes, para os quais não existe check negativo.

## Correção antes de performance

`W7` compara decisões de autorização entre os paths A e B em dezenas de milhares de
pares e falha se **qualquer uma** divergir. Para um sistema de authz, uma resposta
cacheada errada é um incidente de segurança, não uma métrica de performance. O gate
roda antes de qualquer medição de tempo, e a suíte aborta se ele falhar.

## Layout

```
PLAN.md                  desenho experimental (leia primeiro)
docker-compose.yml       origin, pgcache, 3× openfga, prometheus, grafana, exporters
model/model.json         authorization model (Google-Drive-like, TTU + usersets)
tools/internal/universe  a massa e o oráculo analítico (+ testes)
tools/internal/fga       cliente HTTP mínimo do OpenFGA, sem dependências
tools/cmd/fgaseed        bootstrap / geração TSV para COPY / verificação
tools/cmd/fgabench       gerador de carga open-loop, W1..W7
scripts/                 seed, run, suite, ceiling, report
results/                 saída bruta (CSV + JSON + /metrics crus)
report/                  HTML interativo, Markdown, CSV consolidado
```

## Notas de integração que custaram tempo

- A imagem do PgCache **embute um PostgreSQL 18** como store de cache. Não precisa de
  um `cache-pg` separado (o lab do Mattermost assumia que precisava).
- `ALLOWED_TABLES=tuple,authorization_model` exclui `changelog`: a query de
  `ReadChanges` interpola `NOW()` como texto literal, então o mesmo SQL devolve
  linhas diferentes conforme o relógio anda. É a única leitura não determinística do
  OpenFGA.
- `--experimentals` **não** é vazio por default (vem `["pipeline_list_objects"]`).
  Precisa ser zerado explicitamente ou os paths não são comparáveis.
- `--request-timeout` default de 3 s faz uma run saturada medir timeout, não
  latência. Elevado para 60 s.
- `--datastore-max-open-conns` default de 30 é o teto real de concorrência contra o
  banco, independente de qualquer outra flag.
- `PGCACHE_TELEMETRY=off` — telemetria anônima é ruído de rede não controlado.
- O driver `pgx` usa protocolo estendido com cache de statements. Se o PgCache
  engasgar com isso, `PGX_EXEC_MODE=simple_protocol` no `.env` troca o modo.

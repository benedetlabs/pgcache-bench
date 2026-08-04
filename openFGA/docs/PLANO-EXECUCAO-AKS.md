# Executar o benchmark no AKS

Passo a passo para rodar o lab no cluster **`eks-1`**, que já existe. Nada aqui
cria ou destrói cluster — só adiciona e remove node pools dentro dele.

Para o significado de cada métrica e para saber quando um número não vale, veja
`docs/RUNBOOK.md`. Para o desenho do experimento, `PLAN.md`.

---

## O que vai subir

Quatro node pools novos, um pod em cada:

```
pool origin    D8ds_v6   ->  Postgres 17          (a origem)
pool pgcache   E8ds_v6   ->  PgCache              (proxy + PG 18 embutido)
pool openfga   D4_v5     ->  openfga-a / -b / -c  (os tres caminhos)
pool loadgen   D4_v5     ->  pod de trabalho      (voce roda o benchmark daqui)
```

24 vCPU no total, somados aos 4 já em uso pelo `agentpool`. Cabe na quota.

Os pools existentes (`agentpool`, `userpool`) **não são tocados**. Os pods do lab
não caem neles porque cada pool novo leva um taint `role=<nome>:NoSchedule` que
só os pods do chart toleram.

---

## Passo 1 — criar os node pools

```bash
cd infra/aks
./cluster.sh preflight     # opcional: mostra quota, CoreDNS e nós atuais
./cluster.sh pools         # ~5 min
```

Cria os quatro pools e a StorageClass `origin-premiumv2` (Premium SSD v2 com
IOPS provisionado, porque em E3 a origem vira I/O bound por desenho e o disco
precisa ser um parâmetro citável).

## Passo 2 — instalar o chart

```bash
helm install lab ./chart -n pgcache-lab --create-namespace
kubectl -n pgcache-lab get pods -o wide -w
```

Espere todos `Running`. Confira que estão em **nós diferentes** — é o requisito
de isolamento do experimento.

> Já validado no seu cluster: `helm template ... | kubectl apply --dry-run=server`
> passou pelo admission com `hostNetwork: true`. O Azure Policy não bloqueia.

## Passo 3 — medir o RTT

```bash
./cluster.sh rtt
```

Anote p50 e p99 em `infra/aks/lab-params.env`. **Não pule este passo.** O
experimento inteiro pendura nesse número: sem ele, não dá para separar quanto do
resultado é rede e quanto é o cache.

Referência: containers no mesmo host ~0,05 ms · nós na mesma zona 0,2-0,5 ms ·
zonas distintas 1-3 ms. Multiplicado por 124-480 consultas por `Check`, é isso
que o cache tem a poupar.

## Passo 4 — colocar o repositório no pod

O `loadgen` sobe com volume vazio. Duas opções:

```bash
# a) copiar desta máquina
kubectl -n pgcache-lab cp . loadgen:/lab

# b) clonar de um remoto (init container)
helm upgrade lab ./chart -n pgcache-lab \
  --set loadgen.repoUrl=https://github.com/voce/pgcache-openfga-bench
```

## Passo 5 — rodar

```bash
kubectl -n pgcache-lab exec -it loadgen -- bash

# dentro do pod:
make build && make test          # oráculo + regressão de localidade
make seed RUNG=E1                # espera 600/600 sem divergência

# gate de correção — antes de qualquer medida de tempo
./bin/fgabench -workload w7 -manifest results/E1/manifest.json \
  -target "$FGA_A" -diff-target "$FGA_B" -workers 32

# medição
DOC_DIST=zipf WARMUP_MAX=3m make suite RUNG=E1 REPS=3 RATE=40 DUR=60s
```

---

## Acesso externo aos recursos

**Por padrão, não.** Os Services são headless e os pods usam `hostNetwork`, então
tudo escuta em IP privado da VNet. Não há LoadBalancer, Ingress nem IP público.
Isso é intencional: expor Postgres na internet seria ruim, e o benchmark não
precisa — o gerador roda **dentro** do cluster, junto dos alvos.

Para inspecionar da sua máquina, use `port-forward`. Não custa nada e não expõe:

```bash
# API do OpenFGA, os três caminhos
kubectl -n pgcache-lab port-forward svc/openfga-a 8080:8080 &
kubectl -n pgcache-lab port-forward svc/openfga-b 8090:8090 &
kubectl -n pgcache-lab port-forward svc/openfga-c 8100:8100 &

# métricas Prometheus
kubectl -n pgcache-lab port-forward svc/pgcache   9090:9090 &   # PgCache
kubectl -n pgcache-lab port-forward svc/openfga-a 2112:2112 &   # OpenFGA A

# Postgres da origem — para psql, DBeaver, etc.
kubectl -n pgcache-lab port-forward svc/origin 15432:5432 &
```

Depois disso, na sua máquina:

```bash
curl -s localhost:9090/metrics | grep pgcache_queries_cache_hit
psql "postgres://fga:fgapass@localhost:15432/openfga" -c '\dt'
```

Se quiser um Grafana apontando para essas métricas, o `docker-compose.yml` do
repositório já traz um — suba localmente e configure o datasource para
`localhost:9090` com os port-forwards ativos.

> **Não crie Service do tipo LoadBalancer para isto.** Além do custo do IP
> público, exporia Postgres e as APIs do OpenFGA sem autenticação na internet.
> Se precisar mesmo de acesso persistente, use um Ingress interno restrito à
> VNet, não um IP público.

---

## Coletar os resultados

O `loadgen` grava em `emptyDir` — **os dados morrem junto com o node pool.**
Copie antes de derrubar qualquer coisa:

```bash
kubectl -n pgcache-lab cp loadgen:/lab/results ./results-aks
cp infra/aks/lab-params.env ./results-aks/
kubectl -n pgcache-lab get events --sort-by=.lastTimestamp > ./results-aks/eventos.txt
```

O `lab-params.env` leva SKU, zona, IOPS do disco e o RTT medido. Sem esses
parâmetros ao lado dos números, o resultado não é interpretável depois.

Nos eventos, procure `Evicted`, `OOMKilled`, `NodeNotReady` e `Preempted` dentro
da janela de medição. Qualquer um contamina a run — é o equivalente ao reinício
do PgCache que zerou os contadores no lab local.

---

## Desligar

```bash
cd infra/aks && ./cluster.sh pools-down
```

Remove o release, o namespace e os quatro node pools. O cluster e os seus pools
continuam intactos.

**Node pool esquecido ligado é o único jeito deste lab custar caro.**

---

## O segundo braço: quanto custa a rede do Kubernetes

Este é o resultado que só o AKS produz, e que uma VM não daria.

O chart tem um interruptor, `networkMode`:

- `bare` (padrão) — `hostNetwork` + Service headless. Sem kube-proxy, sem NAT,
  sem overlay no caminho de dados. Mede como se fossem VMs.
- `cni` — rede de pod normal + ClusterIP. Mede a topologia de produção real.

Rodando a mesma campanha nos dois, com massa, taxa e SKU idênticos, **a diferença
é o custo da camada de rede do Kubernetes** sobre uma carga de 124 a 480
consultas por requisição.

```bash
helm upgrade lab ./chart -n pgcache-lab --set networkMode=cni
kubectl -n pgcache-lab delete pod loadgen
helm upgrade lab ./chart -n pgcache-lab --set networkMode=cni   # repõe o pod
```

Guarde os resultados dos dois modos em diretórios separados. O `report.py`
separa por `doc_dist` e taxa alvo, mas **não** por modo de rede — misturar os
dois no mesmo `all-runs.csv` seria agregar dois experimentos diferentes.

---

## Pendências antes da primeira campanha

Três coisas do `scripts/run.sh` dependem do Docker do host e **não funcionam
dentro do pod**:

| No lab local | No pod | Situação |
|---|---|---|
| `docker compose exec -T origin psql` | `psql` direto (o pod já tem `PGHOST`/`PGUSER`/`PGPASSWORD`) | trivial |
| `docker stats` amostrado na janela | `kubectl top nodes` num laço | **precisa ser escrito** |
| `docker compose restart pgcache` | `kubectl rollout restart deploy/pgcache` | **precisa de RBAC** |

O resto do pipeline — `fgabench`, `fgaseed`, `report.py`, o oráculo — é agnóstico
e roda sem alteração.

Lista completa:

1. Interruptor `RUNNER=docker|k8s` no `run.sh` e no `suite.sh`.
2. ServiceAccount + Role para o `loadgen`: `pods/exec` e `patch` em `deployments`,
   para reiniciar o PgCache entre repetições (cada rep do path B precisa partir de
   cache frio).
3. Amostrador de `node-stats.csv` no formato `ts,node,cpu_cores,mem_bytes`,
   coletado **durante** a janela. No lab local, a amostragem única pós-run mediu o
   sistema ocioso e invalidou a conclusão sobre CPU.
4. Fixar os digests das imagens — `latest` não identifica build.
5. Suspender o auto-upgrade do cluster durante as janelas de campanha: uma
   reimagem de nó no meio de uma suíte derruba a medição sem aviso.

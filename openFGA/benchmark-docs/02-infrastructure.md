# 02 — Infrastructure

Measurement platform for the PgCache × OpenFGA benchmark. Two environments run the same
three-path topology (A = baseline, B = via PgCache, C = OpenFGA in-process cache) against
the same origin: a local Docker Compose lab on a MacBook, and an AKS cluster in
`brazilsouth`.

**Provenance.** Every number below is either read from a file in this repository or
captured live on 2026-08-02 with read-only commands: `kubectl get pods/nodes/svc/pvc/sc`,
`kubectl version`, `az aks show -g eks-01_group -n eks-1`, `az vm list-usage -l brazilsouth`,
`az grafana list`, `az resource list`. No resource was created, modified, or deleted.
Historical measurements are cited from artifacts under `results/`.

Sources: `infra/aks/chart/` (Chart 0.1.0), `infra/aks/cluster.sh`, `infra/aks/lab.yaml`,
`docker-compose.yml`, `PLAN.md`.

---

## 1. Two environments

### 1.1 Local lab (Docker Compose)

Host: Apple M1 Pro, 16 GiB physical. Docker Desktop's Linux VM is the actual envelope:
**8 vCPU shared with macOS, 7.75 GiB RAM** (`docker info` reports 8 CPUs / 8,321,994,752 B).
All containers, including the observability stack, live inside that 7.75 GiB.

Declared per-container ceilings in `docker-compose.yml`:

| Service | `cpus` | `mem_limit` | Notes |
|---|---|---|---|
| `origin` (postgres:17) | 3.0 | 4 GiB | `shared_buffers=1GB` fixed at every rung |
| `pgcache` | 4.0 (`PGCACHE_CPUS`) | 5 GiB (`PGCACHE_MEM`) | image embeds a full PostgreSQL 18 as the cache store |
| `openfga-a` / `-b` / `-c` | 2.0 each | 2 GiB each | shared anchor `x-openfga-common` |
| observability (prometheus, grafana, cadvisor, pg-exporter) | unbounded | unbounded | shares the same 7.75 GiB |

The declared sum is **13 vCPU and 15 GiB against 8 vCPU and 7.75 GiB**. This is only
tolerable because of the lab's golden rule: exactly one path is under load at a time
(`PLAN.md` §8.1); the other two OpenFGA containers idle. It is still oversubscription, and
it is the structural reason the lab was moved to Kubernetes.

Two failure modes this produced, both documented in `docker-compose.yml` comments and
visible in artifacts:

- `mem_limit: 8g` on a 7.75 GiB VM is a cgroup that never cuts. PgCache grew to
  **5.123 GiB / 7.75 GiB** (`results/_contaminado-mem8g/w3/B-r1-155453/docker-stats.csv`),
  exhausted the VM, and restarted mid-window — counters zeroed, 197 errors. Those runs are
  quarantined under `results/_contaminado-mem8g/`.
- At E3 the PgCache container was capped *below* the origin (2.0 vs 3.0 vCPU) and pinned:
  `fga-pgcache,202.07%,5.245GiB / 6GiB` while `fga-b,0.94%,81.35MiB / 2GiB`
  (`results/E3/w1/B-v3/docker-stats.csv`). A container at its own ceiling measures the
  ceiling, not the software.

### 1.2 AKS lab

Four roles, **one pod per node**, resource `requests` only, no limits, node SKU as the
envelope. Currently deployed in `sharedPool` mode on the pre-existing `userpool`
(Spot `Standard_D8as_v5`), Helm release `lab` revision 6, namespace `pgcache-lab`,
`NETWORK_MODE=bare`.

### 1.3 Comparison

| Dimension | Local Compose lab | AKS `eks-1` (as deployed) |
|---|---|---|
| CPU envelope | 8 vCPU shared, whole lab | 7820m allocatable **per role node**, 4 nodes |
| RAM envelope | 7.75 GiB shared, whole lab | 30,457,548 Ki (~29.0 GiB) allocatable per node |
| Isolation mechanism | cgroup `cpus` / `mem_limit` per container | dedicated node per role (pod anti-affinity) |
| CPU cap semantics | hard CFS quota per container | **no limits declared** — no quota is set |
| Origin ↔ PgCache RTT | same host, ~0.05 ms | NIC-to-NIC, same zone; `cluster.sh rtt` reference 0.2–0.5 ms |
| Origin storage | Docker named volume `origin-data` on the Mac's SSD | Premium SSD v2, 256 GiB, 8000 IOPS / 400 MB/s provisioned |
| Network path under test | Docker bridge only | `bare` (hostNetwork) **or** `cni` (pod network) — the delta is the measurement |
| Observability | Prometheus + Grafana + cAdvisor in-lab | in-lab metrics endpoints; Azure Managed Prometheus disabled |
| Cost when idle | zero | `userpool` autoscales to 0; control plane Free tier |
| Failure mode to watch | OOM / cgroup ceiling contaminating the window | Spot eviction mid-window invalidating the run |

Both environments hold the same experimental controls: `shared_buffers=1GB` fixed at every
rung, `PGCACHE_ALLOWED_TABLES=tuple,authorization_model` (excludes `changelog`, the only
volatile OpenFGA read), OpenFGA request timeout 60 s, identical datastore connection
ceiling across the three paths. The AKS values are re-emitted into the `lab-config`
ConfigMap so the generator can copy them into `results/**/versions.txt`.

---

## 2. AKS cluster `eks-1`

Captured from `az aks show -g eks-01_group -n eks-1`.

| Property | Value |
|---|---|
| Region | `brazilsouth` |
| Kubernetes version | `1.35.6` (server `v1.35.6`, all nodes on `1.35.6`) |
| Control plane SKU | `Base` / tier **`Free`** |
| Support plan | `KubernetesOfficial` |
| Upgrade channel | `patch`; node OS channel `NodeImage` |
| Node resource group | `MC_eks-01_group_eks-1_brazilsouth` |

### 2.1 Network profile

| Property | Value |
|---|---|
| Network plugin | `azure`, plugin mode **`overlay`** |
| Dataplane | **`cilium`** (network policy `cilium`) |
| Pod CIDR | `10.244.0.0/16` |
| Service CIDR | `10.0.0.0/16`, DNS service IP `10.0.0.10` |
| Node subnet (observed) | `10.224.0.0/16` |
| Load balancer | `standard`, `nodeIPConfiguration` backend pool, 1 managed outbound IP |
| Outbound type | `loadBalancer` |
| Advanced networking / observability | disabled |
| IP families | IPv4 only |

Relevant detail for §5: there is **no Linux `kube-proxy` DaemonSet** on this cluster.
`kubectl -n kube-system get ds` shows `cilium`, `azure-cns`, `azure-ip-masq-agent`,
`cloud-node-manager`, the CSI node drivers, and Windows-only stubs. Service translation is
Cilium eBPF, and cross-node pod traffic is Azure CNI Overlay encapsulation.

### 2.2 Node pools

| Pool | Mode | SKU | vCPU / RAM | Count | Autoscale | Zone | Priority | Taints |
|---|---|---|---|---|---|---|---|---|
| `agentpool` | System | `Standard_D2_v5` | 2 / 8 GiB | 1 | 1–2 | `brazilsouth-1` | Regular | none |
| `userpool` | User | `Standard_D8as_v5` | 8 / 32 GiB | 4 | **0–5** | `brazilsouth-1` | **Spot**, eviction `Deallocate`, `spotMaxPrice -1` | `kubernetes.azure.com/scalesetpriority=spot:NoSchedule` |

`userpool` carries the node label `kubernetes.azure.com/scalesetpriority=spot`, OS disk
256 GiB Managed, `maxPods` 110, `scaleDownMode: Delete`. Both pools have
`kubeletConfig: null` — see §4 for why that matters.

Allocatable, live:

| Node | Pool | Internal IP | Allocatable CPU | Allocatable memory |
|---|---|---|---|---|
| `aks-agentpool-11928609-vmss000001` | agentpool | 10.224.0.5 | 1900m | 5,929,716 Ki |
| `aks-userpool-11928609-vmss000000` | userpool | 10.224.0.6 | 7820m | 30,457,548 Ki |
| `aks-userpool-11928609-vmss000002` | userpool | 10.224.0.8 | 7820m | 30,457,544 Ki |
| `aks-userpool-11928609-vmss000003` | userpool | 10.224.0.9 | 7820m | 30,457,548 Ki |
| `aks-userpool-11928609-vmss000004` | userpool | 10.224.0.10 | 7820m | 30,457,548 Ki |

**Spot is the standing hazard.** Eviction policy is `Deallocate`; an eviction inside a
measurement window invalidates that repetition exactly as the PgCache restart did locally.
`values.yaml` prescribes the post-suite check:

```
kubectl -n pgcache-lab get events | grep -i 'preempt\|evict'
```

Anything inside the window → discard and re-run that repetition.

### 2.3 `cluster.sh` dedicated pools (alternative, not currently deployed)

`infra/aks/cluster.sh` operates on the existing cluster only — it never creates or deletes
a cluster. `./cluster.sh pools` would add four tainted single-node pools:

| Pool | SKU | vCPU / RAM | Family quota rationale |
|---|---|---|---|
| `origin` | `Standard_D8ds_v6` | 8 / 32 GiB | Ddsv6, premium-storage capable |
| `pgcache` | `Standard_E8ds_v6` | 8 / 64 GiB | Edsv6, premium-storage capable |
| `openfga` | `Standard_D4_v5` | 4 / 16 GiB | Dv5 |
| `loadgen` | `Standard_D4_v5` | 4 / 16 GiB | Dv5 |

Each with `--node-taints role=<name>:NoSchedule`, `--labels role=<name>`, `--max-pods 30`,
zone from `TOPOLOGY` (`single_az` default, zone 1 — where the system pool and therefore
CoreDNS live, so headless-Service resolution does not cross zones), and
`--kubelet-config` with `cpuManagerPolicy: static` + `cpuCfsQuota: false`. B-series is
explicitly excluded: burstable credits mean a saturated run measures "the balance ran
out", not "the system saturated".

`./cluster.sh rtt` runs a 200-packet `ping` from the openfga node to the origin node under
`hostNetwork` and is treated as a mandatory artifact — without a measured RTT there is no
way to attribute results between network and cache.

---

## 3. Placement: one pod per node

The requirement is not "one node pool per component", it is **one pod per node**. Origin,
PgCache and OpenFGA must not share CPU, or the measurement becomes noise.

In `sharedPool` mode this is achieved without creating any pools, via
`templates/_helpers.tpl` → `lab.placement`:

```yaml
affinity:
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchExpressions:
            - key: lab.benedet/role
              operator: NotIn
              values: ["<this pod's role>"]
        topologyKey: kubernetes.io/hostname
```

Read it as: *"do not place me on a host that already runs a pod whose role differs from
mine."* Same-role pods are allowed to co-locate, which is deliberate — `openfga-a`, `-b`
and `-c` all carry `lab.benedet/role: openfga` and share one node, and that is safe
precisely because only one path is under load at a time.

Live result (`kubectl -n pgcache-lab get pods -o wide`):

| Pod | Role label | Node | IP |
|---|---|---|---|
| `origin-0` | `origin` | `...vmss000000` | 10.224.0.6 |
| `pgcache-6b4cf7bfbd-bpvsd` | `pgcache` | `...vmss000003` | 10.224.0.9 |
| `openfga-a-6ff8668787-4k67m` | `openfga` | `...vmss000002` | 10.224.0.8 |
| `openfga-b-5c678ddc9f-vdb52` | `openfga` | `...vmss000002` | 10.224.0.8 |
| `openfga-c-58cf4cd684-c2rcc` | `openfga` | `...vmss000002` | 10.224.0.8 |
| `loadgen` | `loadgen` | `...vmss000004` | 10.224.0.10 |

Four roles → four distinct nodes, with the three OpenFGA pods sharing one. The three
identical pod IPs are the `hostNetwork` fingerprint (§5).

### 3.1 The `NotIn` subtlety that hung the migration Job

Kubernetes label-selector semantics are not SQL semantics. For `matchExpressions`,
**a pod that does not carry the key at all satisfies `NotIn`** — there is no three-valued
logic, an absent key is simply "not in the value set".

Consequence: a pod without `lab.benedet/role` is selected by *every* lab pod's
anti-affinity selector, on every node. It is mutually exclusive with the entire lab —
no lab pod can be scheduled onto a host it occupies, and (when it inherits the same rule)
it can only land on a host that runs no lab pod at all.

The OpenFGA schema-migration Job hit exactly this. It is required — without it the three
OpenFGA pods loop on `datastore requires migrations: at revision '0', but requires '4'`
and never pass readiness — but as originally written it had no role label, and it sat
`Pending` indefinitely.

`templates/migrate-job.yaml` fixes it two ways, both still in the file:

1. The Job's pod template carries `lab.benedet/role: {{ .Values.openfga.nodeRole }}`,
   i.e. it joins the `openfga` role. Correct choice: same image, short execution, and the
   OpenFGA node has headroom.
2. In `sharedPool` mode the Job renders **only** `nodeSelector` + `tolerations`, not the
   anti-affinity block, so it is never repelled from the node it is meant to share.

It also runs `restartPolicy: OnFailure` with `backoffLimit: 30` instead of an init
container polling `pg_isready` — fewer moving parts, and it retries if the origin is not
up yet. It targets the origin directly, never PgCache: it is DDL, outside `ALLOWED_TABLES`,
and pointless to cache.

Live confirmation: `job/openfga-migrate` shows `Completed` events at both the initial
install and the most recent upgrade; the Job object itself is gone because of
`helm.sh/hook-delete-policy: hook-succeeded`.

---

## 4. Requests only, no CPU limits

Every workload in the chart declares `resources.requests` and **no `limits`**:

| Component | CPU request | Memory request | Node headroom rationale |
|---|---|---|---|
| `origin` | 6 | 20 Gi | ~29 GiB allocatable; the remainder stays with the kubelet |
| `pgcache` | 6 | 24 Gi | ~29 GiB allocatable |
| `openfga` (each of A/B/C) | 1 | 2 Gi | three pods on one node, one under load at a time |
| `loadgen` | 3 | 8 Gi | generator must never be the bottleneck |

Verified live: every container in `pgcache-lab` reports `{"requests": {...}}` with no
`limits` key.

**Why.** A Kubernetes CPU limit is translated by the kubelet into a CFS bandwidth quota:
`cpu.cfs_quota_us` over a `cpu.cfs_period_us` window of 100 ms. A container that spends
its quota inside a window is **descheduled until the window rolls** — up to ~100 ms of
dead time attributed to nothing visible in the application. Statistically this lands in the
tail: p50 barely moves, p99 jumps.

That is the same shape as the signal the lab exists to isolate. The PgCache shape-registration
signature is *p50 tied, p99 ~7× worse*. If CPU limits were in play, a p99 spike would be
uninterpretable: throttling and shape registration are indistinguishable in the latency
histogram. Requests are scheduling arithmetic (they decide placement and cgroup weight);
they do not freeze anything.

The isolation that limits would have provided comes from the node instead: one pod per
node means the only competitor for CPU is the kubelet and the DaemonSets.

**Caveat, verified live.** `cluster.sh` additionally disables the mechanism at the kubelet
(`cpuCfsQuota: false`, `cpuManagerPolicy: static`), but that config is attached per node
pool at `az aks nodepool add` time. Both currently existing pools report
`kubeletConfig: null`, so in the deployed `sharedPool` mode that belt-and-braces layer is
**not** active. It is not needed — no limit is declared, so no quota is written — but any
future manifest that adds a CPU limit on `userpool` would be throttled, silently.

---

## 5. The two network modes

`networkMode` in `values.yaml` is the chart's most consequential switch.

| | `bare` (deployed) | `cni` |
|---|---|---|
| Pod network | `hostNetwork: true` | normal pod network (Azure CNI Overlay, `10.244.0.0/16`) |
| DNS policy | `ClusterFirstWithHostNet` | default `ClusterFirst` |
| Service | headless, `clusterIP: None` | normal `ClusterIP` |
| Resolution result | pod IP == node IP; connection goes straight NIC-to-NIC | virtual IP, translated by the dataplane |
| Data path | no service translation, no NAT, no overlay encapsulation | Cilium eBPF service translation + overlay encapsulation between nodes |
| What it measures | the systems, as if they were VMs | your production topology |

`dnsPolicy: ClusterFirstWithHostNet` is mandatory in `bare`: without it the pod inherits
the node's `resolv.conf` and cannot see cluster Services at all.

Live evidence that `bare` is active: `lab-config` reports `NETWORK_MODE=bare`; all five
Services (`origin`, `pgcache`, `openfga-a/b/c`) show `CLUSTER-IP None`; every pod reports
`hostNetwork: true` and `dnsPolicy: ClusterFirstWithHostNet`; and `openfga-a`, `-b`, `-c`
all report IP `10.224.0.8`, which is the InternalIP of node `...vmss000002`.

**The difference between the two modes is itself a publishable measurement.** With mass,
request rate, SKU and topology held identical, the `bare` → `cni` delta is the cost of the
Kubernetes network layer under a load that issues 124–480 datastore queries per
authorization request (`results/E1/w1/A-r1-*/summary.json` records 131.94 queries per
request at E1/w1). Per-request amplification is what turns a small per-hop overhead into a
visible number. The VM-based lab cannot produce this figure at all.

Two qualifiers:

- On *this* cluster the `cni` side is Cilium eBPF plus Azure CNI Overlay encapsulation,
  not iptables `kube-proxy`. The delta is specific to that dataplane and must be reported
  as such.
- `bare` requires `hostNetwork` to pass admission. The `azurepolicy` addon **is enabled**
  on `eks-1` (with `azure-policy` and `azure-policy-webhook` running in `kube-system`), so
  a future policy assignment or Pod Security Standard could reject it. Today it does not —
  the pods are running. `cluster.sh preflight` documents the real check:
  `helm template lab ./chart -n pgcache-lab | kubectl apply --dry-run=server -f -`.
  If `bare` ever fails admission and the lab falls back to `cni`, that must appear in the
  report, because the results stop being comparable with the local lab.

Three OpenFGA pods sharing a node's network namespace also forced per-path gRPC ports
(8081/8091/8101) — the upstream default 8081 collided with
`panic: failed to listen: 0.0.0.0:8081: address already in use`. HTTP (8080/8090/8100) and
metrics (2112/2113/2114) are likewise disjoint per path.

---

## 6. Resource sizing rationale

Sizing is derived from measured local behaviour, not from round numbers.

### 6.1 PgCache is the hungriest component

The `pgcache/pgcache` image embeds a full PostgreSQL 18 as its cache store. The dominant
consequence for results: **a cache hit is not a memory read, it is a complete SQL query
against that local database.** The memory consequence is that PgCache needs origin-class
resources, not proxy-class resources.

Measured at rung **E1**, which holds only **84,598 tuples** (`results/E1/manifest.json`;
"85 K"):

| Artifact | Peak RSS | Peak CPU | Limit in effect |
|---|---|---|---|
| `results/E1/**/docker-stats.csv` | **4.791 GiB** | 298.43% | `cpus: 4.0` / `mem_limit: 5g` |
| `results/_contaminado-mem8g/w3/B-r1-155453/docker-stats.csv` | 5.123 GiB | 83.27% | `mem_limit: 8g` on a 7.75 GiB host → restart |
| `results/E3/w1/B-v3/docker-stats.csv` | 5.245 GiB | 202.07% | `cpus: 2.0` / `mem_limit: 6g` → pinned |

The chart comment cites 4.41 GiB as the E1 peak; the artifacts in `results/E1` reach
4.791 GiB, which only strengthens the argument. Either way: ~4.4–4.8 GiB at 85 K tuples,
on the *smallest* real rung. E3 carries **9,710,498 tuples**. Hence
`pgcache.resources.requests: {cpu: 6, memory: 24Gi}` and, in the dedicated-pool variant,
a memory-optimized `Standard_E8ds_v6` (8 vCPU / 64 GiB).

### 6.2 OpenFGA saturates 2 vCPU while the origin idles

At the suite's default rate of **40 rps** (`rate_target_rps: 40`, `rate_achieved_rps`
39.9986 in `results/E1/w1/A-r1-*/summary.json`), the OpenFGA containers — capped at
`cpus: 2.0` — hit their ceiling, while the origin, capped at `cpus: 3.0`, did not:

| Container | Cap | Max observed CPU | Utilisation of cap |
|---|---|---|---|
| `fga-a` (baseline) | 2.0 | **204.73%** | saturated |
| `fga-c` (appcache) | 2.0 | **202.56%** | saturated |
| `fga-b` (via pgcache) | 2.0 | 199.26% | saturated |
| `fga-origin` | 3.0 | 187.11% | ~62% |

(`cpu_perc` in `docker stats` is relative to one core; 202% against `cpus: 2.0` is total
saturation.) This reorients the whole experiment: **the bottleneck was not the database.**
It is consistent with the amplification factor — ~132 datastore queries per Check at E1,
124–480 across workloads — where the per-query CPU cost inside OpenFGA dominates.

> Provenance note. `PLAN.md` §"Achado" states the origin sat at 4.8% CPU while OpenFGA
> saturated 2 vCPU, and the errata immediately below it invalidates that specific pair of
> figures, because `docker stats` was being sampled *after* the run with the system idle.
> The percentages in the table above are read directly from the per-run
> `docker-stats.csv` artifacts and are the ones to cite. The sampling defect is fixed in
> the code; the affected hypotheses (H1/H2/H3/H5) are back to "not measured".

On AKS this is why `openfga.resources.requests` is only `{cpu: 1, memory: 2Gi}` and why
that is not a contradiction: requests are a scheduling floor, not a cap. Three OpenFGA
pods sit on one 7820m node with no limits, and the one path under load can consume
essentially the whole node.

### 6.3 Origin and generator

- `origin`: `{cpu: 6, memory: 20Gi}`. `shared_buffers` stays pinned at **1 GB at every
  rung** — without that control the ladder would measure "how much RAM I gave Postgres",
  not scalability (`PLAN.md` §4). `wal_level=logical`, `max_replication_slots=10` and
  `max_wal_senders=10` are required by PgCache's CDC invalidation, not tuning.
  `track_io_timing=on` is what makes `blks_read` vs `blks_hit` reportable.
- `loadgen`: `{cpu: 3, memory: 8Gi}`, deployed as a bare **Pod, not a Job**. A Job with the
  benchmark embedded would couple the experiment's lifecycle to a Kubernetes object: an
  aborted run becomes a CrashLoop, and `kubectl delete job` would take the artifacts with
  it. Results live on an `emptyDir` and must be copied out before the pools go down:
  `kubectl -n pgcache-lab cp loadgen:/lab/results ./results-aks`.

---

## 7. Azure quota constraints

This is a pay-as-you-go subscription, and family quota — not regional vCPU count — is what
actually blocks SKU choices. Captured from `az vm list-usage -l brazilsouth`:

| Quota bucket | Current / Limit | Free |
|---|---|---|
| Total Regional vCPUs | 2 / 42 | 40 |
| **Total Regional Low-priority vCPUs** | **32 / 40** | 8 |
| Standard Dv5 Family vCPUs | 2 / 32 | 30 |
| Standard Ddsv6 Family vCPUs | 0 / 10 | 10 |
| Standard Edsv6 Family vCPUs | 0 / 10 | 10 |
| **Standard DASv5 Family vCPUs** | **0 / 0** | 0 |
| Standard DDSv5 Family vCPUs | 0 / 0 | 0 |
| Standard DSv5 Family vCPUs | 0 / 0 | 0 |
| Standard EDSv5 Family vCPUs | 0 / 0 | 0 |
| Standard Ev5 / EASv5 / EADSv5 Family vCPUs | 0 / 0 | 0 |

**103 of the enumerated VM families have limit 0** in `brazilsouth` on this subscription,
including most of the v5 line. Requesting `Standard_D8ds_v5` fails with `QuotaExceeded`
even though "Total Regional vCPUs" shows 40 free — the regional bucket is necessary but not
sufficient, the family bucket is checked too.

**Spot is charged to a different bucket.** The live cluster is the proof: `userpool` runs
four `Standard_D8as_v5` nodes, which belong to the **DASv5** family whose regular quota is
**0 / 0**. They exist because Spot instances count against *Total Regional Low-priority
vCPUs*, currently **32 / 40** — exactly 4 × 8 vCPU. That is the whole reason the lab can
run 32 vCPU of D8as_v5 on a subscription that cannot allocate a single on-demand DASv5 core.
It is also why the low-priority ceiling of 40 caps the shared pool: `maxCount: 5` × 8 vCPU
= 40, precisely at the limit.

Implications:

- The `sharedPool` mode (currently deployed) is bounded by *Total Regional Low-priority
  vCPUs* = 40.
- The `dedicatedPools` mode (`cluster.sh`) is bounded by family quota, which is why its
  SKUs are Ddsv6 / Edsv6 / Dv5 — the three families with real headroom that are also
  premium-storage capable where the origin disk needs it. Its budget: 8 + 8 + 4 + 4 = 24
  vCPU of on-demand capacity, plus the 2 already in use, against Total Regional 42.
- Verify before changing any SKU:
  `az vm list-usage -l brazilsouth -o table | grep -i 'family\|Total Regional'`.

---

## 8. Storage

The origin's PersistentVolume is **Premium SSD v2 with explicitly provisioned IOPS and
throughput**, created by `templates/storageclass.yaml` (the chart, not `cluster.sh` —
in `sharedPool` mode no node pools are created, so there would be nowhere else to put it,
and without the StorageClass the origin PVC never binds and the whole lab collapses in
cascade: origin `Pending` → pgcache without origin DNS → OpenFGA stuck "waiting for
database").

| Parameter | Value | Reason |
|---|---|---|
| `skuName` | `PremiumV2_LRS` | supports independently provisioned IOPS/throughput |
| `DiskIOPSReadWrite` | **8000** | fixed, citable experimental variable |
| `DiskMBpsReadWrite` | **400** | idem |
| `cachingMode` | `None` | Premium SSD v2 does not support host caching — platform constraint |
| `volumeBindingMode` | `WaitForFirstConsumer` | disk must be born in the same zone as the node that got the pod |
| `reclaimPolicy` | `Delete` | ephemeral lab |
| `allowVolumeExpansion` | `true` | |
| Size | 256 Gi | |

Live: PVC `data-origin-0` is `Bound` (256 Gi, RWO, StorageClass `origin-premiumv2`), and
the subscription reports `PremiumV2StorageDisks 1 / 5000`,
`PremiumV2TotalDiskSizeInGB 256 / 1048576`.

**Why it is provisioned explicitly rather than left to a default class.** At rung E3 the
tuple table is ~5.3 GB against a `shared_buffers` deliberately pinned at 1 GB. The origin
therefore becomes **I/O bound by design** — that is the point of the ladder, since the whole
premise of a cache in front of Postgres only has a chance once the working set stops fitting
in RAM. Once the origin is I/O bound, the disk stops being an infrastructure detail and
becomes an experimental variable. "8000 IOPS / 400 MB/s" is citable in a report; "whatever
the default StorageClass gave us" is not, and would silently vary with region, SKU and disk
size. Local NVMe was rejected for the same reason: the number would not be reproducible.

The cluster also ships ten built-in StorageClasses (including `managed-csi-premium-v2`);
the lab uses its own so the parameters are pinned in the chart and travel with the release.

---

## 9. Cost controls

| Control | State | Evidence |
|---|---|---|
| Control plane tier | **Standard → Free** | `sku: {name: Base, tier: Free}` |
| Container Insights (`omsagent`) | **disabled** | `addonProfiles.omsagent.enabled: false` |
| Azure Managed Prometheus | **disabled** | `azureMonitorProfile.metrics.enabled: false` |
| Azure Managed Grafana | **kept** (owner's choice) | `grafana-20260802160437`, SKU `Standard`, `brazilsouth`, RG `eks-01_group` |
| Azure Monitor workspace | present but not written to | `defaultazuremonitorworkspace-cq` in `eks-01_group`; AKS metrics scraping is off |
| Key Vault secrets provider addon | disabled | `addonProfiles.azureKeyvaultSecretsProvider.enabled: false` |
| Azure Policy addon | enabled | required by org config; relevant to §5 admission |
| `userpool` scale-to-zero | `minCount: 0` | pool costs nothing between campaigns |
| Spot pricing | `spotMaxPrice: -1` (pay up to on-demand) | eviction `Deallocate` |

Dropping the control plane from Standard to Free removes the uptime SLA and the higher
API-server scaling tier. For a 4–6 node ephemeral measurement cluster that is the correct
trade; it is worth stating in the report because control-plane throttling under a very
chatty client would be a confound, and this lab's clients are not chatty toward the API
server.

Container Insights and Managed Prometheus are redundant here: the lab already exports
OpenFGA metrics (`--metrics-enable-rpc-histograms`, `--datastore-metrics-enabled`), PgCache
metrics on port 9090, and `pg_stat_statements` from the origin, and the generator scrapes
those endpoints directly into `results/**`. Managed Grafana is retained by the owner's
explicit decision — it is a per-hour resource independent of the cluster's lifecycle, so it
survives `./cluster.sh pools-down`.

The single largest cost risk in this design is not any of the above: it is a **forgotten
node pool**. `cluster.sh pools-down` removes the Helm release, the namespace and the four
pools; in `sharedPool` mode the equivalent is letting the autoscaler take `userpool` back
to `minCount: 0`.

---

## 10. Known gaps

Facts a reader should have before citing any number produced on this platform.

1. **Image tags are floating.** `postgres:17`, `pgcache/pgcache:latest`,
   `openfga/openfga:latest`, `golang:1.24`. `latest` does not identify a build. Digests
   must be pinned before publishing; `results/**/versions.txt` exists to record them.
2. **Path B carries a documented confound.** Path B sets
   `default_query_exec_mode=simple_protocol` because without it PgCache classifies ~99.97%
   of queries as non-cacheable (pgx extended protocol). With it, literals are interpolated
   into the SQL text and the shape space grows with the cartesian product of the mass.
   Path B is therefore "path A without prepared statements, plus a cache" — not
   "path A plus a cache".
3. **The RTT artifact is mandatory and currently unrecorded.** `lab-params.env` is generated
   with `RTT_P50_MS` / `RTT_P99_MS` empty, and it is only written by `cluster.sh pools`,
   which has not been run in the deployed `sharedPool` mode.
4. **`NETWORK_MODE` in `lab-params.env` is also empty** and must be filled per campaign for
   the §5 comparison to be reconstructible.
5. **The origin password is a literal (`fgapass`) in `values.yaml`.** Acceptable for an
   ephemeral lab on a private network; an exposed cluster requires Workload Identity + Key
   Vault.
6. **kubelet `cpuCfsQuota: false` is not active** in the deployed `sharedPool` mode (§4).

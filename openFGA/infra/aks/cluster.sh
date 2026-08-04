#!/usr/bin/env bash
# Node pools dedicados do lab PgCache x OpenFGA, DENTRO DE UM CLUSTER EXISTENTE.
#
# Este script NAO cria nem destroi cluster. Ele so' adiciona e remove os node
# pools do lab no cluster que voce ja' tem.
#
#   ./cluster.sh preflight     o cluster serve para medir?
#   ./cluster.sh pools         cria os 4 node pools + StorageClass
#   ./cluster.sh rtt           mede o RTT entre os nos (artefato obrigatorio)
#   ./cluster.sh pools-down    remove os pools e o namespace
#
# O requisito de medicao nao e' "cluster dedicado", e' NO' DEDICADO: os pods do
# lab nao podem dividir no' com mais nada. Um node pool com taint resolve isso
# dentro de qualquer cluster.
set -euo pipefail
cd "$(dirname "$0")"

RG="${RG:-eks-01_group}"
CLUSTER="${CLUSTER:-eks-1}"
LOCATION="${LOCATION:-brazilsouth}"
NS="${NS:-pgcache-lab}"

# ── Topologia ───────────────────────────────────────────────────────────────
#
#   single_az  (default) todos os node pools na mesma zona.
#   cross_az   origem numa zona, PgCache + OpenFGA + gerador noutra.
#
# single_az NAO e' experimento degradado. No laptop, origem e PgCache eram
# containers no mesmo host: round-trip da ordem de 0,05 ms. Em nos separados
# dentro de uma zona e' NIC a NIC de verdade, tipicamente 0,2 a 0,5 ms — 4 a 10x
# mais. Multiplicado por 124-480 consultas por Check, ja' sao 25 a 240 ms por
# requisicao que o cache pode poupar.
#
# Zona 1 por default porque e' onde esta' o system pool do eks-1, e portanto o
# CoreDNS. Service headless resolve via CoreDNS — pool do lab noutra zona faria
# toda resolucao atravessar zona, somando latencia fora do que se quer medir.
TOPOLOGY="${TOPOLOGY:-single_az}"
ZONE="${ZONE:-1}"

case "$TOPOLOGY" in
  single_az) ZONE_ORIGIN="$ZONE"; ZONE_APP="$ZONE" ;;
  cross_az)  ZONE_ORIGIN="${ZONE_ORIGIN:-1}"; ZONE_APP="${ZONE_APP:-2}" ;;
  *) echo "topologia invalida: $TOPOLOGY (use single_az|cross_az)"; exit 2 ;;
esac

# ── SKUs ────────────────────────────────────────────────────────────────────
#
# Escolhidos pela QUOTA POR FAMILIA, nao so' pelo tamanho. Nesta subscription,
# em brazilsouth, a maioria das familias v5 tem limite ZERO: DDSv5 0/0,
# EDSv5 0/0, DSv5 0/0. Pedir D8ds_v5 falha com QuotaExceeded mesmo havendo folga
# no "Total Regional vCPUs".
#
# Familias com folga real E capazes de Premium Storage (necessario para o disco
# da origem): Ddsv6 (10) e Edsv6 (10). Para quem nao precisa de disco premium,
# Dv5 tem 28 livres.
#
#   origin   D8ds_v6   8 vCPU / 32 GiB  premium=True   -> Ddsv6  8/10
#   pgcache  E8ds_v6   8 vCPU / 64 GiB  premium=True   -> Edsv6  8/10
#   openfga  D4_v5     4 vCPU / 16 GiB                 -> Dv5
#   loadgen  D4_v5     4 vCPU / 16 GiB                 -> Dv5
#   total regional: 24 + 4 ja' em uso = 28/42
#
# Serie B esta' fora de proposito: burstable usa credito de CPU, e sob carga
# sustentada a medicao vira "o saldo acabou", nao "o sistema saturou".
#
# Confira sua quota antes de mudar qualquer um:
#   az vm list-usage -l brazilsouth -o table | grep -i 'family\|Total Regional'
SKU_ORIGIN="${SKU_ORIGIN:-Standard_D8ds_v6}"
SKU_PGCACHE="${SKU_PGCACHE:-Standard_E8ds_v6}"
SKU_OPENFGA="${SKU_OPENFGA:-Standard_D4_v5}"
SKU_LOADGEN="${SKU_LOADGEN:-Standard_D4_v5}"

# Disco da origem. Em E3 a tabela tem ~5,3 GB contra 1 GB de shared_buffers,
# entao a origem vira I/O bound POR DESENHO — o disco vira variavel do
# experimento. Premium SSD v2 com IOPS explicito, e nao NVMe local: o numero
# precisa ser citavel no relatorio, nao "o que a maquina deu".
DISK_IOPS="${DISK_IOPS:-8000}"
DISK_MBPS="${DISK_MBPS:-400}"

POOLS="origin pgcache openfga loadgen"

sku_for() {
  case "$1" in
    origin)  echo "$SKU_ORIGIN" ;;
    pgcache) echo "$SKU_PGCACHE" ;;
    openfga) echo "$SKU_OPENFGA" ;;
    loadgen) echo "$SKU_LOADGEN" ;;
  esac
}

zone_for() { [ "$1" = origin ] && echo "$ZONE_ORIGIN" || echo "$ZONE_APP"; }

# ── Kubelet: desliga o throttling de CFS ────────────────────────────────────
#
# Limite de CPU em pod e' cota CFS em janelas de 100 ms; pod que estoura fica
# congelado ate' a janela virar, e isso aparece como SPIKE DE p99 —
# indistinguivel da assinatura de registro de shape do PgCache (p50 empatado,
# p99 7x pior) que este lab justamente tenta isolar.
#
# A config e' POR NODE POOL: entra so' nos pools do lab, sem tocar nos seus.
write_kubelet_config() {
  cat > /tmp/kubelet-lab.json <<'JSON'
{
  "cpuManagerPolicy": "static",
  "cpuCfsQuota": false
}
JSON
}

preflight() {
  echo "==> cluster: $CLUSTER (rg $RG, $LOCATION)"
  echo "==> contexto kubectl: $(kubectl config current-context)"
  echo

  echo "--- hostNetwork liberado? ---"
  echo "    Checagem real (roda admission de verdade, inclusive Azure Policy):"
  echo "      helm template lab ./chart -n $NS | kubectl apply --dry-run=server -f -"
  echo "    Se passar sem 'admission webhook denied', o modo bare funciona."
  echo

  echo "--- quota por familia (o que mais barra) ---"
  az vm list-usage -l "$LOCATION" -o json 2>/dev/null | python3 -c "
import json,sys
alvo=('Ddsv6','Edsv6','Dv5 Family','Total Regional vCPUs','Low-priority')
for x in json.load(sys.stdin):
    n=x['localName']
    if any(a in n for a in alvo):
        c,l=int(x['currentValue']),int(x['limit'])
        print('    %-42s %4d / %4d   livre=%d'%(n,c,l,l-c))
" 2>/dev/null || echo "    (checagem falhou)"
  echo

  echo "--- CoreDNS (Service headless resolve por ele) ---"
  kubectl -n kube-system get deploy coredns -o jsonpath='    {.status.replicas} replicas{"\n"}' 2>/dev/null || true
  echo

  echo "--- nos existentes ---"
  kubectl get nodes -L agentpool,topology.kubernetes.io/zone 2>/dev/null
}

add_pool() {
  local name="$1" sku zone
  sku=$(sku_for "$name"); zone=$(zone_for "$name")
  echo "==> node pool $name ($sku, zona $zone)"
  az aks nodepool add \
    --resource-group "$RG" --cluster-name "$CLUSTER" \
    --name "$name" --node-count 1 --node-vm-size "$sku" --zones "$zone" \
    --mode User \
    --node-taints "role=$name:NoSchedule" \
    --labels "role=$name" \
    --kubelet-config /tmp/kubelet-lab.json \
    --max-pods 30 \
    --output none
}

pools() {
  echo "==> adicionando pools do lab em $CLUSTER, topologia $TOPOLOGY"
  write_kubelet_config
  for p in $POOLS; do add_pool "$p"; done

  # Premium SSD v2 exige zona e nao aceita cache de host — restricao da
  # plataforma, nao escolha.
  kubectl apply -f - <<YAML
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: origin-premiumv2
provisioner: disk.csi.azure.com
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
parameters:
  skuName: PremiumV2_LRS
  DiskIOPSReadWrite: "$DISK_IOPS"
  DiskMBpsReadWrite: "$DISK_MBPS"
  cachingMode: None
YAML

  cat > lab-params.env <<EOF
# Gerado por cluster.sh — copie para results/**/versions.txt
CLUSTER=$CLUSTER
LOCATION=$LOCATION
TOPOLOGY=$TOPOLOGY
ORIGIN_ZONE=$ZONE_ORIGIN
APP_ZONE=$ZONE_APP
SKU_ORIGIN=$SKU_ORIGIN
SKU_PGCACHE=$SKU_PGCACHE
SKU_OPENFGA=$SKU_OPENFGA
SKU_LOADGEN=$SKU_LOADGEN
ORIGIN_DISK_IOPS=$DISK_IOPS
ORIGIN_DISK_MBPS=$DISK_MBPS
NETWORK_MODE=
RTT_P50_MS=
RTT_P99_MS=
EOF

  echo
  echo "==> pronto. Parametros em infra/aks/lab-params.env"
  echo "    Proximo:  helm install lab ./chart -n $NS --create-namespace"
  echo
  echo "    LEMBRETE: ao terminar, ./cluster.sh pools-down."
  echo "    Node pool esquecido ligado e' o unico jeito deste lab custar caro."
}

# O experimento pendura neste numero, inclusive em single_az: sem o RTT medido
# nao da' para dizer quanto do resultado e' rede e quanto e' o cache.
rtt() {
  local origin_ip
  origin_ip=$(kubectl get nodes -l role=origin \
    -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
  echo "==> RTT do no' de openfga ate' a origem ($origin_ip), topologia $TOPOLOGY"

  kubectl run rtt-probe --rm -i --restart=Never --image=nicolaka/netshoot \
    --overrides="$(cat <<JSON
{"spec":{
  "hostNetwork": true,
  "nodeSelector": {"role":"openfga"},
  "tolerations": [{"key":"role","value":"openfga","effect":"NoSchedule"}]
}}
JSON
)" -- ping -c 200 -i 0.05 -q "$origin_ip"

  echo
  echo "Anote p50/p99 em lab-params.env (RTT_P50_MS / RTT_P99_MS)."
  echo "Referencia: mesmo host ~0,05 ms · mesma zona 0,2-0,5 ms · zonas distintas 1-3 ms."
}

pools_down() {
  echo "==> removendo release helm e namespace $NS"
  helm uninstall lab -n "$NS" 2>/dev/null || true
  kubectl delete namespace "$NS" --ignore-not-found --wait=false
  for p in $POOLS; do
    echo "==> removendo node pool $p"
    az aks nodepool delete --resource-group "$RG" --cluster-name "$CLUSTER" \
      --name "$p" --no-wait --output none 2>/dev/null || true
  done
  echo "==> em remocao. Confira com:"
  echo "    az aks nodepool list -g $RG --cluster-name $CLUSTER -o table"
}

case "${1:-}" in
  preflight)  preflight ;;
  pools)      pools ;;
  rtt)        rtt ;;
  pools-down) pools_down ;;
  *) cat <<USAGE
uso: ./cluster.sh <subcomando>

Opera sobre o cluster EXISTENTE $CLUSTER (rg $RG). Nao cria nem destroi cluster.

  preflight     checa se o cluster serve para medir
  pools         cria os 4 node pools do lab + StorageClass
  rtt           mede o RTT entre os nos (artefato obrigatorio)
  pools-down    remove release, namespace e node pools

Variaveis: TOPOLOGY=single_az|cross_az · ZONE=1 · RG · CLUSTER · LOCATION
           SKU_* · DISK_IOPS · DISK_MBPS
USAGE
     exit 2 ;;
esac

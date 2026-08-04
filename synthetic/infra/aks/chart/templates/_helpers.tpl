{{/*
Helpers que concentram as decisoes de medicao. Sao poucos de proposito: quanto
menos logica espalhada nos templates, menor a chance de os tres caminhos
diferirem em algo que nao seja a variavel sob teste.
*/}}

{{- define "lab.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
lab.benedet/network-mode: {{ .Values.networkMode }}
{{- end }}

{{/*
Colocacao. O requisito nao e' "um node pool por componente", e' UM POD POR NO:
origem, PgCache e OpenFGA nao podem dividir CPU, senao a medicao vira ruido.

Dois jeitos de conseguir isso:

  sharedPool     (default) um unico pool com autoscale, e anti-affinity entre
                 roles diferentes. O cluster autoscaler sobe os nos necessarios
                 para satisfazer a restricao. Menos infraestrutura para criar e
                 esquecer ligada.

  dedicatedPools um node pool por componente, cada um com seu taint. Util quando
                 os componentes precisam de SKUs diferentes.

Na anti-affinity, o operador e' NotIn no proprio role: cada pod repele pods de
role DIFERENTE no mesmo hostname, mas aceita os do mesmo role. E' o que permite
openfga-a, -b e -c dividirem um no' — a regra de ouro do lab e' um path sob carga
por vez, entao dois deles estao sempre ociosos.
*/}}
{{- define "lab.placement" -}}
{{- $root := .root -}}
{{- $role := .role -}}
{{- if eq $root.Values.placement.mode "sharedPool" }}
{{- with $root.Values.placement.sharedPool.nodeSelector }}
nodeSelector: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $root.Values.placement.sharedPool.tolerations }}
tolerations: {{- toYaml . | nindent 2 }}
{{- end }}
affinity:
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchExpressions:
            - key: lab.benedet/role
              operator: NotIn
              values: [{{ $role | quote }}]
        topologyKey: kubernetes.io/hostname
{{- else }}
nodeSelector:
  role: {{ $role }}
tolerations:
  - key: role
    value: {{ $role }}
    effect: NoSchedule
{{- end }}
{{- end }}

{{/*
Rede do pod. Em `bare`, hostNetwork tira kube-proxy, NAT e overlay do caminho de
dados; dnsPolicy precisa virar ClusterFirstWithHostNet, senao o pod usa o
resolv.conf do no' e nao enxerga os Services do cluster.
*/}}
{{- define "lab.podNetwork" -}}
{{- if eq .Values.networkMode "bare" }}
hostNetwork: true
dnsPolicy: ClusterFirstWithHostNet
{{- end }}
{{- end }}

{{/*
Rede do Service. Headless (clusterIP: None) faz o DNS devolver o IP do pod — que
com hostNetwork E' o IP do no' — entao a conexao vai direta. ClusterIP normal
passa por kube-proxy, que e' exatamente o que o modo `cni` quer medir.
*/}}
{{- define "lab.serviceClusterIP" -}}
{{- if eq .Values.networkMode "bare" }}
clusterIP: None
{{- end }}
{{- end }}

{{/*
URI do datastore de um caminho do OpenFGA. Concentrado aqui porque a unica
diferenca legitima entre os paths A e B e' o host — e, no caso do B, o
default_query_exec_mode, que e' o confundimento documentado em values.yaml.
*/}}
{{- define "lab.datastoreUri" -}}
{{- $root := .root -}}
{{- $p := .path -}}
{{- $o := $root.Values.origin -}}
{{- $ns := $root.Release.Namespace -}}
{{- if eq $p.datastore "pgcache" -}}
postgres://{{ $o.user }}:{{ $o.password }}@pgcache.{{ $ns }}.svc.cluster.local:{{ $root.Values.pgcache.sqlPort }}/{{ $o.database }}?sslmode=disable{{ if $p.execMode }}&default_query_exec_mode={{ $p.execMode }}{{ end }}
{{- else -}}
postgres://{{ $o.user }}:{{ $o.password }}@origin.{{ $ns }}.svc.cluster.local:5432/{{ $o.database }}?sslmode=disable{{ if $p.execMode }}&default_query_exec_mode={{ $p.execMode }}{{ end }}
{{- end -}}
{{- end }}

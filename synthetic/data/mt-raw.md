# Dados brutos — campanha multi-tenant (mt1), AKS, 2026-08-04

Esquema: 7 tabelas, 463 MB (cabe no shared_buffers de 1 GB — caso mais dificil).
tenant_id denormalizado em orders e order_item, indexado (tenant_id, created_at DESC)
e (tenant_id, status). 200 tenants, 2.500 pedidos por tenant.

Requisicao = /tmp/mt/dashboard.sql, 8 statements, todos filtrando por tenant_id:
  1 GROUP BY status; 2 sum+count; 3 count de customer; 4 pagina ORDER BY created_at
  DESC LIMIT 20 OFFSET; 5 count filtrado por status; 6 JOIN orders-customer;
  7 JOIN order_item-product + GROUP BY + ORDER BY; 8 JOIN + LIMIT + OFFSET.
Variabilidade por requisicao: tenant, offset de paginacao (10), status (4).

Escrita = /tmp/mt/write.sql, 1 statement: UPDATE orders via subquery indexada por
tenant_id. Mistura por -f script@peso do pgbench, autocommit, -M simple.

Topologia: 3 nos distintos do userpool (origin, pgcache, loadgen), mesma zona
brazilsouth-1. Caminhos diferem so' em host/porta.
Aquecimento: passes de 30 s repetidos ate' a taxa de acerto estabilizar
(delta < 1 ponto percentual entre passes consecutivos); coluna "warm" = quantos
passes foram necessarios.

## Custo da requisicao na origem, 1 cliente, sem concorrencia
latency average = 39.511 ms   tps = 25.31

## Varredura de tenants ativos (8 clientes)
tenants  A_lat_ms  A_rps  B_lat_ms  B_rps   hit     ganho_rps  warm
1        79.206    101    6.960     1149    100.0%  +1038%     2
10       81.181    99     6.998     1143    100.0%  +1055%     2
50       82.513    97     7.071     1131    100.0%  +1066%     2
200      83.265    96     7.085     1129    100.0%  +1076%     3

## Varredura de concorrencia (tenants=200)
clientes  A_lat_ms  A_rps  B_lat_ms  B_rps   hit     ganho_rps
1         32.229    31     5.069     197     100.0%  +535%
4         53.478    75     5.626     711     100.0%  +848%
8         83.052    96     7.088     1129    100.0%  +1076%
16        148.678   108    11.798    1356    100.0%  +1156%
32        290.625   110    23.350    1370    100.0%  +1145%

## Varredura de escrita (8 clientes, leitura sobre 200 tenants)
escrita_pct  tenants_com_escrita  A_lat_ms  A_rps  B_lat_ms  B_rps  hit     ganho_rps
0            200                  83.177    96     7.100     1127   100.0%  +1074%
5            1                    78.992    101    7.199     1111   100.0%  +1000%
5            10                   79.853    100    7.296     1096   100.0%  +996%
5            200                  79.776    100    7.495     1067   100.0%  +967%
10           1                    78.517    102    7.415     1079   100.0%  +958%
10           200                  77.882    103    7.546     1060   100.0%  +929%

## Contexto para comparacao (campanhas anteriores, ja' publicadas)
- Campanha s4, pgbench point-select: leitura 0.191 ms, escrita 4.879 ms.
  A 10% de escrita o ganho de vazao caiu de +37% para -2%.
- Sonda de formas (synthetic/RESULTS-shapes.md): latencia do PgCache plana em
  0.162-0.182 ms para todas as 10 formas; da origem 0.228-0.431 ms.
- openFGA: origem 39.5-42.2 us/query, PgCache 41.9-47.0 us/query.

## Nao medido nesta campanha
- Gate de correcao diferencial (w7/w8) NAO rodou para este workload.
- Percentis: so' latencia media do pgbench.
- Caminho C: nao existe, nao ha' aplicacao.
- Degraus com dados maiores que a memoria.

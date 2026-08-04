# Descobertas de integração PgCache × OpenFGA

Registro do que foi necessário para o PgCache efetivamente cachear a carga do
OpenFGA. Os dois primeiros itens são armadilhas silenciosas: em ambas o proxy
funciona, responde corretamente, reporta as consultas como *cacheable* — e nunca
cacheia nada. Nenhum erro, nenhum aviso, 100 % de miss para sempre.

---

## 1. `DISK_LIMIT` é comparado com o uso TOTAL do filesystem

**Sintoma.** `pgcache_queries_cacheable` sobe normalmente, mas
`pgcache_cache_queries_registered` fica em `0` indefinidamente. Todas as consultas
viram miss. Os *population workers* iniciam e ficam ociosos
(`pgcache_cache_queries_pending 0`, `..._loading 0`). Nada é logado.

**Diagnóstico.** As duas métricas que revelam o problema:

```
pgcache_cache_disk_limit_bytes   10737418240   ← DISK_LIMIT configurado (10 GiB)
pgcache_cache_disk_used_bytes    59538976768   ← 55 GiB
```

`disk_used_bytes` não é o tamanho do cache — é o uso do **volume inteiro**. Num host
com 55 GiB já ocupados, um `DISK_LIMIT` "razoável" de 10 GiB coloca o PgCache em
pressão de disco permanente já no boot, e ele recusa registrar qualquer consulta.

**Correção.** Não definir `DISK_LIMIT` (o default calcula a partir do espaço livre), ou
defini-lo acima do uso total observado do filesystem. Depois da correção, no mesmo
host e mesma carga:

```
pgcache_cache_disk_limit_bytes  474736566272
pgcache_cache_queries_registered        2865
pgcache_queries_cache_hit              12926
pgcache_queries_cache_miss              2921     → 81,6 % de hit ratio
```

**Verificação obrigatória antes de qualquer medição:**

```bash
curl -s localhost:19091/metrics \
  | grep -E 'cache_disk_(limit|used)_bytes|cache_queries_registered'
```

Se `used > limit`, o cache está desligado. Se `registered == 0` depois de carga, idem.

---

## 2. O modo de protocolo do `pgx` decide se há cache

O OpenFGA usa `pgx`, que por default fala **protocolo estendido** com cache de
statements (`default_query_exec_mode=cache_statement`). Nesse modo o PgCache
classifica praticamente tudo como não-cacheável:

```
pgcache_queries_total              615572
pgcache_queries_uncacheable        615370     ← 99,97 %
pgcache_queries_cacheable               1
pgcache_protocol_extended_queries  615371
```

Além de não cachear, o proxy fica **16× mais lento** que a origem direta
(414 ms vs 26 ms por check) — todo o custo do proxy, nenhum benefício.

Trocando para `default_query_exec_mode=simple_protocol` na URI do datastore:

```
pgcache_queries_total             7615
pgcache_queries_cacheable         7546      ← 99,1 %
pgcache_queries_uncacheable         53
pgcache_protocol_simple_queries   7615
pgcache_protocol_extended_queries    0
```

**Trade-off que precisa ficar explícito no relatório.** `simple_protocol` faz o `pgx`
interpolar os parâmetros no texto do SQL. Isso tem dois efeitos opostos:

- torna a consulta legível para o proxy, que passa a poder cacheá-la;
- elimina os *prepared statements* do lado do Postgres, então a origem volta a
  planejar cada consulta.

Ou seja: o path B não é "o path A mais um cache". É o path A **sem prepared
statements** mais um cache. Um benchmark honesto precisa dizer isso — e é por isso
que `PGX_EXEC_MODE` é uma variável do `.env`, não um valor cravado no código: dá
para medir o custo isolado da troca de protocolo rodando o path A nos dois modos.

---

## 3. A publicação de replicação é responsabilidade do operador

A documentação sugere que a publicação é gerenciada dinamicamente. Na prática a
própria imagem imprime no boot:

```
pgcache: Note: Ensure origin database has publication 'pgcache_fga_pub' created:
         CREATE PUBLICATION pgcache_fga_pub FOR ALL TABLES;
```

E no start seguinte:

```
NOTICE: publication "pgcache_fga_pub" does not exist, skipping
```

O `scripts/seed.sh` cria a publicação depois de subir o PgCache. Sem CDC não há
invalidação, e um cache de autorização sem invalidação é um problema de segurança,
não de performance.

---

## 4. A imagem embute o próprio PostgreSQL

`docker inspect pgcache/pgcache:latest` mostra `PG_MAJOR=18`. O store de cache é um
PostgreSQL 18 rodando dentro do mesmo container, em `127.0.0.1:5433`. **Não existe
serviço `cache-pg` separado** — o lab do Mattermost (`../../pgCache-bench`) assume
que existe, e essa suposição está errada. Consequência prática: o container do
PgCache precisa de CPU e RAM de banco de dados, não de proxy.

---

## 5. O custo de cache frio é real e precisa entrar na janela de aquecimento

Depois de um reseed, o PgCache reinicia com o cache vazio e **registra** cada shape
nova enquanto serve. Registrar é caro: no E0, um W7 de 20 000 pares que leva
segundos no path A não terminou em vários minutos no path B com cache frio. Depois
de aquecido, o mesmo path chega a 87 % de hit ratio.

Por isso o protocolo exige aquecimento antes de abrir a janela de análise, e por
isso o cenário S4 do lab irmão (custo de re-aquecimento) é um cenário separado, não
um detalhe.

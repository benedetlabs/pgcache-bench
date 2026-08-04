# Sonda de formas de query — o que é cacheável, e quanto vale

**Rodou:** 2026-08-04, AKS `eks-1`, esquema com formato de aplicação (7 tabelas,
463 MB, cabe no `shared_buffers` de 1 GB — caso mais difícil). 8 clientes,
`-M simple`, dois caminhos que diferem só no host.

Primeiro passo do plano de simular uma aplicação real: antes de montar requisições
compostas, medir cada forma de query **isolada**. Se alguma não for cacheável, é
melhor descobrir em vinte minutos do que depois de uma campanha de três horas.

---

## Resposta curta

**Nenhuma forma é incacheável, e todas ganham.** Join, `LEFT JOIN`, `GROUP BY`,
`COUNT(*)`, `EXISTS`, `IN (...)`, `ORDER BY … LIMIT` — todas atingem 100% de
acerto e todas superam a origem.

| forma | origem | PgCache | ganho |
|---|---:|---:|---:|
| `q01` lookup por PK | 0,228 ms · 35.137 tps | 0,165 ms · 48.409 tps | +38% |
| `q03` `COUNT(*)` com WHERE | 0,241 · 33.262 | 0,162 · 49.245 | +48% |
| `q08` `ORDER BY … LIMIT 20` | 0,258 · 31.039 | 0,174 · 45.990 | +48% |
| `q06` `GROUP BY` + agregado | 0,259 · 30.874 | 0,168 · 47.507 | +54% |
| `q07` `IN (10 ids)` | 0,261 · 30.606 | 0,182 · 44.022 | +44% |
| `q09` `LEFT JOIN` | 0,292 · 27.386 | 0,173 · 46.247 | +69% |
| `q04` join de 2 tabelas | 0,330 · 24.266 | 0,171 · 46.681 | **+92%** |
| `q10` `EXISTS` correlacionado | 0,392 · 20.426 | 0,176 · 45.467 | **+123%** |
| `q05` join de 3 tabelas | 0,431 · 18.560 | 0,179 · 44.709 | **+141%** |
| `q02` listagem por FK + LIMIT | 0,429 · 18.643 | 0,163 · 49.097 | **+163%** |

## O mecanismo

Olhe a coluna do PgCache de cima a baixo: **0,162 a 0,182 ms**. Doze por cento de
variação entre a query mais barata e a mais cara do conjunto.

Agora a coluna da origem: **0,228 a 0,431 ms**. Oitenta e nove por cento.

> O PgCache cobra um preço praticamente fixo por resposta, seja ela um lookup de
> chave primária ou um join de três tabelas. A origem cobra pelo trabalho. O ganho
> é, quase exatamente, quanto trabalho a query custa à origem.

Ordenada por custo na origem, a tabela acima tem ganho monotonicamente crescente.
Isso é a versão medida do C9, e é a primeira vez que a plataforma vê o mecanismo
isolado forma a forma.

**Consequência para o desenho dos cenários:** uma requisição real é feita das
formas caras — join, paginação, `COUNT(*)` de total. O lookup por PK, onde todo o
laboratório viveu até agora, é o **pior** caso do conjunto.

---

## Dois defeitos nossos que esta sonda expôs

### 1. `helm --set` engoliu a lista de tabelas, em silêncio

A primeira execução deu **0,0% de acerto em todas as dez formas**, inclusive no
lookup por chave primária. Isso não era resultado — era configuração.

`helm --set` e `--set-string` quebram vírgulas não escapadas em atribuições
separadas, então `pgcache.allowedTables=product,category,...` foi descartado
inteiro: `helm get values` voltou vazio e o deployment manteve as tabelas do
pgbench. O PgCache estava funcionando perfeitamente e recusando cachear tabelas
que ninguém tinha liberado.

Isto **já estava registrado como defeito** no laboratório do OpenFGA, e pisei nele
de novo. A correção durável não é escapar a vírgula — é a lista morar no
`values.yaml`, onde fica gravada no release e não pode ser digitada errado.

### 2. Aquecimento insuficiente, pela terceira vez (D-24)

A segunda execução, já com as tabelas liberadas, produziu uma tabela plausível e
errada: `q01` com 19,5% de acerto e perdendo 42%, enquanto `q02` fazia 100% e
ganhava 164%. A leitura natural seria "algumas formas cacheiam melhor que outras".

Era aquecimento. Um passe de 15 s não aquece 20.000 entradas distintas. Com dois
passes de 20 s o mesmo `q01` no mesmo keyspace vai a **100% de acerto e +38%**.

Verificado que não era capacidade: zero evicções, 8,4 GB usados de 26,9 GB de
orçamento. E o keyspace varrido isoladamente confirma o padrão — 500, 5.000 e
20.000 chaves todos chegam a 100%; só em 100.000 o acerto cai para 44%.

O ranking da segunda execução media **velocidade de aquecimento**, não
cacheabilidade. É exatamente o defeito que retratou nove células da campanha r5 do
OpenFGA, e a terceira vez que ele aparece.

**A regra que fica:** nenhuma célula de caminho B vale nada sem prova de que a
taxa de acerto estabilizou. Passe fixo de aquecimento é palpite disfarçado de
protocolo.

---

## O que isto não diz

- **Ainda não é uma requisição.** São formas isoladas. A composição — dez a quinze
  statements com formas misturadas, medidos como uma latência só — é o passo
  seguinte, e é onde a amplificação multiplica o efeito para os dois lados.
- **Zero escrita.** A campanha s4 mostrou que a 10% de escrita a vantagem de vazão
  desaparece. Esta sonda é leitura pura e precisa ser lida junto com aquela.
- **Sem caminho C.** Não há aplicação, então não há cache de aplicação para
  comparar.

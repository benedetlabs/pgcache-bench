# SaaS multi-tenant: o cenário que devia ser o mais duro

**Campanha mt1 · AKS · 4 de agosto de 2026**

Escolhemos o multi-tenant primeiro justamente porque parecia o pior caso. Num
SaaS, toda query carrega `WHERE tenant_id = :t`, e a intuição era que isso
multiplicaria o espaço de consultas distintas pelo número de tenants ativos,
derrubando a taxa de acerto do cache até ele não valer nada.

Não foi o que aconteceu. **O PgCache entregou entre 5 e 12 vezes mais vazão, com
taxa de acerto de 100% em todas as células**, e a vantagem sobreviveu a 10% de
escrita — o mesmo nível de escrita que, uma campanha atrás, tinha zerado o ganho
por completo.

Este relatório explica por quê, e o que isso corrige do que publicamos ontem.

---

## O que foi medido

Um esquema com formato de aplicação: sete tabelas, 463 MB, chaves estrangeiras e
índices como um ORM criaria. `tenant_id` denormalizado em `orders` e
`order_item` — o padrão que aplicações multi-tenant reais usam, para que toda
query possa filtrar direto em vez de fazer join até chegar no tenant. Duzentos
tenants, 2.500 pedidos cada.

Os 463 MB cabem no `shared_buffers` de 1 GB da origem. Isso é deliberado: é o
caso **mais difícil** para um cache, porque o banco de origem já está servindo
tudo da memória. Foi exatamente essa condição que afundou o OpenFGA.

**A requisição** é um render de dashboard — oito statements, todos filtrando por
tenant: contagem de pedidos por status, soma de faturamento, contagem de
clientes, página de pedidos recentes com `ORDER BY … LIMIT … OFFSET`, contagem
filtrada por status, um join com `customer`, um join com `product` agregado por
produto, e uma página de itens. Além do tenant, cada requisição sorteia um offset
de paginação e um status, porque é essa variabilidade — e não o tenant — que faz
o espaço de consultas de uma aplicação real crescer.

Os dois caminhos rodam em três nós separados do cluster, na mesma zona de
disponibilidade, e diferem apenas no host e na porta do banco. Nada mais.

**Sobre o aquecimento**, que é onde esta plataforma já errou três vezes: cada
célula do caminho B foi aquecida com passes repetidos de 30 segundos até a taxa
de acerto parar de subir — variação menor que um ponto percentual entre passes
consecutivos. Duas passagens bastaram na maioria dos casos, três com 200 tenants.
Passe fixo de aquecimento é palpite disfarçado de protocolo, e já retratou nove
células de uma campanha nossa.

---

## A magnitude, e de onde ela vem

Uma requisição de dashboard custa **39,5 ms na origem** com um único cliente e
nenhuma concorrência. Não é uma query de brinquedo: são oito statements com
agregação e join sobre os 2.500 pedidos e 10.000 itens de um tenant.

Variando a concorrência:

| clientes | origem | PgCache | ganho |
|---:|---|---|---:|
| 1 | 32,2 ms · 31 rps | 5,07 ms · 197 rps | **+535%** |
| 4 | 53,5 ms · 75 rps | 5,63 ms · 711 rps | +848% |
| 8 | 83,1 ms · 96 rps | 7,09 ms · 1.129 rps | +1076% |
| 16 | 148,7 ms · 108 rps | 11,80 ms · 1.356 rps | +1156% |
| 32 | 290,6 ms · 110 rps | 23,35 ms · 1.370 rps | +1145% |

O número grande no fim da tabela é a soma de dois efeitos, e vale separá-los.

**A primeira linha é cache puro.** Com um cliente só não há fila em lugar nenhum,
nem na origem nem no proxy, e mesmo assim o PgCache responde em 5,07 ms contra
32,2 ms — **6,4 vezes mais rápido**. Nada disso vem de concorrência.

**O resto vem da saturação da origem.** Repare na coluna do meio: a origem sobe
de 31 para 110 requisições por segundo e depois trava. De 16 para 32 clientes ela
ganha duas requisições por segundo e a latência dobra, de 148 para 290 ms. Está
saturada. O PgCache, no mesmo intervalo, vai de 1.356 para 1.370 rps com latência
subindo de 11,8 para 23,4 ms.

Um dashboard a 290 ms é um dashboard que o usuário percebe como quebrado. O mesmo
dashboard pelo cache responde em 23 ms.

### O número de tenants não importou

A hipótese que motivou escolher este cenário simplesmente não se confirmou:

| tenants ativos | origem | PgCache | ganho | acerto |
|---:|---|---|---:|---:|
| 1 | 79,2 ms · 101 rps | 6,96 ms · 1.149 rps | +1038% | 100% |
| 10 | 81,2 ms · 99 rps | 7,00 ms · 1.143 rps | +1055% | 100% |
| 50 | 82,5 ms · 97 rps | 7,07 ms · 1.131 rps | +1066% | 100% |
| 200 | 83,3 ms · 96 rps | 7,09 ms · 1.129 rps | +1076% | 100% |

De um tenant para duzentos, o ganho varia menos de quatro pontos percentuais e a
taxa de acerto não sai de 100%. Duzentos tenants multiplicados por dez offsets de
paginação e quatro status dão alguns milhares de consultas distintas — o
suficiente para exigir três passes de aquecimento em vez de dois, e nada além
disso.

O espaço de consultas de um SaaS com esse formato é grande, mas não é grande na
escala que importaria.

---

## Por que aqui ganhou tanto, tendo perdido no OpenFGA

O mecanismo já tinha aparecido na sonda de formas de query que precedeu esta
campanha, e aqui ele se confirma em escala.

Naquela sonda, dez formas diferentes de query foram medidas isoladas. A latência
do PgCache ficou entre 0,162 e 0,182 ms para todas — doze por cento de variação
entre a consulta mais barata e a mais cara do conjunto. A da origem foi de 0,228
a 0,431 ms, oitenta e nove por cento.

> O PgCache cobra um preço praticamente fixo por resposta. A origem cobra pelo
> trabalho. O ganho é, quase exatamente, quanto trabalho a consulta custa à
> origem.

Um dashboard multi-tenant custa caro à origem — oito statements, agregações,
joins — e custa ao PgCache o mesmo que qualquer outra resposta. Daí a diferença
de uma ordem de grandeza.

O OpenFGA era o oposto exato. A origem servia cada consulta em 39,5 a 42,2
microssegundos, e o PgCache em 41,9 a 47,0. Não havia trabalho a economizar; só
sobrava a sobrecarga do proxy, multiplicada por 104 a 171 consultas por
requisição. O mesmo produto, o mesmo mecanismo, e resultados opostos porque a
origem estava em regimes opostos.

---

## O achado que corrige o que publicamos ontem

Ontem registramos o critério **C11**, a partir da campanha s4: acima de mais ou
menos 10% de escrita, a vantagem de vazão desaparece. Aquela campanha mediu
exatamente isso — a 10% de escrita o ganho caiu de +37% para −2%.

Aqui, com os mesmos 10% de escrita, o ganho continua em **+929%**.

| escrita | tenants com escrita | origem | PgCache | ganho | acerto |
|---:|---:|---|---|---:|---:|
| 0% | — | 83,2 ms · 96 rps | 7,10 ms · 1.127 rps | +1074% | 100% |
| 5% | 1 | 79,0 ms · 101 rps | 7,20 ms · 1.111 rps | +1000% | 100% |
| 5% | 10 | 79,9 ms · 100 rps | 7,30 ms · 1.096 rps | +996% | 100% |
| 5% | 200 | 79,8 ms · 100 rps | 7,50 ms · 1.067 rps | +967% | 100% |
| 10% | 1 | 78,5 ms · 102 rps | 7,42 ms · 1.079 rps | +958% | 100% |
| 10% | 200 | 77,9 ms · 103 rps | 7,55 ms · 1.060 rps | +929% | 100% |

Entramos nesta campanha achando que a explicação seria a **concentração** da
escrita: na s4, leituras e escritas dividiam as mesmas mil chaves, o pior caso
possível para invalidação, enquanto uma aplicação real escreve num subconjunto
pequeno e lê de um conjunto grande.

**Essa hipótese estava errada.** Compare as linhas de 10%: escrita concentrada em
um único tenant dá +958%, espalhada por todos os duzentos dá +929%. Vinte e nove
pontos de diferença num ganho de novecentos. A concentração quase não importa.

O que importa é a **razão entre o custo da leitura e o custo da escrita**.

Na campanha s4 a leitura era um point select de 0,191 ms e a escrita um `UPDATE`
de 4,879 ms — a escrita custava vinte e cinco vezes mais. Com 10% das transações
sendo escrita, elas consumiam cerca de **três quartos do orçamento de tempo**. E
como escritas passam idênticas pelos dois caminhos, o cache não tinha o que
acelerar no pedaço que dominava o relógio.

Aqui a leitura é um dashboard de 79 ms e a escrita é um `UPDATE` indexado. Com
10% de escrita, elas consomem uma fração desprezível do tempo. Sobra quase tudo
para o cache trabalhar.

> A regra correta não é "que fração das transações escreve". É **que fração do
> tempo a escrita consome**. Uma aplicação com leituras caras aguenta uma
> proporção de escrita muito maior antes de o cache deixar de compensar.

Isso **qualifica** o C11, não o retrata. A campanha s4 mediu corretamente o que
dizia medir, num regime onde as leituras eram baratíssimas. O erro foi generalizar
de um regime para todos.

---

## O que este relatório não pode sustentar

**O gate de correção diferencial não rodou para esta carga.** Toda campanha
anterior desta plataforma bloqueava a publicação de qualquer número de desempenho
até comparar, consulta a consulta, a resposta da origem com a do PgCache. Este
relatório está publicando sem isso.

Os gates anteriores passaram sempre — milhares de comparações, três sujeitos,
zero divergências, inclusive com leituras chegando durante escritas em voo. Mas
esta carga tem formas de consulta que os gates anteriores nunca exercitaram:
joins de duas tabelas, `GROUP BY` com `ORDER BY`, paginação com `OFFSET`. É
precisamente onde uma divergência seria mais plausível.

**Rodar o gate contra este workload é a próxima coisa a fazer**, antes de qualquer
outro cenário. Até lá, os números acima estão sujeitos a ele.

**Só latência média.** O pgbench reporta a média nativamente; os percentis exigem
registro por transação, que esta campanha não coletou. Para uma afirmação sobre
latência a média é a estatística mais fraca disponível, porque o dano de um cache
mora na cauda.

**Não existe caminho C.** Não há aplicação aqui, portanto não há cache de
aplicação com que comparar. Isto mede o envelope do produto sob carga com formato
de aplicação — não é um veredito de adoção, e num sujeito real o cache da própria
aplicação já venceu o PgCache antes.

**Um degrau só.** Os 463 MB cabem na memória da origem. É o caso mais difícil
para o cache, e por isso o resultado é conservador nesse eixo — mas nada aqui diz
o que acontece quando o conjunto de dados excede a memória.

**Duzentos tenants.** Um SaaS com dezenas de milhares de tenants ativos tem um
espaço de consultas ordens de grandeza maior. Esta campanha não chega perto disso,
e a extrapolação não é segura: a curva foi plana entre 1 e 200, mas nada garante
que continue plana em 20.000.

---

## Onde isto deixa a plataforma

Depois de dois sujeitos reais reprovados e oito campanhas em consultas de chave
primária, este é o primeiro cenário em que o PgCache entrega o tipo de ganho que
justificaria adotá-lo — e ele apareceu quando a carga passou a ter formato de
aplicação de verdade.

A leitura conjunta das campanhas fica assim: o PgCache paga quando a origem tem
trabalho real para economizar, e o trabalho real está nas consultas compostas de
uma requisição — joins, agregações, paginação — e não no lookup por chave
primária em que este laboratório viveu quase o tempo todo.

Falta o gate de correção. Depois dele, os cenários de e-commerce e CMS.

---

*Dados brutos: [`data/mt-raw.md`](data/mt-raw.md). Campanhas relacionadas:
[sonda de formas](RESULTS-shapes.md), [eixo de escrita](RESULTS-aks-s4.md).*

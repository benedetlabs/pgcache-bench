# Campanhas s3/s4 — escrita, percentis, e a correção de duas conclusões nossas

**Rodou:** 2026-08-04, AKS `eks-1`, namespace `pgcache-synth`, mesmo degrau P0
(escala 10, 150 MB), mesma topologia de três nós das campanhas
[s1](RESULTS-aks-s1.md) e [s2](RESULTS-aks-s2.md). 2 repetições, 45 s por célula.

Este par de campanhas fechou o maior buraco da plataforma — o custo da coerência
sob escrita — e adicionou percentis, que faltavam em tudo que publicamos antes.

**Duas conclusões anteriores caem aqui.** Uma delas é a manchete das campanhas
s1 e s2.

---

## 1. O resultado principal: a vantagem some com 5% de escrita

Mistura de leitura e escrita via `@weight` do pgbench, ambos os scripts em
autocommit, escrevendo na **mesma** faixa de chaves que as leituras consultam —
escrever em outro lugar daria um número lisonjeiro e sem sentido, porque o CDC
não teria nada em cache para invalidar.

| escrita | origem tps | PgCache tps | tps | p99 leitura A | p99 leitura B | p99 | acerto | lag CDC |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 0% | 36.376 | 49.690 | **+37%** | 0,313 ms | 0,264 ms | **−16%** | 98,6% | 0 |
| 5% | 19.175 | 20.120 | +5% | 0,294 ms | 0,484 ms | **+65%** | 98,8% | 155 KB |
| 10% | 12.573 | 12.308 | **−2%** | 0,300 ms | 0,511 ms | **+70%** | 98,7% | 102 KB |
| 30% | 4.784 | 4.738 | −1% | 0,312 ms | 0,561 ms | +80% | 98,4% | 225 KB |
| 50% | 2.910 | 2.794 | −4% | 0,319 ms | 0,576 ms | +81% | 98,1% | 202 KB |

Duas leituras, e a segunda é a mais importante.

### A vantagem de throughput desaparece

**+37% sem escrita, +5% com 5% de escrita, e negativa a partir de 10%.**

O mecanismo é aritmético e não tem nada de sutil: nesta bancada um `UPDATE` custa
cerca de 25 vezes o que custa um point select (4,879 ms contra 0,191 ms medidos
diretamente). Numa mistura 90/10 as escritas consomem a maior parte do tempo — e
**escritas passam direto pelos dois caminhos, idênticas**. O PgCache não as
acelera nem as atrasa; elas simplesmente afogam qualquer ganho do lado da
leitura.

A vazão total cai de 36.376 para 12.573 no próprio caminho A quando 10% das
transações viram escrita. O gargalo deixou de ser a leitura.

### A leitura pelo PgCache fica mais lenta sob escrita — a origem não

Esta é a medição que a plataforma nunca tinha feito, e é o custo da coerência
aparecendo.

Olhe as duas colunas de p99 de leitura. **O caminho A é plano**: 0,294 a
0,319 ms, seja com 0% ou 50% de escrita. A origem não se importa com a carga de
escrita quando serve uma leitura de chave primária.

**O caminho B não é plano.** Vai de 0,264 ms (mais rápido que a origem, sem
escrita) para 0,576 ms — uma penalidade de **+81%** no p99 da leitura.

E a taxa de acerto **fica em 98%** o tempo todo. Ou seja: não é o CDC despejando
entradas do cache. As entradas continuam lá e continuam sendo servidas. O que
acontece é que servi-las passa a custar mais enquanto o fluxo de invalidação
corre em paralelo.

> **O que isso significa na prática.** Todo número publicado nas campanhas s1 e
> s2 — inclusive os +176% de vazão a 256 clientes — foi medido com **0% de
> escrita**. Numa proporção OLTP completamente comum, 10% de escrita, a vantagem
> de vazão desaparece e a latência de leitura no p99 piora 70%.
>
> Isso não invalida s1 e s2: aquelas campanhas mediram o que diziam medir. Mas
> qualquer leitura delas isolada desta aqui é uma leitura enganosa.

---

## 2. Correção sob escrita concorrente — o gate passou

O portão w7 das campanhas anteriores checa correção **depois** que uma rajada de
escrita assentou. É o caso fácil. O caso difícil — e o único em que um cache
coerente por CDC pode de fato ser pego servindo dado velho — é a leitura que
chega **enquanto** as escritas estão em voo.

**Método**, porque comparar dois hosts sob mutação concorrente exige cuidado: lê
a origem, lê o PgCache, lê a origem **de novo**. Se as duas leituras da origem
concordam, a linha ficou estável durante a janela e a resposta do PgCache
*precisa* bater. Se discordam, a linha mudou no meio e a amostra é
**inconclusiva** — contada e reportada, nunca pontuada como divergência.

Isso mantém o teste honesto nos dois sentidos: ele não consegue produzir uma
divergência falsa, e não consegue esconder uma verdadeira.

O escritor é **limitado por taxa** de propósito (50/s sobre 1.000 chaves). Sem
limite ele mudaria toda chave dentro de toda janela, toda amostra sairia
inconclusiva, e o teste passaria por nunca concluir nada.

| lote | conclusivas OK | divergentes | inconclusivas |
|---|---:|---:|---:|
| 1 | 393 | 0 | 7 |
| 2 | 382 | 0 | 18 |
| 3 | 388 | 0 | 12 |
| 4 | 381 | 0 | 19 |
| 5 | 387 | 0 | 13 |
| **total** | **1.931** | **0** | **69** |

**1.931 comparações conclusivas, zero divergências.**

As 69 inconclusivas são a prova de que o teste não foi vazio: elas são chaves que
o escritor de fato alterou dentro da janela de amostragem. Um resultado com zero
inconclusivas significaria que o escritor não estava tocando a faixa amostrada.

Métricas de CDC ao final da janela: lag de 33.944 bytes, `staleness` de
**2,876 s**.

**Um detalhe que não sei explicar e por isso registro:** o contador
`pgcache_cache_invalidations` marcou **0** durante toda a janela, apesar de 6.000
`UPDATE`s terem sido aplicados e do lag de CDC ser diferente de zero. Ou o
contador mede um tipo específico de invalidação que este caminho não usa, ou as
entradas estão sendo atualizadas no lugar em vez de invalidadas. Não confirmei
qual, e não vou inventar. O que está medido é o comportamento observável: as
respostas batem.

---

## 3. A inferência do w5 estava errada

A campanha s2 publicou uma **inferência** sobre o colapso em `span=10000`: que o
portão de materialização estaria recusando as entradas. Instrumentamos
`pgcache_cache_mv_gate` para transformar a inferência em fato.

Ela estava errada.

| span | origem | PgCache | acerto | `mv_admit` | `mv_reject` |
|---:|---:|---:|---:|---:|---:|
| 1 | 0,291 ms | **0,164 ms** | 99,4% | 0 | **1000** |
| 10 | 0,290 ms | 0,404 ms | 98,8% | 1000 | 0 |
| 100 | 0,306 ms | 0,401 ms | 99,0% | 1000 | 0 |
| 1.000 | 0,488 ms | **0,402 ms** | 98,9% | 1000 | 0 |
| 10.000 | 2,216 ms | **769,888 ms** | **15,2%** | **118** | **0** |

Duas coisas que a inferência errava, e a segunda é mais interessante que a
primeira.

**O portão rejeita onde o PgCache GANHA.** Em `span=1` ele recusa as 1.000
entradas — e é exatamente aí que o PgCache é mais rápido (0,164 contra
0,291 ms). Recusar materializar significa servir pelo caminho de cache simples,
que é barato. A rejeição do portão é uma decisão boa, não um sintoma.

**Em `span=10000` o portão não rejeita nada.** `mv_reject = 0`. Ele **admite
118** de 1.000 — e as outras 882 não aparecem em nenhum dos dois contadores.

Então o colapso não é uma política recusando trabalho. É consistente com uma
**fila de construção que não dá conta**: 882 entradas ficam pendentes, nunca
completam dentro da janela, e toda consulta por elas é um miss. Daí os 15,2% de
acerto.

Isso continua sendo inferência — mas agora é uma inferência com o candidato
anterior **eliminado por medição**. Confirmar exige registrar
`pgcache_cache_mv_build_queue` por célula, o que esta campanha não fez.

O comportamento de custo plano entre `span` 10 e 1.000 fica explicado: naquela
faixa o portão admite tudo, o PgCache serve a partir da view materializada, e o
custo dela não depende de quantas linhas a origem teria que varrer.

---

## 4. Percentis — o conserto de rigor

As campanhas s1 e s2 reportaram **latência média**, porque é o que o pgbench
imprime. Para uma afirmação sobre latência a média é a estatística mais fraca
disponível: o dano de um cache mora na cauda, já que um miss custa várias vezes
um hit.

Agora todas as células registram p50, p95 e p99, amostrando 1% das transações via
`--log --sampling-rate`.

**Duas decisões de método que mudam o que os números significam:**

**Os percentis são só do script de leitura.** Com escrita na mistura, um p99 não
filtrado descreveria o flush do WAL, não o cache — e como as escritas passam
idênticas pelos dois caminhos, incluí-las comprimiria a diferença A/B por um
motivo que não tem nada a ver com caching. Na tabela do w6, `p99 leitura` é o
script de leitura isolado; a coluna de tps é a mistura inteira.

**Percentis com menos de 200 amostras não são reportados.** A campanha s3 pegou
isso do jeito honesto: a célula `span=10000` roda a 10 tps, então 45 s a 1% deram
cerca de quatro amostras, e o "p99" saiu em 296 ms contra uma **média de 794 ms**.
Um p99 abaixo da média é aritmeticamente impossível para latência, e é o sinal de
que a amostra não significa nada. Agora imprime `-`.

**E a boa notícia:** onde a amostragem é adequada, o p99 acompanha a média de
perto. No w5 sem escrita, a origem faz 0,291 ms de média e 0,463 de p99; o
PgCache faz 0,164 e 0,299. As conclusões de s1 e s2, tiradas de médias,
sobrevivem no p99.

---

## 5. Um defeito nosso, para o registro

As células do w6 na campanha s3 saíram com colunas desalinhadas e foram
descartadas. Com mais de um script `-f`, o pgbench imprime um bloco por script
**além** do resumo, cada um com sua própria linha `- latency average = ...`. O
padrão do parser não estava ancorado no início da linha, casou com as três, e a
variável virou uma string multilinha que quebrou o `printf` em silêncio.

Não disparou a guarda de valor vazio, porque uma string multilinha não é vazia.
Corrigido com âncora de início de linha e `exit` no primeiro casamento; o w6 foi
refeito do zero como campanha s4.

---

## 6. O que muda na leitura geral

Antes destas campanhas o resumo era: *o PgCache paga quando `H < O` e a taxa de
acerto passa do break-even, e o ganho cresce com a carga.*

Continua verdade — **para cargas de leitura**. O que faltava:

> A proporção de escrita é uma condição tão eliminatória quanto a taxa de acerto.
> A 10% de escrita, uma proporção OLTP banal, a vantagem de vazão desaparece e a
> latência de leitura no p99 piora 70%, mesmo com 98% de acerto.

Onde o PgCache continua entregando muito: cargas dominadas por leitura contra uma
origem sob pressão. Onde ele não entrega: qualquer coisa com escrita
significativa na mesma faixa de dados.

---

## O que continua sem medição

- **Escrita e leitura em faixas separadas.** Aqui as duas dividem as mesmas 1.000
  chaves de propósito, que é o pior caso para invalidação. Uma aplicação real
  costuma escrever num subconjunto e ler de um conjunto maior. Esse gradiente não
  foi varrido, e é o experimento mais promissor que sobrou.
- **`mv_build_queue`**, que confirmaria ou derrubaria a nova explicação do
  precipício do w5.
- **`pgcache_cache_invalidations` marcando zero** sob 6.000 UPDATEs. Anomalia de
  instrumentação ou comportamento real — não sei.
- **Degraus P1 e P2**, com dados maiores que a memória.
- **Caminho C**, permanentemente. Não há aplicação aqui.

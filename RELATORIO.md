# Vale a pena colocar um cache na frente do Postgres?

**Relatório de um laboratório de benchmark · agosto de 2026**

Este documento conta o que fizemos, o que deu errado, o que tivemos que retratar
e o que finalmente medimos. É a versão para ler de ponta a ponta. Os números
brutos e o detalhe de cada campanha estão nos relatórios individuais, linkados ao
longo do texto.

---

## 1. A pergunta

O PgCache é um cache de leitura que fica entre a aplicação e o PostgreSQL. Ele
fala o protocolo do Postgres, então a aplicação não sabe que ele existe — troca-se
o host da conexão e pronto. O que o torna interessante é a coerência: em vez de
depender de TTL, ele acompanha o log de replicação do banco de origem e invalida
as entradas afetadas quando os dados mudam. Em teoria, isso dá o ganho de um cache
sem o problema clássico do cache, que é servir dado velho.

A pergunta que queríamos responder era simples de enunciar:

> Colocar o PgCache na frente de uma aplicação real melhora a latência e o
> throughput dela, comparado a não ter cache nenhum — e comparado ao cache que a
> própria aplicação já traz?

E uma pergunta que costuma ficar implícita nesse tipo de comparação, mas que
decidimos tornar explícita desde o começo:

> Ele devolve as mesmas respostas?

### O detalhe que decide quase tudo

Há uma coisa sobre o PgCache que parece um detalhe de implementação e acaba sendo
o fato central deste relatório inteiro: **um "hit" não é uma leitura de memória.**

A imagem do PgCache embute um PostgreSQL próprio, e é nele que as entradas ficam.
Quando o cache acerta, o que acontece é uma consulta SQL completa a esse banco
local — conexão, parse, execução, transporte do resultado. É mais barato que ir à
origem, mas não é de graça, e não é da ordem de nanossegundos como um `GET` de
Redis.

Isso significa que o PgCache tem um **piso**. Existe um custo mínimo por consulta
abaixo do qual ele não consegue ir. Se a origem já serve mais rápido que esse
piso, nenhum ajuste de configuração ajuda — o cache vira puro custo adicional.

Levamos duas campanhas inteiras para entender que era isso que estava acontecendo.

---

## 2. Como decidimos medir

Montamos três caminhos idênticos, rodando o mesmo binário da aplicação, contra os
mesmos dados, na mesma classe de máquina:

- **Caminho A — baseline.** A aplicação vai direto ao Postgres. Nenhum cache.
- **Caminho B — PgCache.** A aplicação vai ao PgCache, que vai ao Postgres.
- **Caminho C — cache da aplicação.** A aplicação usa o cache que ela mesma traz,
  contra o Postgres direto.

A única diferença entre A e B tinha que ser o endereço do banco. Qualquer outra
coisa diferente e a comparação perde o sentido — voltaremos a isso, porque foi
exatamente aí que erramos.

O caminho C existe porque é o competidor honesto. Não adianta mostrar que o
PgCache bate um banco sem cache se a aplicação já tem um cache próprio que faz o
mesmo trabalho de graça. Um cache de aplicação **elimina** a consulta; o PgCache
**acelera** a consulta. São coisas diferentes, e a diferença importa.

### Um portão de correção que bloqueia tudo

Antes de qualquer número de desempenho, toda campanha roda uma verificação
diferencial: emite-se a mesma consulta para a origem e para o PgCache e compara-se
o resultado byte a byte. Qualquer divergência aborta a campanha.

Esse portão nunca falhou. Em todas as campanhas, com todos os sujeitos, incluindo
imediatamente após rajadas de escrita para forçar a invalidação por CDC, o
PgCache devolveu exatamente o mesmo resultado que a origem. Milhares de
comparações, zero divergências.

**Se você tirar uma única conclusão deste relatório, que seja essa: a correção do
PgCache nunca esteve em questão.** Todo o resto é sobre desempenho.

---

## 3. Primeiro sujeito: OpenFGA

O OpenFGA é um serviço de autorização — você pergunta "o usuário X pode ler o
documento Y?" e ele responde. Escolhemos porque ele parecia perfeito: escrito em
Go, Postgres como banco principal, e uma característica que num cache de leitura é
ouro puro.

**Amplificação.** Uma única chamada `Check` gera entre 104 e 171 consultas ao
banco. Se o cache absorve cada uma delas, o ganho por requisição do usuário é
multiplicado por cem.

Rodamos oito campanhas. Foi um trabalho longo, e a maior parte dele foi consertar
o laboratório, não medir o produto: pods que não subiam, nós spot que a Azure
recuperava no meio da janela, resultados perdidos quando um volume efêmero morria
junto com o pod. Nada disso é interessante aqui, mas explica por que demorou.

### O que medimos, e por que estava errado

A campanha r5 produziu uma tabela bonita. O PgCache perdia, e a perda crescia com
a carga — a 160 requisições por segundo, o p99 do caminho B era 159,92 ms contra
algo muito menor no baseline. Escrevemos aquilo como se fosse um achado sobre o
produto.

Não era. Era um achado sobre a nossa configuração.

Duas coisas estavam erradas ao mesmo tempo.

**Primeiro: nunca aquecemos o cache direito.** O PgCache tem um período em que
está aprendendo quais consultas valem a pena guardar. Nós tínhamos escrito o
código de aquecimento adaptativo — que roda até a taxa de acerto estabilizar — e
nunca passamos o parâmetro que o ligava. As 52 execuções da r5 registraram 45
segundos de aquecimento, todas elas, enquanto a taxa de acerto só estabilizava
depois de dois minutos e meio. Medimos um cache frio e chamamos de PgCache.

**Segundo: nunca usamos as tabelas fixadas.** A documentação do produto descreve
um recurso em que certas tabelas são pré-carregadas e mantidas atualizadas em
tempo real pelo CDC, em vez de invalidadas. O nosso caso de uso era literalmente o
exemplo da documentação — leituras de tabela única, sem join. Rodamos tudo na
configuração padrão.

Refizemos como r6, com o aquecimento até o joelho da curva e as tabelas fixadas. O
p99 de três workloads caiu de 25,16 / 21,94 / 28,68 ms para 15,25 / 12,92 / 13,12
ms. Aquele número de 159,92 ms a 160 rps virou 16,74 ms.

**Retratamos a afirmação da r5.** Ela está marcada como retratada nos documentos,
com o motivo. Um laboratório que não retrata não serve para nada.

### O problema de verdade

Mesmo depois de consertado, o PgCache continuava perdendo para o caminho C na
maior parte das células. E aí fomos medir a coisa que deveríamos ter medido no
primeiro dia.

A origem servia cada consulta em **39,5 a 42,2 microssegundos**. O PgCache servia
em **41,9 a 47,0**.

Cerca de 5 microssegundos de sobrecarga por consulta. Para um proxy de rede, isso
é excelente — é uma implementação eficiente. Mas o sinal está errado, e nenhuma
quantidade de configuração conserta sinal errado. Multiplicado pelas 104 a 171
consultas de cada `Check`, aqueles 5 microssegundos viravam uma penalidade
visível por requisição.

Por que a origem era tão rápida? Porque o conjunto de dados inteiro — 84.598
tuplas — cabia com folga no `shared_buffers` de 1 GB. A taxa de acerto do buffer
pool da origem era de **99,93%**.

Tínhamos construído um cache de leitura na frente de um banco de dados que já
estava servindo inteiramente da memória.

> **O que aprendemos:** amplificação multiplica o que for maior. Muitas consultas
> caras é o melhor caso possível para um cache. Muitas consultas baratas é o pior,
> porque o que se multiplica é a sobrecarga do proxy.

Virou o critério **C9** da nossa lista de triagem: o custo por consulta da origem
precisa estar confortavelmente acima do piso do proxy. E uma regra que escrevemos
junto, porque nos pegamos tentados: **não conserte isso estrangulando a origem.**
Limitar a memória do banco para forçar I/O e fazer o cache parecer bom produz um
cenário, não uma medição. Chegamos a montar uma campanha assim, a r8, e a matamos
antes de rodar.

---

## 4. Segundo sujeito: NetBox

Trocamos de aplicação. O NetBox é uma ferramenta de inventário de rede e
endereçamento IP — Django, e **o Postgres não é uma opção, é requisito**, o que é
raro e bom.

Passou em tudo que sabíamos verificar. Leituras fora de transação. Nenhuma view,
nenhuma CTE recursiva, nenhuma tabela sem chave primária. Amplificação alta —
uma listagem de prefixos na interface emite 117 consultas, das quais 100 são duas
formas repetidas, que é o padrão ideal para um cache. E nenhum cache nativo, então
a comparação seria limpa.

Antes de gastar tempo de cluster, rodamos uma sonda barata numa noite, no laptop.
Instrumentamos uma requisição e perguntamos: quanto do tempo dela é banco?

| endpoint | tempo total | tempo de banco | fatia |
|---|---:|---:|---:|
| listagem de dispositivos (API, 13 consultas) | 91,49 ms | 25 ms | 27,3% |
| listagem de prefixos (interface, 117 consultas) | 190,34 ms | 6 ms | **3,2%** |

O banco é entre 3% e 27% da requisição. O resto é Django — serialização,
avaliação de permissões, renderização de template.

Isso é um **teto**. Mesmo um cache que respondesse instantaneamente não conseguiria
melhorar a listagem de prefixos em mais de 3%. Não importa a taxa de acerto, não
importa a configuração: um cache na camada de dados só pode devolver a fatia do
tempo que a camada de dados realmente ocupa.

O PgCache ficou mais lento em todos os endpoints, nos dois volumes de dados
testados, com e sem tabelas fixadas.

Virou o critério **C9b**. E os dois juntos formam um par que vale mais que a soma:

> O **OpenFGA** falhou porque a origem era **rápida demais para bater** — 40
> microssegundos por consulta, e o banco era a maior parte da requisição.
>
> O **NetBox** falhou porque a aplicação era **lenta demais para o banco
> importar** — consultas caras em termos absolutos, mas minoria de uma requisição
> dominada por Python.

Falhas opostas, mesmo eixo. E o que ficou claro é que os oito critérios que
tínhamos antes só perguntavam *isso pode ser cacheado?* Nenhum deles perguntava
*vale a pena cachear?*

---

## 5. A virada

Nesse ponto havia duas leituras possíveis. Uma: o PgCache não vale a pena.
Outra: nós ainda não tínhamos conseguido montar um cenário em que a pergunta
pudesse ser respondida.

O que apontava para a segunda leitura era um detalhe incômodo. Em todas as
campanhas do OpenFGA, rodamos entre 40 e 160 requisições por segundo contra uma
origem com 99,93% de acerto de buffer. **A origem estava entediada.** Ela nunca
chegou perto de trabalhar de verdade.

Um cache não ajuda um banco entediado. O valor de um cache aparece quando o banco
está com dificuldade — e nós nunca tínhamos gerado carga suficiente para isso,
porque as aplicações reais que estávamos usando não conseguiam gerar essa carga.
O gargalo era sempre a aplicação, não o banco.

Foi aí que mudamos de estratégia: em vez de procurar uma aplicação onde o PgCache
brilhasse, medir o **envelope** do PgCache diretamente, com uma ferramenta que
consegue saturar um banco.

Isso muda a pergunta, e é importante ser explícito sobre isso. Uma ferramenta
sintética não tem camada de aplicação, então não existe caminho C. A pergunta
deixa de ser *"vale a pena adotar isso na minha aplicação?"* e passa a ser
*"o que este produto custa e o que ele entrega, e sob que condições?"*.

São perguntas diferentes. A segunda não substitui a primeira — mas responde algo
que a primeira, do jeito que estávamos fazendo, nunca ia responder.

### Escolhendo a ferramenta

Avaliamos cinco: pgbench, sysbench, HammerDB, BenchBase e YCSB. O detalhe
decisivo em cada caso foi como elas falam com o Postgres, verificado no código
fonte e não na documentação.

**pgbench venceu com folga**, e por um motivo específico. O PgCache precisa que
os valores venham embutidos no texto do SQL, e o pgbench faz isso **por padrão** —
o modo `simple` é o default dele. Em todas as outras ferramentas seria preciso
forçar essa configuração, e forçá-la significa deixar a origem mais lenta, o que
faz o cache parecer melhor por razões que não têm nada a ver com o cache. É o
mesmo erro que estragou a campanha r5.

E o pgbench tem uma coisa que nenhuma aplicação real nos deu: **controle sobre a
taxa de acerto do cache.** Com uma linha de script, dá para variar quantas chaves
distintas o workload toca — e portanto quanto o cache consegue acertar — de forma
contínua. No OpenFGA a taxa era 90% e ponto final; no NetBox era 40% e ponto
final. Não dava para mover para ver quanto valia.

**BenchBase foi rejeitado**, apesar de ser o mais atraente na superfície — é o
único com workloads multi-tabela realistas (Wikipedia, Twitter). O bloqueio está
no arquivo `Worker.java`, no construtor: ele chama `setAutoCommit(false)` uma vez,
para toda a vida da conexão. Isso faz toda leitura de todo workload rodar dentro
de uma transação explícita, e o PgCache não serve leituras dentro de transação —
ele repassa tudo. Não é ajustável; está no construtor. Já tínhamos medido esse
comportamento antes: 25 de 25 consultas servidas do cache fora de transação, 0 de
25 dentro.

**HammerDB foi rejeitado** porque o driver PostgreSQL dele executa as transações
como funções no servidor. O cliente manda uma chamada opaca e o corpo roda lá
dentro. O PgCache nunca vê as consultas. Não há o que cachear.

---

## 6. A fórmula

A primeira sonda com pgbench, ainda no laptop, produziu o achado que reorganizou
tudo o que tínhamos feito até então.

Medimos três coisas:

- **O** — quanto a origem custa por consulta
- **H** — quanto o PgCache custa quando **acerta**
- **M** — quanto o PgCache custa quando **erra** (porque um erro é ida ao cache
  *e* ida à origem)

Se `r` é a taxa de acerto, o PgCache está na frente quando o custo médio dele é
menor que o da origem:

```
r · H  +  (1 − r) · M   <   O
```

Isolando `r`:

```
r  >  (M − O) / (M − H)
```

Isso é aritmética de escola, mas tem duas consequências que não são óbvias, e as
duas precisam valer.

**Primeira: se `H ≥ O`, acabou.** Se um acerto custa o mesmo ou mais que a
consulta na origem, nenhuma taxa de acerto salva — nem 100%. Não é um problema de
ajuste fino; é estrutural.

**Segunda: origem mais rápida exige taxa de acerto maior.** Conforme `O` se
aproxima de `H`, o piso de acerto necessário sobe rapidamente. Um banco muito
rápido não só reduz o ganho — ele aumenta o preço de entrada.

### O que isso explica retroativamente

Aqui é onde a fórmula se paga.

**OpenFGA:** `O ≈ 40 µs`, `H ≈ 45 µs`. A condição 1 falhava. Aquela taxa de acerto
de 90% que passamos duas campanhas tentando melhorar era **irrelevante** — não
existia configuração possível. Gastamos semanas caçando um problema de ajuste que
era um problema de aritmética.

**NetBox:** taxa de acerto de 40%, contra uma barra muito mais alta. Falhava a
condição 2 com folga.

Se esse critério existisse antes, nenhum dos dois teria chegado a uma campanha.
Ele virou o **C10**.

---

## 7. Os resultados no Kubernetes

Levamos para o AKS: três nós dedicados, um para cada componente. Isso não é
preciosismo — o caminho B roda a origem **e** o PgCache, enquanto o caminho A roda
só a origem. Se dividissem CPU, o caminho B teria menos processador para a origem
exatamente na concorrência onde a comparação se decide.

Conferimos, antes de acreditar em qualquer número, que os três nós estavam na
mesma zona de disponibilidade. Uma diferença de zona teria adicionado dezenas de
microssegundos a um dos caminhos e explicado o resultado inteiro sozinha.

Volume de dados: 1 milhão de linhas, 150 MB, contra 1 GB de `shared_buffers`.
Escolhemos de propósito o caso **mais difícil** para o cache — a origem serve tudo
da memória, que é exatamente a condição que afundou o OpenFGA.

### O portão de correção

Três passes de 2.000 consultas: cache frio, cache quente, e imediatamente depois
de um `UPDATE` em 5.000 linhas para forçar a invalidação por CDC.

6.000 comparações. **Zero divergências.**

### Calibração

| | valor | condição |
|---|---:|---|
| **O** — origem | **167 µs** | caminho A |
| **H** — PgCache acertando | **119 µs** | 98,5% de acerto |
| **M** — PgCache errando | **614 µs** | 5,9% de acerto |

Break-even: `(614 − 167) / (614 − 119)` = **90,3%** com um cliente.

**Pela primeira vez na plataforma, as duas condições valem.** `H = 119 < O = 167`.

Uma surpresa útil: no laptop a origem custava 77 µs, e eu tinha previsto que numa
máquina de verdade ela ficaria mais rápida, subindo a barra e talvez apagando o
ganho. Aconteceu o contrário — no AKS ela custa **167 µs**, mais que o dobro.
O motivo é que aqui existe um salto de rede real entre nós, que no laptop, com
containers na mesma máquina, não existia. Um acerto no cache atravessa um salto
só, igual ao caminho A. Um erro atravessa dois — e é por isso que `M` subiu ainda
mais, de 174 para 614 µs.

### A curva de saturação

Este é o resultado que nenhum sujeito real conseguiu produzir. Mantivemos a taxa
de acerto fixa perto de 98,6% e variamos apenas a concorrência:

| clientes | origem (latência / tps) | PgCache (latência / tps) | ganho de tps |
|---:|---|---|---:|
| 1 | 196 µs / 5.106 | 115 µs / 8.681 | +70% |
| 8 | 255 µs / 31.391 | 154 µs / 51.930 | +65% |
| 16 | 321 µs / 49.988 | 178 µs / 89.882 | +80% |
| 32 | 516 µs / **62.078** | 265 µs / 120.964 | +95% |
| 64 | 1.073 µs / 59.687 | 506 µs / 126.518 | +112% |
| 128 | 2.183 µs / 58.661 | 965 µs / 132.688 | +126% |
| 256 | 5.053 µs / 50.706 | 1.827 µs / **140.099** | **+176%** |

Leia a coluna do meio de cima para baixo. **A origem atinge o pico em 32 clientes
e depois piora** — cai de 62.078 para 50.706 transações por segundo, com a
latência subindo 26 vezes. É o colapso clássico de saturação: mais clientes
disputando o mesmo recurso produzem menos trabalho útil.

O PgCache não vira. Sobe até 140.099 e a latência dele cresce 16 vezes, não 26.

A 256 clientes: **2,8 vezes o throughput, com 36% da latência.**

E note onde está o **menor** ganho: com um cliente só. A comparação que quase todo
mundo faz primeiro é o pior caso do PgCache aqui — e ainda assim ele ganha 70%.

### O crossover, confirmado de forma independente

A calibração previu que a 8 clientes o ponto de virada estaria em **77%** de taxa
de acerto. Testamos isso com uma distribuição de acesso completamente diferente —
Zipf, com cauda contínua, em vez de um corte duro:

| concentração | taxa de acerto | resultado |
|---|---:|---|
| baixa | 67,3% | PgCache −14% |
| | 76,5% | PgCache −5% |
| | 86,0% | PgCache **+15%** |
| | 93,1% | PgCache **+29%** |
| alta | 98,6% | PgCache **+56%** |

O crossover caiu entre 76,5% e 86,0%. A previsão veio de um experimento; a
confirmação veio de outro. A fórmula não foi ajustada aos dados que a
confirmaram.

Detalhe que vale notar: o throughput da origem é **plano** ao longo dessa tabela
inteira, de 31,5 mil a 32,7 mil. A origem não liga para a distribuição do acesso.
Toda a variação pertence ao cache.

---

## 8. As três coisas que qualificam o resultado

Um relatório que parasse na seção anterior estaria vendendo. Estas três não são
notas de rodapé.

### O protocolo muda a manchete

Aplicações reais quase sempre usam *prepared statements* — é o padrão do driver
Java depois de cinco execuções, e da maioria dos drivers. Prepared statements
poupam o Postgres de analisar e planejar a consulta toda vez.

Testamos os três modos, com os dois caminhos rodando cada um:

| modo | origem | PgCache | ganho |
|---|---:|---:|---:|
| simple | 62.128 tps | 119.824 tps | **+93%** |
| extended | 56.192 tps | 123.624 tps | **+120%** |
| prepared | **94.344 tps** | 124.364 tps | **+32%** |

O **PgCache é praticamente indiferente ao protocolo** — 4% de variação. O custo de
servir não depende de como a consulta chegou.

**A origem não é.** Varia 68%. Com prepared statements ela fica 51% mais rápida
que no modo simple.

Consequência direta: aqueles +176% a 256 clientes foram medidos em modo `simple`.
Em `prepared`, a 32 clientes, o ganho é **+32%, não +93%**. Continua sendo ganho,
com 24% menos latência — mas é um terço da margem.

**Todo número que sair desta plataforma daqui para frente tem que dizer em qual
protocolo foi medido.**

### Existe um precipício, e ele não avisa

Testamos consultas que agregam faixas de linhas, variando o tamanho da faixa:

| linhas varridas | origem | PgCache | taxa de acerto | resultado |
|---:|---:|---:|---:|---|
| 2 | 320 µs | 156 µs | 99,4% | **+105%** |
| 11 | 339 µs | 396 µs | 98,9% | −14% |
| 101 | 355 µs | 396 µs | 99,1% | −10% |
| 1.001 | 549 µs | 393 µs | 99,1% | **+40%** |
| 10.001 | 2.423 µs | **714.506 µs** | **14,9%** | **−100%** |

Olhe a coluna do PgCache: **393 a 396 microssegundos, constante**, de 11 a 1.001
linhas. Ele guarda o *resultado* — um único valor agregado — então quantas linhas
a origem teve que varrer não chega até ele. A origem, essa sim, encarece: 339 →
355 → 549 µs. É por isso que o sinal volta a ser positivo em 1.001 linhas.

E aí, em 10.001 linhas, não piora um pouco. **Despenca.** A taxa de acerto cai de
99,1% para 14,9% — o PgCache basicamente para de aceitar essas entradas — e o
caminho fica **295 vezes pior** que a origem sem cache nenhum. As duas repetições
concordam.

Isso nos obrigou a **corrigir um critério que tínhamos escrito naquela mesma
manhã**. A versão do laptop dizia "custo vindo de tamanho de resultado não é
aproveitável pelo cache". Está errado: em 1.001 linhas o cache aproveita o custo
de varredura e ganha 40%. O que é verdade é outra coisa:

> O cache aproveita o custo de varredura da origem até um limite de capacidade, e
> nesse limite ele **para de aceitar** a entrada em vez de degradar suavemente.
> Passando dele, quase toda consulta paga o caminho de erro mais a sobrecarga do
> proxy.

A explicação provável — e isto é inferência, não medição — é o rastreamento de
dependências: um `sum()` sobre 10.001 linhas devolve uma linha só, mas *depende
de* 10.001. Com mil variações da consulta, são dezenas de milhões de dependências
para manter e invalidar.

**A lição prática:** ao avaliar, olhe a **taxa de acerto**, não a latência. A
curva de latência parece suave até a borda do precipício.

Chegamos a suspeitar que os 65× de perda que vimos no laptop fossem artefato da
memória limitada do Docker Desktop. Não eram. Reproduz num nó de 30 GB, pior.

### Não é uma decisão de adoção

Não existe caminho C aqui. Não há aplicação, então não há cache de aplicação para
comparar. O que este trabalho mede é o **envelope do produto**: sob que condições
ele paga, e quanto.

Se a sua aplicação tem um cache próprio que elimina a consulta antes dela sair, a
comparação relevante é outra, e esta bancada não a respondeu.

---

## 8b. O achado que qualifica todos os anteriores: escrita

Tudo nas seções acima foi medido com **zero escrita**. Isso não é um detalhe de
configuração — é a condição em que quase nenhum sistema de produção vive, e era o
maior buraco da plataforma. Nenhuma das oito campanhas anteriores tinha escrito
uma linha durante a medição.

Fechamos isso misturando leitura e escrita, com as escritas batendo **na mesma
faixa de chaves** que as leituras consultam. Escrever noutro lugar daria um
número bonito e sem sentido, porque o CDC não teria nada em cache para invalidar.

| escrita | origem tps | PgCache tps | ganho | p99 leitura A | p99 leitura B |
|---:|---:|---:|---:|---:|---:|
| 0% | 36.376 | 49.690 | **+37%** | 0,313 ms | 0,264 ms |
| 5% | 19.175 | 20.120 | +5% | 0,294 ms | 0,484 ms |
| 10% | 12.573 | 12.308 | **−2%** | 0,300 ms | 0,511 ms |
| 30% | 4.784 | 4.738 | −1% | 0,312 ms | 0,561 ms |
| 50% | 2.910 | 2.794 | −4% | 0,319 ms | 0,576 ms |

**A vantagem de vazão desaparece com 10% de escrita** — uma proporção OLTP
completamente banal.

O motivo é aritmético. Nesta bancada um `UPDATE` custa cerca de 25 vezes um point
select, e **escritas passam idênticas pelos dois caminhos**: o cache não as
acelera nem as atrasa. Numa mistura 90/10 elas consomem a maior parte do tempo e
afogam o ganho do lado da leitura. A vazão total cai de 36.376 para 12.573 no
próprio caminho sem cache — o gargalo deixou de ser a leitura.

**E há um segundo efeito, que é o custo da coerência aparecendo.** Compare as duas
últimas colunas. O p99 de leitura da origem é **plano**: 0,294 a 0,319 ms, com 0%
ou com 50% de escrita. O do PgCache sobe de 0,264 para 0,576 ms — **81% pior**.

A taxa de acerto fica em 98% o tempo todo, então não é o CDC despejando entradas.
Elas continuam lá e continuam sendo servidas; servi-las é que passa a custar mais
enquanto o fluxo de invalidação corre em paralelo.

Isso não retrata as seções anteriores — elas mediram o que diziam medir. Mas
**nenhum número delas pode ser citado sem a proporção de escrita junto.**

### A coerência aguentou

Testamos a única situação em que um cache coerente por CDC pode de fato ser pego
servindo dado velho: leitura chegando **enquanto** as escritas estão em voo.

O método precisa de cuidado, porque sob mutação concorrente duas leituras podem
divergir legitimamente. Lemos a origem, o PgCache, e a origem **de novo**. Se as
duas leituras da origem concordam, a linha ficou estável e a resposta do PgCache
precisa bater. Se discordam, a amostra é inconclusiva e é descartada — nunca
contada como divergência.

**1.931 comparações conclusivas, zero divergências.** As 69 inconclusivas provam
que o teste não foi vazio: são chaves que o escritor de fato alterou dentro da
janela.

### E uma inferência nossa que caiu

A seção sobre o precipício dizia que o portão de materialização estaria
*recusando* as entradas grandes. Instrumentamos o contador e ele mostrou o
contrário, duas vezes.

Em `span=1`, onde o PgCache é **mais rápido**, o portão rejeita as 1.000 entradas
— recusar materializar significa servir pelo caminho barato, e a rejeição é uma
decisão boa. Em `span=10000`, onde acontece o colapso, `mv_reject` é **zero**: ele
admite 118 de 1.000, e as outras 882 não aparecem em contador nenhum.

Então não é uma política recusando trabalho. É consistente com uma fila de
construção que não dá conta. Continua sendo inferência — mas agora com o
candidato anterior eliminado por medição, que é a diferença entre um palpite e
uma hipótese.

---

## 9. O que sabemos

**O PgCache é semanticamente transparente.** Milhares de comparações diferenciais,
em três sujeitos diferentes, incluindo logo após invalidação por CDC. Zero
divergências, sempre.

**Ele paga quando duas condições valem, e as duas são propriedades do workload,
não da configuração.** Um acerto precisa custar menos que a consulta na origem, e
a taxa de acerto precisa passar de `(M − O) / (M − H)`.

**O ganho cresce com a carga.** É o oposto do que parecia na campanha r5, que
retratamos. Quanto mais o banco de origem sofre, mais o cache entrega — e é
justamente aí que se precisa dele. Com a origem em colapso, 2,8× o throughput com
36% da latência.

**A margem depende muito do protocolo do driver.** +93% em modo simple, +32% com
prepared statements.

**Existe um precipício em consultas com pegada de invalidação grande**, e ele não
dá aviso na curva de latência.

**Escrita elimina o ganho.** A 10% de escrita a vantagem de vazão some, e a
latência de leitura no p99 piora 70%, mesmo com 98% de acerto. O PgCache entrega
em cargas dominadas por leitura contra uma origem sob pressão — e só aí.

**Os percentis confirmam as médias.** Onde a amostragem é adequada, o p99
acompanha a média de perto, então as conclusões tiradas de médias nas seções
anteriores sobrevivem.

## 10. O que ainda não sabemos

**Leitura e escrita em faixas separadas.** No teste acima as duas dividem as
mesmas 1.000 chaves de propósito, que é o pior caso possível para invalidação.
Uma aplicação real costuma escrever num subconjunto pequeno e ler de um conjunto
muito maior. Varrer esse gradiente é o experimento mais promissor que sobrou — é
onde o PgCache pode recuperar a vantagem sob escrita.

**O comportamento das tabelas fixadas sob escrita.** Elas são atualizadas no
lugar em vez de invalidadas, o que em tese as protege exatamente do efeito medido
acima. Não testamos.

**Volumes maiores que a memória.** Só rodamos o degrau onde os dados cabem no
`shared_buffers`. Degraus maiores encarecem a origem e baixam a barra de acerto —
mas precisam ser reportados como escala, e não como o degrau onde o cache
finalmente ficou bonito.

**Percentis.** O pgbench reporta latência média nativamente. p99 exige registro
por transação, o que a 140 mil transações por segundo é um experimento à parte.

**Aplicações reais que passem nos critérios.** Continua sendo a pergunta original,
e continua em aberto. Agora temos uma triagem que custa meia hora de laptop —
meça `O`, `H` e `M`, calcule o break-even, compare com a taxa de acerto que o
workload real produz — em vez de dias de cluster.

---

## Apêndice — onde estão os detalhes

| documento | conteúdo |
|---|---|
| [`docs/TRIAGE-CRITERIA.md`](docs/TRIAGE-CRITERIA.md) | os critérios C1–C10, cada um com o caso que o ensinou |
| [`openFGA/benchmark-docs/`](openFGA/benchmark-docs/) | as oito campanhas do OpenFGA, incluindo a retratação da r5 |
| [`netbox/RESULTS-probe0.md`](netbox/RESULTS-probe0.md) | a sonda que derrubou o NetBox em uma noite |
| [`synthetic/ANALYSIS.md`](synthetic/ANALYSIS.md) | a avaliação das cinco ferramentas, com a evidência de código de cada veredito |
| [`synthetic/RESULTS-probe0.md`](synthetic/RESULTS-probe0.md) | a sonda local que produziu a fórmula |
| [`synthetic/RESULTS-aks-s1.md`](synthetic/RESULTS-aks-s1.md) | calibração e curva de saturação no AKS |
| [`synthetic/RESULTS-aks-s2.md`](synthetic/RESULTS-aks-s2.md) | crossover, protocolo e o precipício |
| [`synthetic/RESULTS-aks-s4.md`](synthetic/RESULTS-aks-s4.md) | escrita, coerência concorrente, percentis |
| [`synthetic/SCENARIOS.md`](synthetic/SCENARIOS.md) | o desenho das campanhas, escrito antes de rodá-las |

Os dados brutos de cada célula estão em `synthetic/results-s1-cells.tsv` e
`synthetic/results-s2-cells.tsv`. Todos os experimentos são reproduzíveis pelos
comandos no fim de cada relatório.

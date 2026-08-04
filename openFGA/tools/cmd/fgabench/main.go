// fgabench — gerador de carga OPEN-LOOP para o benchmark PgCache x OpenFGA.
//
// Por que open-loop: um gerador closed-loop (N threads que esperam a resposta
// anterior antes de disparar a proxima) reduz a carga automaticamente quando o
// servidor fica lento. O resultado e' *coordinated omission*: o sistema saturado
// parece rapido porque o cliente parou de perguntar. Aqui as chegadas sao
// agendadas em taxa fixa e a latencia e' medida a partir do INSTANTE PRETENDIDO
// de chegada, nao de quando um worker ficou livre. Filas do lado do cliente
// aparecem na latencia, como no mundo real.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/benedetlabs/pgcache-openfga-bench/internal/fga"
	"github.com/benedetlabs/pgcache-openfga-bench/internal/metrics"
	"github.com/benedetlabs/pgcache-openfga-bench/internal/universe"
)

type Manifest struct {
	Rung    string          `json:"rung"`
	StoreID string          `json:"store_id"`
	ModelID string          `json:"model_id"`
	Params  universe.Params `json:"params"`
}

type sample struct {
	latNs   int64
	ok      bool
	allowed bool
}

type Result struct {
	RunID       string  `json:"run_id"`
	Path        string  `json:"path"`
	PathName    string  `json:"path_name"`
	Rung        string  `json:"rung"`
	Workload    string  `json:"workload"`
	Target      string  `json:"target"`
	Consistency string  `json:"consistency"`
	RateTarget  float64 `json:"rate_target_rps"`
	Workers     int     `json:"workers"`
	WarmupSec   float64 `json:"warmup_s"`
	DurationSec float64 `json:"duration_s"`
	DocDist     string  `json:"doc_dist"`
	CampaignID  string  `json:"campaign_id"`

	Requests   int64   `json:"requests"`
	Errors     int64   `json:"errors"`
	Dropped    int64   `json:"dropped"`
	Allowed    int64   `json:"allowed"`
	Denied     int64   `json:"denied"`
	RateAchiev float64 `json:"rate_achieved_rps"`

	P50  float64 `json:"p50_ms"`
	P90  float64 `json:"p90_ms"`
	P95  float64 `json:"p95_ms"`
	P99  float64 `json:"p99_ms"`
	P999 float64 `json:"p999_ms"`
	Max  float64 `json:"max_ms"`
	Mean float64 `json:"mean_ms"`

	// Deltas do servidor dentro da janela
	FGADatastoreQueries float64 `json:"fga_datastore_queries"`
	FGARequests         float64 `json:"fga_requests"`
	QueriesPerRequest   float64 `json:"queries_per_request"`
	PgcacheHits         float64 `json:"pgcache_hits"`
	PgcacheMisses       float64 `json:"pgcache_misses"`
	PgcacheHitRatio     float64 `json:"pgcache_hit_ratio"`

	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
}

func main() {
	fs := flag.NewFlagSet("fgabench", flag.ExitOnError)
	target := fs.String("target", "http://localhost:8080", "OpenFGA sob teste")
	manifestPath := fs.String("manifest", "manifest.json", "manifesto do degrau")
	workload := fs.String("workload", "w1", "w1|w2|w3|w4|w5|w7")
	rate := fs.Float64("rate", 200, "taxa de chegada alvo (req/s); 0 = ilimitado")
	dur := fs.Duration("duration", 60*time.Second, "janela de medicao")
	warm := fs.Duration("warmup", 20*time.Second, "aquecimento, excluido da analise")
	workers := fs.Int("workers", 128, "goroutines de execucao")
	zipfS := fs.Float64("zipf", 1.1, "expoente da Zipf (>1); ignorado em w2")
	posRatio := fs.Float64("positive-ratio", 0.85, "fracao de checks positivos em w1")
	consistency := fs.String("consistency", "MINIMIZE_LATENCY", "MINIMIZE_LATENCY|HIGHER_CONSISTENCY")
	pathID := fs.String("path", "A", "rotulo do caminho: A|B|C")
	pathName := fs.String("path-name", "", "nome do caminho (default derivado)")
	runID := fs.String("run-id", "", "identificador da execucao")
	outDir := fs.String("out", "results", "diretorio de saida")
	fgaMetrics := fs.String("fga-metrics", "", "URL /metrics do OpenFGA sob teste")
	pgcMetrics := fs.String("pgcache-metrics", "", "URL /metrics do PgCache (so' no path B)")
	diffTarget := fs.String("diff-target", "", "segundo OpenFGA, obrigatorio em w7")
	seed := fs.Int64("seed", 42, "semente do gerador")
	diffSamples := fs.Int("diff-samples", 4000, "pares comparados em w7")
	campaignID := fs.String("campaign-id", "adhoc",
		"identificador da campanha; entra no CSV e isola execucoes de campanhas distintas")
	docDist := fs.String("doc-dist", "zipf",
		"localidade do eixo documento: zipf|uniform (uniform reproduz a run de 25/07/2026)")
	warmMax := fs.Duration("warmup-max", 0,
		"teto do aquecimento adaptativo; 0 desliga e usa -warmup fixo")
	kneeEps := fs.Float64("warmup-knee-eps", 0.01,
		"variacao maxima do hit ratio entre sondas para considerar o cache aquecido")
	postWarmupExec := fs.String("post-warmup-exec", "",
		"comando sh -c executado entre o aquecimento e a janela de medicao")
	fs.Parse(os.Args[1:])

	switch *docDist {
	case "zipf", "uniform":
	default:
		die("-doc-dist invalido: %q (use zipf|uniform)", *docDist)
	}

	if *pathName == "" {
		*pathName = map[string]string{"A": "baseline", "B": "pgcache", "C": "appcache"}[*pathID]
	}
	if *runID == "" {
		*runID = time.Now().UTC().Format("20060102T150405Z")
	}

	var m Manifest
	raw, err := os.ReadFile(*manifestPath)
	if err != nil {
		die("lendo manifesto: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		die("manifesto invalido: %v", err)
	}
	p := m.Params

	if *workload == "w7" {
		if *diffTarget == "" {
			die("w7 exige -diff-target")
		}
		runDifferential(*target, *diffTarget, m, *consistency, *workers, *diffSamples, *seed)
		return
	}

	cl := fga.New(*target, *workers)
	cl.StoreID, cl.ModelID = m.StoreID, m.ModelID
	if err := cl.Healthy(context.Background()); err != nil {
		die("alvo %s nao responde: %v", *target, err)
	}

	gen := newGenerator(p, *workload, *zipfS, *posRatio, *seed, *docDist)

	// ── aquecimento ────────────────────────────────────────────────────────
	warmedFor := time.Duration(0)
	if *warm > 0 {
		warmedFor = warmup(cl, gen, *rate, *warm, *warmMax, *kneeEps, *workers,
			*consistency, *pgcMetrics, *pathID, *workload)
	}

	// Ganchos que precisam rodar DEPOIS do aquecimento e ANTES da medicao — na
	// pratica o reset do pg_stat_statements. Zerar antes do aquecimento faz a
	// janela do servidor cobrir aquecimento + medicao enquanto a do cliente cobre
	// so' a medicao, e as duas deixam de ser comparaveis.
	if *postWarmupExec != "" {
		c := exec.Command("sh", "-c", *postWarmupExec)
		c.Stdout, c.Stderr = os.Stderr, os.Stderr
		if err := c.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "[%s/%s] post-warmup-exec falhou: %v\n", *pathID, *workload, err)
		}
	}

	// ── janela de medicao ──────────────────────────────────────────────────
	var beforeFGA, afterFGA, beforePGC, afterPGC *metrics.Snapshot
	if *fgaMetrics != "" {
		beforeFGA, _ = metrics.Fetch(*fgaMetrics)
	}
	if *pgcMetrics != "" {
		beforePGC, _ = metrics.Fetch(*pgcMetrics)
	}

	fmt.Fprintf(os.Stderr, "[%s/%s] medindo %s a %.0f rps...\n", *pathID, *workload, *dur, *rate)
	started := time.Now()
	samples := make([]sample, 0, 1<<20)
	var mu sync.Mutex
	dropped := runPhase(cl, gen, *rate, *dur, *workers, *consistency, func(s sample) {
		mu.Lock()
		samples = append(samples, s)
		mu.Unlock()
	})
	ended := time.Now()

	if *fgaMetrics != "" {
		afterFGA, _ = metrics.Fetch(*fgaMetrics)
	}
	if *pgcMetrics != "" {
		afterPGC, _ = metrics.Fetch(*pgcMetrics)
	}

	res := summarize(samples, started, ended)
	res.RunID, res.Path, res.PathName = *runID, *pathID, *pathName
	res.Rung, res.Workload, res.Target = m.Rung, *workload, *target
	res.Consistency, res.RateTarget, res.Workers = *consistency, *rate, *workers
	res.WarmupSec, res.DurationSec = warmedFor.Seconds(), dur.Seconds()
	res.DocDist = *docDist
	res.CampaignID = *campaignID
	res.Dropped = dropped

	// Deltas do servidor. Nomes de serie sao descobertos, nao adivinhados:
	// se o nome canonico nao existir, procuramos por substring.
	if beforeFGA != nil && afterFGA != nil {
		res.FGADatastoreQueries = firstDelta(beforeFGA, afterFGA,
			"openfga_datastore_query_count_sum", "datastore_query_count_sum")
		res.FGARequests = firstDelta(beforeFGA, afterFGA,
			"openfga_datastore_query_count_count", "datastore_query_count_count")
		if res.FGARequests > 0 {
			res.QueriesPerRequest = res.FGADatastoreQueries / res.FGARequests
		}
	}
	if beforePGC != nil && afterPGC != nil {
		res.PgcacheHits = firstDelta(beforePGC, afterPGC,
			"pgcache_queries_cache_hit", "pgcache_queries_cache_hit_total", "cache_hit")
		res.PgcacheMisses = firstDelta(beforePGC, afterPGC,
			"pgcache_queries_cache_miss", "pgcache_queries_cache_miss_total", "cache_miss")
		if tot := res.PgcacheHits + res.PgcacheMisses; tot > 0 {
			res.PgcacheHitRatio = res.PgcacheHits / tot
		}
	}

	dir := filepath.Join(*outDir, m.Rung, *workload, *pathID+"-"+*runID)
	os.MkdirAll(dir, 0o755)
	writeJSON(filepath.Join(dir, "summary.json"), res)
	writeLatencyCSV(filepath.Join(dir, "latencies.csv"), samples)
	appendMasterCSV(filepath.Join(*outDir, "all-runs.csv"), res)
	afterFGA.Save(filepath.Join(dir, "openfga-metrics-after.txt"))
	beforeFGA.Save(filepath.Join(dir, "openfga-metrics-before.txt"))
	afterPGC.Save(filepath.Join(dir, "pgcache-metrics-after.txt"))
	beforePGC.Save(filepath.Join(dir, "pgcache-metrics-before.txt"))

	printSummary(res, dir)
}

func firstDelta(b, a *metrics.Snapshot, names ...string) float64 {
	for _, n := range names {
		if _, ok := a.Sums[n]; ok {
			return metrics.Delta(b, a, n)
		}
	}
	// fallback: procura por substring
	for _, n := range names {
		for _, found := range a.FindSeries(n) {
			return metrics.Delta(b, a, found)
		}
	}
	return 0
}

// ── fase de execucao (open-loop) ────────────────────────────────────────────

type job struct {
	intended time.Time
	seq      int64
}

func runPhase(cl *fga.Client, gen *generator, rate float64, d time.Duration,
	workers int, consistency string, sink func(sample)) int64 {

	buf := int(rate * 4)
	if buf < 1024 {
		buf = 1024
	}
	if buf > 1<<20 {
		buf = 1 << 20
	}
	jobs := make(chan job, buf)
	var droppedV int64
	dropped := &droppedV
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(int64(id)*1_000_003 + 7))
			for j := range jobs {
				// Em saturacao a fila do cliente enche. Drenar tudo depois do
				// deadline estenderia a run muito alem de -duration e mediria
				// uma janela que nao existe. Jobs enfileirados apos o fim sao
				// descartados (e contabilizados como tal), nao executados.
				if ctx.Err() != nil {
					atomic.AddInt64(dropped, 1)
					continue
				}
				ok, allowed := gen.do(ctx, cl, j.seq, consistency, rnd)
				if ctx.Err() != nil && !ok {
					atomic.AddInt64(dropped, 1)
					continue
				}
				if sink != nil {
					sink(sample{
						latNs:   time.Since(j.intended).Nanoseconds(),
						ok:      ok,
						allowed: allowed,
					})
				}
			}
		}(w)
	}

	start := time.Now()
	deadline := start.Add(d)
	var seq int64
	if rate <= 0 {
		// modo saturacao: sem agendamento, workers puxam o mais rapido possivel
		for time.Now().Before(deadline) {
			jobs <- job{intended: time.Now(), seq: seq}
			seq++
		}
	} else {
		interval := time.Duration(float64(time.Second) / rate)
		for i := int64(0); ; i++ {
			intended := start.Add(time.Duration(i) * interval)
			if intended.After(deadline) {
				break
			}
			if wait := time.Until(intended); wait > 0 {
				time.Sleep(wait)
			}
			select {
			case jobs <- job{intended: intended, seq: i}:
			default:
				// fila do cliente cheia: registramos como descarte em vez de
				// bloquear, para nao reintroduzir coordinated omission.
				atomic.AddInt64(dropped, 1)
			}
		}
	}
	cancel()
	close(jobs)
	wg.Wait()
	return atomic.LoadInt64(dropped)
}

// warmup aquece o alvo e devolve quanto tempo aqueceu de fato.
//
// Com -warmup-max 0 o comportamento e' o antigo: aquece por `fixed` e segue. Com
// -warmup-max > 0 e metricas do PgCache disponiveis, aquece em fatias de `fixed`
// ate' o hit ratio DA FATIA estabilizar (variacao < eps entre duas sondas
// consecutivas) ou ate' bater o teto.
//
// Existe porque o PLAN.md secao 8.2 pede esperar o joelho do hit-rate e a
// primeira execucao usou 30 s fixos: em E3 o cache ainda registrava shapes novas
// depois de 6 minutos, e a janela de medicao pegou cache frio rotulado de
// "PgCache". Medir cache frio e chamar de cache e' o erro que este gancho evita.
func warmup(cl *fga.Client, gen *generator, rate float64, fixed, max time.Duration,
	eps float64, workers int, consistency, pgcURL, pathID, workload string) time.Duration {

	if max <= 0 || pgcURL == "" {
		fmt.Fprintf(os.Stderr, "[%s/%s] aquecendo %s a %.0f rps...\n", pathID, workload, fixed, rate)
		runPhase(cl, gen, rate, fixed, workers, consistency, nil)
		return fixed
	}

	fmt.Fprintf(os.Stderr, "[%s/%s] aquecendo ate' o joelho do hit ratio (fatias de %s, teto %s)...\n",
		pathID, workload, fixed, max)

	sliceRatio := func(before, after *metrics.Snapshot) (float64, bool) {
		h := metrics.Delta(before, after, "pgcache_queries_cache_hit")
		m := metrics.Delta(before, after, "pgcache_queries_cache_miss")
		if h+m <= 0 {
			return 0, false
		}
		return h / (h + m), true
	}

	total := time.Duration(0)
	prev, havePrev := 0.0, false
	for total < max {
		before, _ := metrics.Fetch(pgcURL)
		runPhase(cl, gen, rate, fixed, workers, consistency, nil)
		total += fixed
		after, _ := metrics.Fetch(pgcURL)

		r, ok := sliceRatio(before, after)
		if !ok {
			fmt.Fprintf(os.Stderr, "  %s: sem consultas cacheaveis na fatia\n", total)
			continue
		}
		fmt.Fprintf(os.Stderr, "  %s: hit ratio da fatia %.1f%%\n", total, r*100)
		if havePrev && math.Abs(r-prev) < eps {
			fmt.Fprintf(os.Stderr, "[%s/%s] joelho em %s (delta %.4f < %.4f)\n",
				pathID, workload, total, math.Abs(r-prev), eps)
			return total
		}
		prev, havePrev = r, true
	}
	fmt.Fprintf(os.Stderr, "[%s/%s] AVISO: teto de aquecimento (%s) atingido sem estabilizar — "+
		"a janela de medicao pega cache ainda em formacao\n", pathID, workload, max)
	return total
}

// ── geradores de requisicao por workload ────────────────────────────────────

type generator struct {
	p        universe.Params
	workload string
	zipfUser *rand.Zipf
	zipfDoc  *rand.Zipf
	uniform  *rand.Rand
	posRatio float64
	docDist  string
	mu       sync.Mutex
}

func newGenerator(p universe.Params, workload string, s, posRatio float64, seed int64, docDist string) *generator {
	r := rand.New(rand.NewSource(seed))
	if s <= 1.0 {
		s = 1.01
	}
	return &generator{
		p:        p,
		workload: workload,
		zipfUser: rand.NewZipf(r, s, 1.0, uint64(p.Users-1)),
		zipfDoc:  rand.NewZipf(r, s, 1.0, uint64(p.Docs-1)),
		uniform:  rand.New(rand.NewSource(seed ^ 0x5deece66d)),
		posRatio: posRatio,
		docDist:  docDist,
	}
}

// docSeed devolve o indice que semeia a escolha do documento.
//
// Este e' o eixo de localidade do objeto, e ele precisa ser explicito porque a
// primeira execucao do lab errou aqui: PositiveDocFor/NegativeDocFor recebiam o
// numero de sequencia da requisicao, e as duas mapeiam esse parametro por
// aritmetica modular. Sequencia monotona -> permutacao -> 100% de documentos
// distintos numa run de 1600 requisicoes. O eixo usuario tinha Zipf; o eixo
// documento nao tinha nenhuma, apesar do PLAN.md secao 6 descrever Zipf sobre
// "usuarios e objetos". Sem repeticao de object_id nao ha' WHERE repetido, que
// e' exatamente a condicao que o PgCache existe para explorar.
//
// Como as duas funcoes sao deterministicas no par (usuario, semente), semear com
// a Zipf faz semente quente devolver o MESMO documento.
//
//	"zipf"    semente Zipf(alpha) — localidade de objeto, o comportamento pretendido
//	"uniform" semente = numero de sequencia — sem localidade, reproduz a run de 25/07/2026
func (g *generator) docSeed(rnd *rand.Rand, seq int64) int {
	if g.docDist == "uniform" {
		return int(seq)
	}
	return g.pickDoc(rnd)
}

// negativeDoc devolve um documento que o usuario NAO pode ver, preservando ao
// maximo a localidade da semente.
//
// Caminho rapido: a propria semente, quando ja' e' um documento negado. Para um
// par (usuario, documento) sorteado, negado e' o caso comum — a massa e' esparsa
// — entao a esmagadora maioria das requisicoes cai aqui e o documento consultado
// e' literalmente o indice Zipf sorteado. So' quando a semente calha de ser
// positiva caimos no sondador deterministico, que reintroduz espalhamento.
func (g *generator) negativeDoc(u, z int) int {
	if z >= 0 && z < g.p.Docs && !g.p.CanView(u, z) {
		return z
	}
	d := g.p.NegativeDocFor(u, z)
	if d < 0 {
		d = z % g.p.Docs
	}
	return d
}

// pick devolve indices de usuario/documento conforme a distribuicao do workload.
// Zipf de math/rand nao e' seguro para uso concorrente, entao serializamos so'
// a amostragem (barata) e nao a chamada HTTP (cara).
func (g *generator) pickUser(rnd *rand.Rand) int {
	if g.workload == "w2" {
		return rnd.Intn(g.p.Users)
	}
	g.mu.Lock()
	v := int(g.zipfUser.Uint64())
	g.mu.Unlock()
	return v
}

func (g *generator) pickDoc(rnd *rand.Rand) int {
	if g.workload == "w2" {
		return rnd.Intn(g.p.Docs)
	}
	g.mu.Lock()
	v := int(g.zipfDoc.Uint64())
	g.mu.Unlock()
	return v
}

func (g *generator) do(ctx context.Context, cl *fga.Client, seq int64,
	consistency string, rnd *rand.Rand) (ok bool, allowed bool) {

	p := g.p
	switch g.workload {

	case "w1": // hot: mix controlado de positivos e negativos, com localidade
		u := g.pickUser(rnd)
		z := g.docSeed(rnd, seq)
		var d int
		if rnd.Float64() < g.posRatio {
			d = p.PositiveDocFor(u, z)
			if d < 0 {
				d = g.pickDoc(rnd)
			}
		} else {
			un := p.SampleDeniableUser(u)
			if un >= 0 {
				u = un
			}
			d = g.negativeDoc(u, z)
		}
		a, err := cl.Check(ctx, universe.UserRef(u), "viewer", universe.DocObj(d), consistency)
		return err == nil, a

	case "w2": // cold: par uniforme, sem localidade nenhuma
		u, d := rnd.Intn(p.Users), rnd.Intn(p.Docs)
		a, err := cl.Check(ctx, universe.UserRef(u), "viewer", universe.DocObj(d), consistency)
		return err == nil, a

	case "w3": // deny: 100% negativo -> fan-out completo, sem short-circuit
		u := p.SampleDeniableUser(g.pickUser(rnd))
		if u < 0 {
			u = 0
		}
		d := g.negativeDoc(u, g.docSeed(rnd, seq))
		a, err := cl.Check(ctx, universe.UserRef(u), "viewer", universe.DocObj(d), consistency)
		return err == nil, a

	case "w4": // ListObjects: amplificacao de leitura extrema
		u := g.pickUser(rnd)
		objs, err := cl.ListObjects(ctx, "document", "viewer", universe.UserRef(u), consistency)
		return err == nil, len(objs) > 0

	case "w5": // 95% leitura / 5% escrita: lag de CDC e invalidacao
		if rnd.Float64() < 0.05 {
			u := rnd.Intn(p.Users)
			d := rnd.Intn(p.Docs)
			err := cl.Write(ctx, []fga.TupleKey{{
				User:     universe.UserRef(u),
				Relation: "viewer",
				Object:   universe.DocObj(d),
			}}, nil)
			// conflito de tupla ja' existente nao e' falha do sistema sob teste
			return err == nil, false
		}
		u := g.pickUser(rnd)
		d := p.PositiveDocFor(u, int(seq))
		if d < 0 {
			d = g.pickDoc(rnd)
		}
		a, err := cl.Check(ctx, universe.UserRef(u), "viewer", universe.DocObj(d), consistency)
		return err == nil, a

	default:
		u, d := g.pickUser(rnd), g.pickDoc(rnd)
		a, err := cl.Check(ctx, universe.UserRef(u), "viewer", universe.DocObj(d), consistency)
		return err == nil, a
	}
}

// ── W7: correcao diferencial ────────────────────────────────────────────────
//
// Este workload NAO mede tempo. Ele responde a unica pergunta que pode invalidar
// tudo: o PgCache muda alguma decisao de autorizacao? Para um sistema de authz,
// uma resposta cacheada errada e' um incidente de seguranca, nao uma metrica.

func runDifferential(aURL, bURL string, m Manifest, consistency string,
	workers, n int, seed int64) {

	p := m.Params
	ca := fga.New(aURL, workers)
	ca.StoreID, ca.ModelID = m.StoreID, m.ModelID
	cb := fga.New(bURL, workers)
	cb.StoreID, cb.ModelID = m.StoreID, m.ModelID

	type disagreement struct {
		User, Doc    string
		A, B, Oracle bool
	}
	var (
		mu       sync.Mutex
		agree    int64
		disagree []disagreement
		oracleA  int64
		oracleB  int64
		errs     int64
	)

	jobs := make(chan int, workers*4)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(seed + int64(id)))
			ctx := context.Background()
			for range jobs {
				u := rnd.Intn(p.Users)
				d := rnd.Intn(p.Docs)
				// metade dos casos dirigida para positivos, para nao amostrar
				// so' o ramo negativo (que e' o majoritario por construcao)
				if rnd.Intn(2) == 0 {
					if pd := p.PositiveDocFor(u, rnd.Int()); pd >= 0 {
						d = pd
					}
				}
				user, doc := universe.UserRef(u), universe.DocObj(d)
				want := p.CanView(u, d)
				ra, ea := ca.Check(ctx, user, "viewer", doc, consistency)
				rb, eb := cb.Check(ctx, user, "viewer", doc, consistency)
				mu.Lock()
				switch {
				case ea != nil || eb != nil:
					errs++
				case ra != rb:
					if len(disagree) < 50 {
						disagree = append(disagree, disagreement{user, doc, ra, rb, want})
					}
				default:
					agree++
					if ra == want {
						oracleA++
					}
					if rb == want {
						oracleB++
					}
				}
				mu.Unlock()
			}
		}(w)
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	total := agree + int64(len(disagree))
	fmt.Printf("W7 — correcao diferencial (%s vs %s)\n", aURL, bURL)
	fmt.Printf("  amostras comparadas : %d\n", total)
	fmt.Printf("  concordancia A==B   : %d (%.6f%%)\n", agree, pct(agree, total))
	fmt.Printf("  divergencias        : %d\n", len(disagree))
	fmt.Printf("  A bate com oraculo  : %d (%.6f%%)\n", oracleA, pct(oracleA, agree))
	fmt.Printf("  B bate com oraculo  : %d (%.6f%%)\n", oracleB, pct(oracleB, agree))
	fmt.Printf("  erros de transporte : %d\n", errs)
	for _, d := range disagree {
		fmt.Printf("  DIVERGE %s viewer %s -> A=%v B=%v oraculo=%v\n", d.User, d.Doc, d.A, d.B, d.Oracle)
	}
	if len(disagree) > 0 {
		fmt.Fprintln(os.Stderr, "\nFALHA: os caminhos discordam. Nenhum numero de latencia vale nada ate' isto ser resolvido.")
		os.Exit(1)
	}
	if oracleA != agree || oracleB != agree {
		fmt.Fprintln(os.Stderr, "\nAVISO: os dois caminhos concordam entre si mas divergem do oraculo — massa ou modelo suspeitos.")
		os.Exit(1)
	}
	fmt.Println("  OK — nenhuma divergencia de decisao.")
}

func pct(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b) * 100
}

// ── sumarizacao ─────────────────────────────────────────────────────────────

func summarize(ss []sample, start, end time.Time) Result {
	var r Result
	r.StartedAt = start.UTC().Format(time.RFC3339Nano)
	r.EndedAt = end.UTC().Format(time.RFC3339Nano)
	if len(ss) == 0 {
		return r
	}
	lat := make([]float64, 0, len(ss))
	var sum float64
	for _, s := range ss {
		r.Requests++
		if !s.ok {
			r.Errors++
			continue
		}
		if s.allowed {
			r.Allowed++
		} else {
			r.Denied++
		}
		ms := float64(s.latNs) / 1e6
		lat = append(lat, ms)
		sum += ms
	}
	sort.Float64s(lat)
	q := func(p float64) float64 {
		if len(lat) == 0 {
			return 0
		}
		i := int(math.Ceil(p*float64(len(lat)))) - 1
		if i < 0 {
			i = 0
		}
		if i >= len(lat) {
			i = len(lat) - 1
		}
		return lat[i]
	}
	r.P50, r.P90, r.P95, r.P99, r.P999 = q(.50), q(.90), q(.95), q(.99), q(.999)
	if len(lat) > 0 {
		r.Max = lat[len(lat)-1]
		r.Mean = sum / float64(len(lat))
	}
	r.RateAchiev = float64(r.Requests) / end.Sub(start).Seconds()
	return r
}

func printSummary(r Result, dir string) {
	fmt.Printf("\n── %s / %s / degrau %s ──────────────────────────────\n",
		r.PathName, r.Workload, r.Rung)
	fmt.Printf("  requisicoes : %d (erros %d, descartes %d)\n", r.Requests, r.Errors, r.Dropped)
	fmt.Printf("  taxa        : %.0f rps alvo -> %.0f rps alcancado\n", r.RateTarget, r.RateAchiev)
	fmt.Printf("  allowed/den : %d / %d\n", r.Allowed, r.Denied)
	fmt.Printf("  latencia ms : p50=%.2f  p90=%.2f  p95=%.2f  p99=%.2f  p99.9=%.2f  max=%.2f\n",
		r.P50, r.P90, r.P95, r.P99, r.P999, r.Max)
	if r.QueriesPerRequest > 0 {
		fmt.Printf("  amplificacao: %.2f queries no datastore por requisicao\n", r.QueriesPerRequest)
	}
	if r.PgcacheHits+r.PgcacheMisses > 0 {
		fmt.Printf("  pgcache     : %.0f hits / %.0f misses (%.1f%% hit ratio)\n",
			r.PgcacheHits, r.PgcacheMisses, r.PgcacheHitRatio*100)
	}
	fmt.Printf("  saida       : %s\n\n", dir)
}

// ── persistencia ────────────────────────────────────────────────────────────

func writeJSON(path string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	os.WriteFile(path, append(b, '\n'), 0o644)
}

func writeLatencyCSV(path string, ss []sample) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, "latency_ms,ok,allowed")
	for _, s := range ss {
		fmt.Fprintf(f, "%.4f,%t,%t\n", float64(s.latNs)/1e6, s.ok, s.allowed)
	}
}

// Colunas novas entram sempre no FIM: report.py le por nome via DictReader, mas
// um all-runs.csv antigo continua valido se a ordem antiga for prefixo da nova.
var masterHeader = "run_id,path,path_name,rung,workload,rate_target,rate_achieved,requests,errors,dropped,allowed,denied,p50_ms,p90_ms,p95_ms,p99_ms,p999_ms,max_ms,mean_ms,queries_per_request,pgcache_hit_ratio,consistency,started_at,ended_at,doc_dist,warmup_s,campaign_id"

func appendMasterCSV(path string, r Result) {
	os.MkdirAll(filepath.Dir(path), 0o755)
	_, statErr := os.Stat(path)

	// Um all-runs.csv de uma versao anterior tem menos colunas que masterHeader.
	// Continuar acrescentando linhas novas sob o cabecalho velho faz o DictReader
	// do report.py descartar as colunas extras em silencio — foi assim que
	// `doc_dist` chegou vazio ao relatorio e as runs `zipf` e `uniform` acabaram
	// agregadas juntas. Reescrevemos o cabecalho quando ele esta defasado; as
	// linhas antigas seguem validas porque coluna nova so' entra no fim.
	if statErr == nil {
		if b, err := os.ReadFile(path); err == nil {
			if i := bytes.IndexByte(b, '\n'); i >= 0 &&
				string(bytes.TrimRight(b[:i], "\r")) != masterHeader {
				os.WriteFile(path, append([]byte(masterHeader+"\n"), b[i+1:]...), 0o644)
			}
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if os.IsNotExist(statErr) {
		fmt.Fprintln(f, masterHeader)
	}
	fmt.Fprintf(f, "%s,%s,%s,%s,%s,%.0f,%.2f,%d,%d,%d,%d,%d,%.4f,%.4f,%.4f,%.4f,%.4f,%.4f,%.4f,%.4f,%.6f,%s,%s,%s,%s,%.0f,%s\n",
		r.RunID, r.Path, r.PathName, r.Rung, r.Workload, r.RateTarget, r.RateAchiev,
		r.Requests, r.Errors, r.Dropped, r.Allowed, r.Denied,
		r.P50, r.P90, r.P95, r.P99, r.P999, r.Max, r.Mean,
		r.QueriesPerRequest, r.PgcacheHitRatio, r.Consistency, r.StartedAt, r.EndedAt,
		r.DocDist, r.WarmupSec, r.CampaignID)
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "erro: "+f+"\n", a...)
	os.Exit(1)
}

package main

import (
	"math/rand"
	"testing"

	"github.com/benedetlabs/pgcache-openfga-bench/internal/universe"
)

// docsFor roda o gerador em seco e devolve os documentos escolhidos, sem tocar
// em HTTP: replica a selecao de w1/w3 exatamente como do() faz.
func docsFor(t *testing.T, workload, docDist string, n int) []int {
	t.Helper()
	p := universe.Rungs["E1"]
	g := newGenerator(p, workload, 1.1, 0.85, 42, docDist)
	rnd := rand.New(rand.NewSource(7))

	out := make([]int, 0, n)
	for seq := int64(0); seq < int64(n); seq++ {
		u := g.pickUser(rnd)
		z := g.docSeed(rnd, seq)
		var d int
		switch workload {
		case "w1":
			if rnd.Float64() < g.posRatio {
				d = p.PositiveDocFor(u, z)
				if d < 0 {
					d = g.pickDoc(rnd)
				}
			} else {
				if un := p.SampleDeniableUser(u); un >= 0 {
					u = un
				}
				d = g.negativeDoc(u, z)
			}
		case "w3":
			if u = p.SampleDeniableUser(u); u < 0 {
				u = 0
			}
			d = g.negativeDoc(u, z)
		}
		out = append(out, d)
	}
	return out
}

func distinct(xs []int) int {
	s := map[int]struct{}{}
	for _, x := range xs {
		s[x] = struct{}{}
	}
	return len(s)
}

// TestDocLocality trava o bug que invalidou a primeira execucao do lab.
//
// PLAN.md secao 6 promete Zipf(1.1) sobre usuarios E objetos. A implementacao
// original semeava a escolha do documento com o numero de sequencia da
// requisicao; como PositiveDocFor/NegativeDocFor mapeiam o parametro por
// aritmetica modular, sequencia monotona virava permutacao e o eixo documento
// ficava 100% uniforme. Sem object_id repetido nao existe WHERE repetido, e o
// benchmark deixa de exercitar a unica coisa que um cache de leitura faz.
//
// O limiar (< 75% de documentos distintos) e' folgado de proposito: prende a
// regressao sem amarrar o teste a uma implementacao especifica da Zipf.
//
// W1 sofria menos que W3 e o teste mostra isso: PositiveDocFor ja' restringe o
// contradominio aos documentos que o usuario pode ver, entao mesmo semeado com a
// sequencia ele repetia (~37% de distintos). W3 nao tinha nada segurando — 99,4%
// de documentos distintos, ou seja, praticamente nenhum WHERE repetido.
func TestDocLocality(t *testing.T) {
	const n = 1600
	for _, wl := range []string{"w1", "w3"} {
		zipf := distinct(docsFor(t, wl, "zipf", n))
		unif := distinct(docsFor(t, wl, "uniform", n))

		t.Logf("%s: zipf=%d distintos (%.1f%%) · uniform=%d distintos (%.1f%%) em %d requisicoes",
			wl, zipf, 100*float64(zipf)/n, unif, 100*float64(unif)/n, n)

		if zipf >= n*3/4 {
			t.Errorf("%s com -doc-dist=zipf: %d/%d documentos distintos — sem localidade de "+
				"objeto, o cache nao tem WHERE repetido para acertar", wl, zipf, n)
		}
	}
}

// TestUniformModeReproducesTheOldBug documenta o que a run de 25/07/2026 mediu de
// fato, e serve de controle: se -doc-dist=uniform parar de espalhar, os dois
// modos convergiram e a comparacao antes/depois perde o sentido.
func TestUniformModeReproducesTheOldBug(t *testing.T) {
	const n = 1600
	unif := distinct(docsFor(t, "w3", "uniform", n))
	if unif < n*9/10 {
		t.Errorf("w3 com -doc-dist=uniform: %d/%d distintos; o modo legado deveria ser "+
			"praticamente uma permutacao (>90%%)", unif, n)
	}
}

// TestNegativeDocIsActuallyNegative garante que a otimizacao de localidade em
// negativeDoc (usar a propria semente quando ela ja' e' negada) nao contrabandeia
// documentos positivos para dentro de w3, que por definicao e' 100% negativo.
func TestNegativeDocIsActuallyNegative(t *testing.T) {
	p := universe.Rungs["E1"]
	g := newGenerator(p, "w3", 1.1, 0.85, 42, "zipf")
	rnd := rand.New(rand.NewSource(11))

	for seq := int64(0); seq < 5000; seq++ {
		u := p.SampleDeniableUser(g.pickUser(rnd))
		if u < 0 {
			u = 0
		}
		d := g.negativeDoc(u, g.docSeed(rnd, seq))
		if p.CanView(u, d) {
			t.Fatalf("w3 emitiu par POSITIVO: user:u%d pode ver document:d%d", u, d)
		}
	}
}

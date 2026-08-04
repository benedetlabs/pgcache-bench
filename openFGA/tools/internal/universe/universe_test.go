package universe

import "testing"

// bruteForceCanView resolve viewer(document:d{m}, user:u{i}) por BFS explicito
// sobre o conjunto de tuplas REALMENTE gerado, sem usar nenhuma formula.
// E' o contra-teste do oraculo CanView: se os dois divergirem, o benchmark inteiro
// esta' errado, entao isto roda no CI antes de qualquer medicao.
func bruteForceCanView(p Params, i, m int) bool {
	// 1. conjunto de usersets aos quais u{i} pertence, por fechamento transitivo
	//    sobre as tuplas (group:gX#member, member, group:gY) realmente emitidas.
	inSet := map[string]bool{UserRef(i): true}
	frontier := []int{p.UserGroup(i)}
	for len(frontier) > 0 {
		g := frontier[0]
		frontier = frontier[1:]
		if g < 0 || inSet[GroupSet(g)] {
			continue
		}
		inSet[GroupSet(g)] = true
		if par := GroupParent(g); par >= 0 {
			frontier = append(frontier, par)
		}
	}

	// 2. viewer de um documento = direct viewer | editor | viewer from parent
	//    editor = direct editor | owner | editor from parent
	//    Aqui o seeder emite: owner(user), viewer(folder-group), parent(folder),
	//    ruido (grupos sem membros / usuarios que nao sao u{i}), e user:* se publico.
	if p.IsPublicDoc(m) {
		return true // tupla (user:*, viewer, document:d{m}) foi emitida
	}
	if p.DocOwner(m) == i {
		return true // tupla (user:u{i}, owner, document:d{m}) -> editor -> viewer
	}
	// ruido nunca concede: group:n* nao tem membros, user:n* != user:u{i}
	// resta a cadeia de pastas
	k := p.DocFolder(m)
	for k >= 0 {
		if inSet[GroupSet(p.FolderGrantGroup(k))] {
			return true // tupla (group:gX#member, viewer, folder:fk)
		}
		if k == 0 {
			break
		}
		k = p.FolderParent(k)
	}
	return false
}

func TestOracleMatchesBruteForce(t *testing.T) {
	for _, name := range []string{"E0", "E1"} {
		p := Rungs[name]
		checked, pos := 0, 0
		for i := 0; i < p.Users; i++ {
			for m := 0; m < p.Docs; m += 7 {
				want := bruteForceCanView(p, i, m)
				got := p.CanView(i, m)
				if want != got {
					t.Fatalf("%s: divergencia em (u%d, d%d): oraculo=%v bruteforce=%v", name, i, m, got, want)
				}
				if want {
					pos++
				}
				checked++
			}
		}
		ratio := float64(pos) / float64(checked)
		t.Logf("%s: %d pares verificados, %.2f%% positivos", name, checked, ratio*100)
		if ratio < 0.0005 || ratio > 0.60 {
			t.Fatalf("%s: taxa-base de positivos degenerada (%.4f%%) — massa inutil", name, ratio*100)
		}
	}
}

func TestAncestorsTerminate(t *testing.T) {
	p := Rungs["E3"]
	if d := p.MaxFolderDepth(); d < 3 || d > 12 {
		t.Fatalf("profundidade de pasta fora do esperado: %d", d)
	}
	if got := len(GroupAncestors(p.Groups - 1)); got < 5 {
		t.Fatalf("arvore de grupos rasa demais: %d niveis", got)
	}
}

func TestTupleCountsAreOnTarget(t *testing.T) {
	want := map[string][2]int64{
		"E0": {3_000, 20_000},
		"E1": {60_000, 200_000},
		"E2": {700_000, 1_600_000},
		"E3": {8_000_000, 14_000_000},
	}
	for name, rng := range want {
		n := Rungs[name].TupleCount()
		t.Logf("%s -> %d tuplas (profundidade max de pasta: %d)", name, n, Rungs[name].MaxFolderDepth())
		if n < rng[0] || n > rng[1] {
			t.Errorf("%s: %d tuplas fora da faixa alvo [%d,%d]", name, n, rng[0], rng[1])
		}
	}
}

// A amostragem dirigida e' o que permite ao gerador montar mixes exatos.
// Se ela falhar, W1 e W3 viram ruido.
func TestDirectedSamplingIsSound(t *testing.T) {
	for _, name := range []string{"E0", "E1", "E2", "E3"} {
		p := Rungs[name]
		var posOK, negOK, posMiss, negMiss int
		for i := 0; i < 400; i++ {
			u := (i * 7919) % p.Users
			if m := p.PositiveDocFor(u, i*31+1); m >= 0 {
				if !p.CanView(u, m) {
					t.Fatalf("%s: PositiveDocFor(u%d)=d%d NAO e' visivel", name, u, m)
				}
				posOK++
			} else {
				posMiss++
			}
			if m := p.NegativeDocFor(u, i*17+3); m >= 0 {
				if p.CanView(u, m) {
					t.Fatalf("%s: NegativeDocFor(u%d)=d%d E' visivel", name, u, m)
				}
				negOK++
			} else {
				// So' e' aceitavel nao achar negativo para usuario onisciente.
				if !p.IsOmniscient(u) {
					t.Fatalf("%s: u%d nao e' onisciente mas nao tem documento negavel", name, u)
				}
				negMiss++
			}
		}
		t.Logf("%s: positivos %d/%d, negativos %d/%d", name, posOK, posOK+posMiss, negOK, negOK+negMiss)
		if posMiss > 20 {
			t.Errorf("%s: %d falhas ao construir par positivo", name, posMiss)
		}
		// W3 amostra apenas usuarios negaveis; verifica que o seletor funciona.
		for r := 0; r < 200; r++ {
			u := p.SampleDeniableUser(r * 13)
			if u < 0 || p.IsOmniscient(u) {
				t.Fatalf("%s: SampleDeniableUser(%d) devolveu %d (onisciente ou invalido)", name, r, u)
			}
			if m := p.NegativeDocFor(u, r*7+1); m < 0 {
				t.Fatalf("%s: usuario negavel u%d sem documento negavel", name, u)
			}
		}
	}
}

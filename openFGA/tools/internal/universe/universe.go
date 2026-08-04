// Package universe define a MASSA DE DADOS de forma puramente estrutural.
//
// Regra de ouro deste pacote: a autorizacao de qualquer par (usuario, documento)
// e' derivavel em O(profundidade) a partir dos parametros, sem consultar nada.
// Isso permite (a) semear 10M+ tuplas via COPY sem guardar estado e (b) o gerador
// de carga produzir checks POSITIVOS e NEGATIVOS exatos, que e' o que separa um
// benchmark de autorizacao de um gerador de ruido.
//
// Modelo (schema 1.1):
//
//	type user
//	type group    { member: [user, group#member] }
//	type folder   { parent: [folder]; owner; editor: ... or owner or editor from parent;
//	                viewer: [user, user:*, group#member] or editor or viewer from parent }
//	type document { idem folder }
package universe

import (
	"fmt"
	"hash/fnv"
)

// Params descreve um degrau da escada de escala.
type Params struct {
	Name string `json:"name"`

	Users  int `json:"users"`  // user:u0 .. u{Users-1}
	Groups int `json:"groups"` // group:g0 .. g{Groups-1} (arvore binaria)
	Folders int `json:"folders"` // folder:f0 .. f{Folders-1} (arvore B-aria)
	Docs   int `json:"docs"`   // document:d0 .. d{Docs-1}

	FolderBranching int `json:"folder_branching"` // filhos por pasta

	// Ruido: infla a tabela e o fan-out sem alterar a verdade fundamental.
	// group:n{x} nao tem NENHUM membro; user:u{Users+x} nao e' principal de teste.
	NoiseGroupsPerDoc int `json:"noise_groups_per_doc"`
	NoiseUsersPerDoc  int `json:"noise_users_per_doc"`
	NoiseGroups       int `json:"noise_groups"`
	NoiseUsers        int `json:"noise_users"`

	// 1 em cada PublicEvery documentos recebe viewer: user:* (documento publico).
	// 0 desliga.
	PublicEvery int `json:"public_every"`
}

// Degraus da escada (PLAN.md secao 4).
var Rungs = map[string]Params{
	"E0": {Name: "E0", Users: 200, Groups: 50, Folders: 100, Docs: 500,
		FolderBranching: 3, NoiseGroupsPerDoc: 2, NoiseUsersPerDoc: 2,
		NoiseGroups: 200, NoiseUsers: 400, PublicEvery: 100},

	"E1": {Name: "E1", Users: 2000, Groups: 500, Folders: 1000, Docs: 10000,
		FolderBranching: 4, NoiseGroupsPerDoc: 3, NoiseUsersPerDoc: 3,
		NoiseGroups: 2000, NoiseUsers: 4000, PublicEvery: 100},

	"E2": {Name: "E2", Users: 10000, Groups: 2500, Folders: 5000, Docs: 100000,
		FolderBranching: 5, NoiseGroupsPerDoc: 4, NoiseUsersPerDoc: 4,
		NoiseGroups: 10000, NoiseUsers: 20000, PublicEvery: 100},

	"E3": {Name: "E3", Users: 50000, Groups: 12500, Folders: 20000, Docs: 800000,
		FolderBranching: 6, NoiseGroupsPerDoc: 5, NoiseUsersPerDoc: 5,
		NoiseGroups: 50000, NoiseUsers: 100000, PublicEvery: 100},

	"E4": {Name: "E4", Users: 100000, Groups: 25000, Folders: 50000, Docs: 2500000,
		FolderBranching: 6, NoiseGroupsPerDoc: 5, NoiseUsersPerDoc: 5,
		NoiseGroups: 100000, NoiseUsers: 200000, PublicEvery: 100},
}

// TupleCount devolve o numero exato de tuplas que o degrau vai gerar.
func (p Params) TupleCount() int64 {
	var n int64
	n += int64(p.Users)          // user -> group
	n += int64(p.Groups - 1)     // aninhamento de grupos
	n += int64(p.Folders - 1)    // folder parent
	n += int64(p.Folders)        // folder viewer (grupo)
	n += int64(p.Docs)           // doc parent
	n += int64(p.Docs)           // doc owner
	n += int64(p.Docs) * int64(p.NoiseGroupsPerDoc)
	n += int64(p.Docs) * int64(p.NoiseUsersPerDoc)
	if p.PublicEvery > 0 {
		n += int64(p.Docs / p.PublicEvery)
	}
	return n
}

// ── Topologia ────────────────────────────────────────────────────────────────

// GroupParent: arvore binaria. g{j} e' subconjunto de g{(j-1)/2}.
// Tupla emitida: (user=group:g{j}#member, relation=member, object=group:g{parent})
func GroupParent(j int) int {
	if j <= 0 {
		return -1
	}
	return (j - 1) / 2
}

// FolderParent: arvore B-aria.
func (p Params) FolderParent(k int) int {
	if k <= 0 {
		return -1
	}
	return (k - 1) / p.FolderBranching
}

// UserGroup: grupo de entrada do usuario i.
func (p Params) UserGroup(i int) int { return i % p.Groups }

// FolderGrantGroup: grupo que recebe viewer na pasta k.
//
// O deslocamento de Groups/2 nao e' cosmetico. Sem ele, folder:f0 (a raiz, que e'
// ancestral de TODA pasta) concederia group:g0 — que e' a raiz da arvore de grupos
// e portanto contem todo usuario. Resultado: 100% dos pares seriam positivos e a
// massa seria inutil para W3 (checks negativos). Capturado por
// TestOracleMatchesBruteForce antes de qualquer medicao.
//
// Inversa (usada pelo gerador de carga para achar positivos):
//
//	folders que concedem g  ==  { k : k = (g - Groups/2) mod Groups + t*Groups }
func (p Params) FolderGrantGroup(k int) int { return (k + p.Groups/2) % p.Groups }

// FirstFolderGranting: menor k tal que FolderGrantGroup(k) == g, ou -1.
func (p Params) FirstFolderGranting(g int) int {
	k0 := ((g-p.Groups/2)%p.Groups + p.Groups) % p.Groups
	if k0 >= p.Folders {
		return -1
	}
	return k0
}

// DocFolder: pasta pai do documento m.
func (p Params) DocFolder(m int) int { return m % p.Folders }

// DocOwner: usuario dono do documento m.
func (p Params) DocOwner(m int) int { return m % p.Users }

// IsPublicDoc: documento com viewer user:*.
func (p Params) IsPublicDoc(m int) bool {
	return p.PublicEvery > 0 && m%p.PublicEvery == 0
}

// GroupAncestors devolve g{j} e todos os seus ancestrais.
// Um usuario membro de g{j} e' transitivamente membro de todos eles.
func GroupAncestors(j int) []int {
	out := make([]int, 0, 24)
	for c := j; c >= 0; c = GroupParent(c) {
		out = append(out, c)
		if c == 0 {
			break
		}
	}
	return out
}

// FolderAncestors devolve f{k} e todos os seus ancestrais (raiz inclusa).
func (p Params) FolderAncestors(k int) []int {
	out := make([]int, 0, 16)
	for c := k; c >= 0; c = p.FolderParent(c) {
		out = append(out, c)
		if c == 0 {
			break
		}
	}
	return out
}

// FolderDepth: profundidade de f{k} (raiz = 0).
func (p Params) FolderDepth(k int) int { return len(p.FolderAncestors(k)) - 1 }

// MaxFolderDepth: profundidade da pasta mais funda.
func (p Params) MaxFolderDepth() int { return p.FolderDepth(p.Folders - 1) }

// ── Verdade fundamental ──────────────────────────────────────────────────────

// CanView responde, ANALITICAMENTE, se user:u{i} e' viewer de document:d{m}.
// Esta funcao e' o oraculo do benchmark: e' contra ela que validamos que o
// OpenFGA (com e sem PgCache) devolve a resposta certa.
//
// user:u{i} e' viewer de d{m} sse qualquer uma:
//  1. d{m} e' publico (viewer: user:*)
//  2. u{i} e' owner de d{m}      (owner => editor => viewer)
//  3. existe pasta f em ancestrais(pasta de d{m}) cujo grupo concedido
//     g{FolderGrantGroup(f)} esta em ancestrais(grupo de u{i})
//     (viewer from parent, transitivo pela cadeia de pastas + arvore de grupos)
func (p Params) CanView(i, m int) bool {
	if p.IsPublicDoc(m) {
		return true
	}
	if p.DocOwner(m) == i {
		return true
	}
	// grupos aos quais u{i} pertence (entrada + ancestrais)
	ug := GroupAncestors(p.UserGroup(i))
	inUG := func(g int) bool {
		for _, x := range ug {
			if x == g {
				return true
			}
		}
		return false
	}
	for _, f := range p.FolderAncestors(p.DocFolder(m)) {
		if inUG(p.FolderGrantGroup(f)) {
			return true
		}
	}
	return false
}

// ── Nomes ────────────────────────────────────────────────────────────────────

func UserRef(i int) string      { return fmt.Sprintf("user:u%d", i) }
func NoiseUserRef(x int) string { return fmt.Sprintf("user:n%d", x) }
func GroupObj(j int) string     { return fmt.Sprintf("group:g%d", j) }
func GroupSet(j int) string     { return fmt.Sprintf("group:g%d#member", j) }
func NoiseGroupSet(x int) string { return fmt.Sprintf("group:n%d#member", x) }
func FolderObj(k int) string    { return fmt.Sprintf("folder:f%d", k) }
func DocObj(m int) string       { return fmt.Sprintf("document:d%d", m) }

// NoiseGroupFor: escolha deterministica e espalhada dos grupos de ruido do doc m.
func (p Params) NoiseGroupFor(m, slot int) int {
	h := fnv.New32a()
	fmt.Fprintf(h, "ng:%d:%d", m, slot)
	return int(h.Sum32()) % p.NoiseGroups
}

func (p Params) NoiseUserFor(m, slot int) int {
	h := fnv.New32a()
	fmt.Fprintf(h, "nu:%d:%d", m, slot)
	return int(h.Sum32()) % p.NoiseUsers
}

// ── Amostragem dirigida (usada pelo gerador de carga) ───────────────────────
//
// O gerador precisa produzir mixes exatos (W1 ~85% positivo, W3 100% negativo).
// A taxa-base de pares (u,d) positivos e' deliberadamente baixa — o que torna
// negativos triviais de amostrar e positivos algo que precisa ser CONSTRUIDO.

// PositiveDocFor devolve um documento que u{i} PODE ver, ou -1 se nao houver.
// r e' uma fonte de variacao (qualquer inteiro); a funcao e' deterministica em (i, r).
func (p Params) PositiveDocFor(i, r int) int {
	if r < 0 {
		r = -r
	}
	// Caminho 1: subir pela arvore de grupos, achar uma pasta que conceda a um
	// dos grupos do usuario, e pegar um documento dentro dela.
	anc := GroupAncestors(p.UserGroup(i))
	for t := 0; t < len(anc); t++ {
		g := anc[(r+t)%len(anc)]
		k0 := p.FirstFolderGranting(g)
		if k0 < 0 {
			continue
		}
		// pastas k0, k0+Groups, k0+2*Groups, ... < Folders
		nk := 1 + (p.Folders-1-k0)/p.Groups
		k := k0 + (r%nk)*p.Groups
		// documentos m com DocFolder(m)==k  <=>  m = k (mod Folders)
		if k >= p.Docs {
			// nenhum documento cai direto nessa pasta; tenta um descendente dela
			continue
		}
		nm := 1 + (p.Docs-1-k)/p.Folders
		m := k + (r%nm)*p.Folders
		if m < p.Docs && p.CanView(i, m) {
			return m
		}
	}
	// Caminho 2: o usuario e' dono. m = i (mod Users) => DocOwner(m)==i.
	if i < p.Docs {
		nm := 1 + (p.Docs-1-i)/p.Users
		if i%p.Users == i {
			m := i + (r%nm)*p.Users
			if m < p.Docs && p.CanView(i, m) {
				return m
			}
		}
	}
	// Caminho 3: documento publico (viewer: user:*).
	if p.PublicEvery > 0 {
		np := p.Docs / p.PublicEvery
		if np > 0 {
			m := (r % np) * p.PublicEvery
			if m < p.Docs {
				return m
			}
		}
	}
	return -1
}

// NegativeDocFor devolve um documento que u{i} NAO pode ver, ou -1.
func (p Params) NegativeDocFor(i, r int) int {
	if r < 0 {
		r = -r
	}
	for t := 0; t < 64; t++ {
		m := (r*2654435761 + t*97) % p.Docs
		if m < 0 {
			m = -m
		}
		if !p.CanView(i, m) {
			return m
		}
	}
	// Fallback deterministico: varredura linear. Em degraus pequenos (E0) o espaco
	// de documentos e' apertado o suficiente para o sondador aleatorio colidir.
	start := r % p.Docs
	if start < 0 {
		start = -start
	}
	for t := 0; t < p.Docs; t++ {
		m := (start + t) % p.Docs
		if !p.CanView(i, m) {
			return m
		}
	}
	return -1
}

// IsOmniscient: usuarios cujo grupo de entrada esta na cadeia de grupos concedida
// pela pasta RAIZ enxergam todo documento. Isso e' uma propriedade legitima da
// topologia (o equivalente ao grupo "org-wide admin" de um sistema real), nao um
// bug — mas para eles NAO existe check negativo, entao o workload W3 precisa
// exclui-los. Descoberto por TestDirectedSamplingIsSound.
func (p Params) IsOmniscient(i int) bool {
	rootGrant := p.FolderGrantGroup(0)
	for _, g := range GroupAncestors(p.UserGroup(i)) {
		if g == rootGrant {
			return true
		}
	}
	return false
}

// SampleUser devolve o r-esimo usuario NAO-onisciente (para W3).
func (p Params) SampleDeniableUser(r int) int {
	if r < 0 {
		r = -r
	}
	for t := 0; t < p.Users; t++ {
		i := (r + t) % p.Users
		if !p.IsOmniscient(i) {
			return i
		}
	}
	return -1
}

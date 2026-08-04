// fgaseed — semeia a massa de dados do benchmark.
//
// Dois subcomandos:
//
//	bootstrap  cria store + authorization model via API do OpenFGA, imprime manifesto JSON
//	tuples     emite as tuplas em TSV no stdout, pronto para `psql \copy`
//
// Por que TSV + COPY e nao a API Write: a API limita 100 tuplas por chamada
// (writes e remocoes somam juntos). O degrau E3 tem ~9,7M tuplas — seriam ~97 mil
// chamadas HTTP. COPY faz isso em minutos. A validacao pos-seed (`fgaseed verify`)
// confere uma amostra pela API para garantir que o COPY produziu tuplas que o
// OpenFGA realmente enxerga.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/benedetlabs/pgcache-openfga-bench/internal/fga"
	"github.com/benedetlabs/pgcache-openfga-bench/internal/universe"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ulidAt gera um identificador de 26 chars monotonico no alfabeto Crockford
// base32. O OpenFGA so' exige unicidade (indice unico) e ordem estavel
// (ReadPage faz ORDER BY ulid), nao um ULID valido com timestamp real.
func ulidAt(n int64) string {
	var b [26]byte
	for i := 25; i >= 0; i-- {
		b[i] = crockford[n&31]
		n >>= 5
	}
	// prefixo fixo para manter o comprimento e a ordem lexicografica
	b[0] = '7'
	return string(b[:])
}

type Manifest struct {
	Rung      string           `json:"rung"`
	StoreID   string           `json:"store_id"`
	ModelID   string           `json:"model_id"`
	Params    universe.Params  `json:"params"`
	Tuples    int64            `json:"tuples"`
	MaxDepth  int              `json:"max_folder_depth"`
	CreatedAt string           `json:"created_at"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "bootstrap":
		cmdBootstrap(os.Args[2:])
	case "tuples":
		cmdTuples(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "rungs":
		for _, n := range []string{"E0", "E1", "E2", "E3", "E4"} {
			p := universe.Rungs[n]
			fmt.Printf("%-3s  users=%-7d groups=%-6d folders=%-6d docs=%-8d tuplas=%-10d prof=%d\n",
				n, p.Users, p.Groups, p.Folders, p.Docs, p.TupleCount(), p.MaxFolderDepth())
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `uso:
  fgaseed rungs
  fgaseed bootstrap -rung E2 -fga http://localhost:8080 [-manifest out.json]
  fgaseed tuples    -rung E2 -store <STORE_ID> > tuples.tsv
  fgaseed verify    -manifest out.json -fga http://localhost:8080 [-samples 500]`)
	os.Exit(2)
}

func mustRung(name string) universe.Params {
	p, ok := universe.Rungs[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "degrau desconhecido %q (use E0..E4)\n", name)
		os.Exit(2)
	}
	return p
}

// ── bootstrap ───────────────────────────────────────────────────────────────

func cmdBootstrap(args []string) {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	rung := fs.String("rung", "E0", "degrau da escada (E0..E4)")
	addr := fs.String("fga", "http://localhost:8080", "endereco HTTP do OpenFGA")
	modelPath := fs.String("model", "model/model.json", "caminho do authorization model")
	out := fs.String("manifest", "", "arquivo de saida do manifesto (default: stdout)")
	fs.Parse(args)

	p := mustRung(*rung)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c := fga.New(*addr, 4)
	if err := c.Healthy(ctx); err != nil {
		die("OpenFGA nao responde em %s: %v", *addr, err)
	}
	storeID, err := c.CreateStore(ctx, "bench-"+p.Name)
	if err != nil {
		die("CreateStore: %v", err)
	}
	raw, err := os.ReadFile(*modelPath)
	if err != nil {
		die("lendo modelo: %v", err)
	}
	modelID, err := c.WriteAuthorizationModel(ctx, raw)
	if err != nil {
		die("WriteAuthorizationModel: %v", err)
	}

	m := Manifest{
		Rung: p.Name, StoreID: storeID, ModelID: modelID, Params: p,
		Tuples: p.TupleCount(), MaxDepth: p.MaxFolderDepth(),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	enc, _ := json.MarshalIndent(m, "", "  ")
	if *out == "" {
		os.Stdout.Write(append(enc, '\n'))
	} else if err := os.WriteFile(*out, append(enc, '\n'), 0o644); err != nil {
		die("gravando manifesto: %v", err)
	}
	fmt.Fprintf(os.Stderr, "store=%s model=%s degrau=%s tuplas_previstas=%d\n",
		storeID, modelID, p.Name, p.TupleCount())
}

// ── tuples ──────────────────────────────────────────────────────────────────

func cmdTuples(args []string) {
	fs := flag.NewFlagSet("tuples", flag.ExitOnError)
	rung := fs.String("rung", "E0", "degrau da escada")
	store := fs.String("store", "", "STORE_ID (obrigatorio)")
	fs.Parse(args)
	if *store == "" {
		die("-store e' obrigatorio")
	}
	p := mustRung(*rung)

	w := bufio.NewWriterSize(os.Stdout, 4<<20)
	defer w.Flush()

	ts := time.Now().UTC().Format("2006-01-02 15:04:05.000000+00")
	var seq int64

	// emit escreve uma linha no formato text de COPY.
	// Colunas: store, object_type, object_id, relation, _user, user_type, ulid,
	//          inserted_at, condition_name, condition_context
	emit := func(objType, objID, rel, user, userType string) {
		seq++
		w.WriteString(*store)
		w.WriteByte('\t')
		w.WriteString(objType)
		w.WriteByte('\t')
		w.WriteString(objID)
		w.WriteByte('\t')
		w.WriteString(rel)
		w.WriteByte('\t')
		w.WriteString(user)
		w.WriteByte('\t')
		w.WriteString(userType)
		w.WriteByte('\t')
		w.WriteString(ulidAt(seq))
		w.WriteByte('\t')
		w.WriteString(ts)
		w.WriteString("\t\\N\t\\N\n")
	}

	const (
		tUser    = "user"
		tUserSet = "userset"
	)

	// 1. usuario -> grupo de entrada
	for i := 0; i < p.Users; i++ {
		emit("group", fmt.Sprintf("g%d", p.UserGroup(i)), "member", universe.UserRef(i), tUser)
	}
	// 2. aninhamento de grupos: g{j} e' subconjunto de g{parent(j)}
	for j := 1; j < p.Groups; j++ {
		emit("group", fmt.Sprintf("g%d", universe.GroupParent(j)), "member",
			universe.GroupSet(j), tUserSet)
	}
	// 3. arvore de pastas
	for k := 1; k < p.Folders; k++ {
		emit("folder", fmt.Sprintf("f%d", k), "parent",
			universe.FolderObj(p.FolderParent(k)), tUser)
	}
	// 4. concessao de viewer por grupo em cada pasta
	for k := 0; k < p.Folders; k++ {
		emit("folder", fmt.Sprintf("f%d", k), "viewer",
			universe.GroupSet(p.FolderGrantGroup(k)), tUserSet)
	}
	// 5..9. documentos
	for m := 0; m < p.Docs; m++ {
		id := fmt.Sprintf("d%d", m)
		emit("document", id, "parent", universe.FolderObj(p.DocFolder(m)), tUser)
		emit("document", id, "owner", universe.UserRef(p.DocOwner(m)), tUser)
		for s := 0; s < p.NoiseGroupsPerDoc; s++ {
			emit("document", id, "viewer", universe.NoiseGroupSet(p.NoiseGroupFor(m, s)), tUserSet)
		}
		for s := 0; s < p.NoiseUsersPerDoc; s++ {
			emit("document", id, "viewer", universe.NoiseUserRef(p.NoiseUserFor(m, s)), tUser)
		}
		if p.IsPublicDoc(m) {
			// wildcard conta como userset em GetUserTypeFromUser
			emit("document", id, "viewer", "user:*", tUserSet)
		}
	}
	w.Flush()
	if seq != p.TupleCount() {
		fmt.Fprintf(os.Stderr, "AVISO: emitidas %d tuplas, TupleCount() previa %d\n", seq, p.TupleCount())
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "emitidas %d tuplas para o degrau %s\n", seq, p.Name)
}

// ── verify ──────────────────────────────────────────────────────────────────
//
// Fecha o loop entre o oraculo analitico e o que o OpenFGA realmente responde.
// Se isto falhar, a massa esta' errada e nenhum numero de latencia vale nada.

func cmdVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	manifest := fs.String("manifest", "manifest.json", "manifesto gerado por bootstrap")
	addr := fs.String("fga", "http://localhost:8080", "endereco HTTP do OpenFGA")
	samples := fs.Int("samples", 500, "numero de pares por classe (positivo/negativo)")
	consistency := fs.String("consistency", "HIGHER_CONSISTENCY", "modo de consistencia")
	fs.Parse(args)

	var m Manifest
	raw, err := os.ReadFile(*manifest)
	if err != nil {
		die("lendo manifesto: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		die("manifesto invalido: %v", err)
	}
	p := m.Params

	c := fga.New(*addr, 8)
	c.StoreID, c.ModelID = m.StoreID, m.ModelID
	ctx := context.Background()

	var okPos, okNeg, badPos, badNeg, errs int
	for r := 0; r < *samples; r++ {
		// positivo
		u := (r * 7919) % p.Users
		if d := p.PositiveDocFor(u, r*31+1); d >= 0 {
			got, err := c.Check(ctx, universe.UserRef(u), "viewer", universe.DocObj(d), *consistency)
			switch {
			case err != nil:
				errs++
			case got:
				okPos++
			default:
				badPos++
				if badPos <= 5 {
					fmt.Fprintf(os.Stderr, "DIVERGENCIA(+): oraculo diz que u%d VE d%d, OpenFGA diz que nao\n", u, d)
				}
			}
		}
		// negativo
		un := p.SampleDeniableUser(r * 13)
		if d := p.NegativeDocFor(un, r*17+3); d >= 0 {
			got, err := c.Check(ctx, universe.UserRef(un), "viewer", universe.DocObj(d), *consistency)
			switch {
			case err != nil:
				errs++
			case !got:
				okNeg++
			default:
				badNeg++
				if badNeg <= 5 {
					fmt.Fprintf(os.Stderr, "DIVERGENCIA(-): oraculo diz que u%d NAO ve d%d, OpenFGA diz que ve\n", un, d)
				}
			}
		}
	}
	fmt.Printf("verificacao do degrau %s (%s)\n", m.Rung, *addr)
	fmt.Printf("  positivos confirmados: %d   divergentes: %d\n", okPos, badPos)
	fmt.Printf("  negativos confirmados: %d   divergentes: %d\n", okNeg, badNeg)
	fmt.Printf("  erros de transporte:   %d\n", errs)
	if badPos > 0 || badNeg > 0 {
		fmt.Fprintln(os.Stderr, "\nFALHA: o oraculo e o OpenFGA discordam. A massa ou o modelo estao errados.")
		os.Exit(1)
	}
	if errs > 0 {
		os.Exit(1)
	}
	fmt.Println("  OK — oraculo e OpenFGA concordam em 100% da amostra.")
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "erro: "+f+"\n", a...)
	os.Exit(1)
}

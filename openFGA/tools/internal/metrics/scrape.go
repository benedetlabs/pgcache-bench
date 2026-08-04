// Package metrics le endpoints Prometheus em texto e extrai contadores por nome.
// Proposito unico: capturar o delta de metricas do servidor dentro da janela de
// analise, sem depender de PromQL nem do Prometheus estar de pe'.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Snapshot struct {
	At   time.Time
	Raw  string
	Sums map[string]float64 // nome da serie (sem labels) -> soma dos valores
}

// Fetch baixa /metrics e agrega, por nome de serie, a soma dos valores de todas
// as combinacoes de labels. Para contadores isso e' exatamente o que queremos
// (total no processo); para gauges e' uma agregacao grosseira e so' usamos os
// nomes que sabem ser contadores.
func Fetch(url string) (*Snapshot, error) {
	cl := &http.Client{Timeout: 15 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	s := &Snapshot{At: time.Now(), Raw: string(body), Sums: map[string]float64{}}
	sc := bufio.NewScanner(strings.NewReader(s.Raw))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		// <nome>{labels} <valor> [timestamp]
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		val, err := strconv.ParseFloat(strings.TrimSpace(line[sp+1:]), 64)
		if err != nil {
			continue
		}
		key := line[:sp]
		if b := strings.IndexByte(key, '{'); b >= 0 {
			key = key[:b]
		}
		key = strings.TrimSpace(key)
		s.Sums[key] += val
	}
	return s, nil
}

// Delta devolve after[name] - before[name].
func Delta(before, after *Snapshot, name string) float64 {
	if before == nil || after == nil {
		return 0
	}
	return after.Sums[name] - before.Sums[name]
}

// Save grava o texto cru para reanalise posterior.
func (s *Snapshot) Save(path string) error {
	if s == nil {
		return nil
	}
	return os.WriteFile(path, []byte(s.Raw), 0o644)
}

// FindSeries devolve todos os nomes de serie que contem sub — util para
// descobrir os nomes reais expostos pelo PgCache sem adivinhar.
func (s *Snapshot) FindSeries(sub string) []string {
	var out []string
	for k := range s.Sums {
		if strings.Contains(k, sub) {
			out = append(out, k)
		}
	}
	return out
}

func (s *Snapshot) String() string {
	return fmt.Sprintf("snapshot(%d series @ %s)", len(s.Sums), s.At.Format(time.RFC3339))
}

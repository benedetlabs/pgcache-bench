#!/usr/bin/env python3
"""Consolida results/ em relatorio HTML interativo + Markdown tecnico + CSV.

Le results/all-runs.csv (uma linha por execucao) e os summary.json de cada run.
Quando ha' repeticoes, reporta a MEDIANA e a dispersao (min-max) — nunca uma
execucao unica, conforme PLAN.md secao 8.
"""
import argparse, csv, json, os, statistics, sys, html
from collections import defaultdict
from datetime import datetime, timezone

PATHS = [("A", "baseline", "#2a78d6", "#3987e5"),
         ("B", "pgcache", "#eb6834", "#d95926"),
         ("C", "appcache", "#1baf7a", "#199e70")]
PATH_NAME = {p[0]: p[1] for p in PATHS}
RUNGS = ["E0", "E1", "E2", "E3", "E4"]
WL_TITLE = {
    "w1": "W1 · check-hot (Zipf, ~85% positivo)",
    "w2": "W2 · check-cold (uniforme, sem localidade)",
    "w3": "W3 · check-deny (100% negativo, fan-out completo)",
    "w4": "W4 · list-objects",
    "w5": "W5 · misto 95/5 com escrita",
}
NUM = ("rate_target rate_achieved requests errors dropped allowed denied p50_ms p90_ms "
       "p95_ms p99_ms p999_ms max_ms mean_ms queries_per_request pgcache_hit_ratio").split()


def load(results_dir):
    path = os.path.join(results_dir, "all-runs.csv")
    if not os.path.exists(path):
        sys.exit(f"nao encontrei {path} — rode ao menos uma medicao antes de gerar o relatorio")
    rows = []
    with open(path, newline="") as f:
        for r in csv.DictReader(f):
            for k in NUM:
                try:
                    r[k] = float(r.get(k) or 0)
                except ValueError:
                    r[k] = 0.0
            rows.append(r)
    return rows


# Acima desta fracao de descartes os percentis deixam de descrever a carga: o que
# sobra na amostra sao as requisicoes que conseguiram passar, e as que teriam
# demorado mais sao justamente as que a fila derrubou. Nao e' amostragem
# aleatoria, entao o p99 fica sem referente.
DROP_LIMIT = 0.10


def agg(rows):
    """(rung, workload, path) -> mediana entre repeticoes DA MESMA TAXA ALVO.

    A taxa alvo entra na chave de agrupamento. Sem ela, uma sonda de rampa a 5 rps
    e a medicao a 40 rps caem no mesmo balde, e a "mediana" resultante nao
    corresponde a nenhuma condicao que foi executada — foi o que produziu, na run
    de 25/07/2026, um E1/w1/path A reportado a 15 rps contra B e C a 40 rps.

    Para cada (degrau, workload) escolhemos a taxa com maior cobertura de paths
    (empate -> maior taxa), que e' a unica em que os tres caminhos sao
    comparaveis entre si. PLAN.md secao 8.4.
    """
    g = defaultdict(list)
    for r in rows:
        # Execucao que falhou inteira nao e' uma repeticao, e' um incidente. A run
        # de validacao do pipeline novo errou 1200 de 1200 requisicoes (o store do
        # degrau tinha sido apagado por um seed posterior) e, agregada junto,
        # arrastava a mediana com latencias zeradas.
        if r["requests"] and r["errors"] / r["requests"] > 0.5:
            continue
        # campaign_id na chave: `run_id` reinicia em r1 a cada campanha e o
        # volume de resultados nao era limpo, entao o all-runs.csv acumulava
        # linhas de campanhas diferentes sob rotulos identicos. Sem esta chave o
        # relatorio tirava mediana entre campanhas que rodaram com codigo e
        # infraestrutura distintos — o defeito D-18.
        g[(r["rung"], r["workload"], r["rate_target"],
           r.get("doc_dist") or "?", r.get("campaign_id") or "?",
           r["path"])].append(r)

    # doc_dist entra na chave pelo mesmo motivo que rate_target: `zipf` e
    # `uniform` sao workloads diferentes — um tem localidade de objeto, o outro
    # nao. Agregar os dois juntos e' tirar a media de dois experimentos.
    cover = defaultdict(set)
    for (rung, wl, rate, dist, camp, p) in g:
        cover[(rung, wl, rate, dist, camp)].add(p)

    # Criterio de escolha, em ordem: mais caminhos cobertos (so' compara quem tem
    # os tres), mais repeticoes, e por fim a execucao mais recente. O ultimo
    # desempate importa: quando uma reexecucao repete a mesma taxa e a mesma
    # cobertura da anterior, e' a nova que vale — sem isso o relatorio congela na
    # primeira medicao e ignora silenciosamente a rodada que voce acabou de fazer.
    def score(rate, dist, camp):
        rs = [r for k, v in g.items() if k[:5] == (rung_, wl_, rate, dist, camp) for r in v]
        return (len(cover[(rung_, wl_, rate, dist, camp)]), len(rs),
                max((r["started_at"] for r in rs), default=""))

    chosen = {}
    for (rung_, wl_, rate, dist, camp) in cover:
        key = (rung_, wl_)
        cur = chosen.get(key)
        if cur is None or score(rate, dist, camp) > score(*cur):
            chosen[key] = (rate, dist, camp)

    out = {}
    for (rung, wl, rate, dist, camp, p), rs in g.items():
        if chosen.get((rung, wl)) != (rate, dist, camp):
            continue

        def med(field):
            v = [x[field] for x in rs]
            return statistics.median(v) if v else 0.0
        def lo(field):
            return min((x[field] for x in rs), default=0.0)
        def hi(field):
            return max((x[field] for x in rs), default=0.0)

        req = sum(x["requests"] for x in rs)
        drp = sum(x["dropped"] for x in rs)
        drop_ratio = drp / (req + drp) if (req + drp) else 0.0

        out[(rung, wl, p)] = {
            "reps": len(rs),
            "rate_target": rate,
            "samples": req,
            "drop_ratio": drop_ratio,
            "collapsed": drop_ratio > DROP_LIMIT,
            "doc_dist": sorted({(x.get("doc_dist") or "?") for x in rs})[0],
            "campaign_id": camp,
            **{f: med(f) for f in NUM},
            **{f + "_lo": lo(f) for f in ("p50_ms", "p95_ms", "p99_ms", "rate_achieved")},
            **{f + "_hi": hi(f) for f in ("p50_ms", "p95_ms", "p99_ms", "rate_achieved")},
        }
    return out


def discarded(rows, A):
    """Execucoes deixadas de fora por nao estarem na taxa alvo comparavel."""
    kept = {(k[0], k[1], m["rate_target"], m["doc_dist"], m["campaign_id"])
            for k, m in A.items()}
    return [r for r in rows
            if (r["rung"], r["workload"], r["rate_target"],
                r.get("doc_dist") or "?", r.get("campaign_id") or "?") not in kept]


def gain(a, b):
    """Reducao percentual de b em relacao a a. Positivo = b melhor."""
    if not a:
        return 0.0
    return (a - b) / a * 100.0


# ── Markdown ────────────────────────────────────────────────────────────────

def markdown(A, meta):
    L = []
    w = L.append
    w("# Relatório — PgCache × OpenFGA\n")
    w(f"_Gerado em {meta['generated']} · {meta['runs']} execuções · "
      f"{meta['reps_note']}_\n")
    w("\n## Sumário executivo\n")
    if meta["headline"]:
        for h in meta["headline"]:
            w(f"- {h}")
    else:
        w("- Sem dados suficientes para uma conclusão. Rode `make suite`.")
    w("\n## Resultados por degrau e workload\n")
    rungs = sorted({k[0] for k in A}, key=lambda r: RUNGS.index(r) if r in RUNGS else 99)
    for rung in rungs:
        wls = sorted({k[1] for k in A if k[0] == rung})
        for wl in wls:
            rate = next(m["rate_target"] for k, m in A.items() if k[0] == rung and k[1] == wl)
            w(f"\n### {rung} — {WL_TITLE.get(wl, wl)}\n")
            w(f"_Taxa alvo {rate:.0f} rps — a mesma nos três caminhos._\n")
            w("| Path | reps | amostras | descarte | rps alcançado | p50 ms | p95 ms | p99 ms | erros | queries/req | hit ratio |")
            w("|---|---|---|---|---|---|---|---|---|---|---|")
            any_collapsed = False
            for pid, pname, _, _ in PATHS:
                m = A.get((rung, wl, pid))
                if not m:
                    continue
                hr = f"{m['pgcache_hit_ratio']*100:.1f}%" if m["pgcache_hit_ratio"] else "—"
                qr = f"{m['queries_per_request']:.2f}" if m["queries_per_request"] else "—"
                mark = " ⚠" if m["collapsed"] else ""
                any_collapsed = any_collapsed or m["collapsed"]
                w(f"| {pid} · {pname}{mark} | {m['reps']} | {m['samples']:.0f} | "
                  f"{m['drop_ratio']*100:.0f}% | {m['rate_achieved']:.0f} | "
                  f"{m['p50_ms']:.2f} | {m['p95_ms']:.2f} | {m['p99_ms']:.2f} | "
                  f"{m['errors']:.0f} | {qr} | {hr} |")
            if any_collapsed:
                w("")
                w(f"⚠ **Colapso.** Descarte acima de {DROP_LIMIT*100:.0f}%: os percentis desta "
                  "linha são calculados só sobre as requisições que completaram, e as que "
                  "teriam demorado mais são exatamente as que a fila derrubou. Trate como "
                  "*não sustentou a taxa*, não como uma medida de latência.")
            a, b = A.get((rung, wl, "A")), A.get((rung, wl, "B"))
            if a and b:
                w("")
                if a["collapsed"] or b["collapsed"]:
                    w("**PgCache vs baseline:** não comparável — pelo menos um dos caminhos "
                      "colapsou e seus percentis não descrevem a carga oferecida.")
                else:
                    w(f"**PgCache vs baseline:** p50 {gain(a['p50_ms'], b['p50_ms']):+.1f}% · "
                      f"p95 {gain(a['p95_ms'], b['p95_ms']):+.1f}% · "
                      f"p99 {gain(a['p99_ms'], b['p99_ms']):+.1f}% "
                      f"(positivo = PgCache mais rápido)")
    w("\n## Metodologia\n")
    w("Ver `PLAN.md`. Pontos que determinam a validade destes números:\n")
    w("- Gerador **open-loop**: latência medida a partir do instante pretendido de "
      "chegada, não de quando um worker ficou livre. Sem *coordinated omission*.")
    w("- Caches internos do OpenFGA **desligados** nos paths A e B "
      "(`--experimentals=\"\"` inclusive, que não é vazio por default).")
    w("- PgCache com `ALLOWED_TABLES=tuple,authorization_model` — `changelog` fica "
      "fora do cache porque sua query embute `NOW()` e não é determinística.")
    w("- Massa validada contra um oráculo analítico antes de qualquer medição "
      "(`fgaseed verify`), e correção diferencial A vs B (W7) como gate.")
    w("- Repetições reportadas por mediana com dispersão min–max.")
    return "\n".join(L) + "\n"


# ── HTML ────────────────────────────────────────────────────────────────────

HTML_TMPL = r"""<!DOCTYPE html>
<html lang="pt-BR"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Relatório · PgCache × OpenFGA</title>
<style>
:root{color-scheme:light dark}
*{box-sizing:border-box}
body{margin:0;font:15px/1.55 ui-sans-serif,-apple-system,"Segoe UI",Inter,system-ui,sans-serif;
     background:var(--bg);color:var(--text-primary)}
.viz-root{
  --bg:#f6f6f4; --surface-1:#fcfcfb; --border:#e2e1dc;
  --text-primary:#0b0b0b; --text-secondary:#52514e; --text-muted:#84837c;
  --series-A:#2a78d6; --series-B:#eb6834; --series-C:#1baf7a;
  --good:#1a7f37; --bad:#c4331d; --grid:#e8e7e2;
}
@media (prefers-color-scheme:dark){:root:where(:not([data-theme="light"])) .viz-root{
  --bg:#111110; --surface-1:#1a1a19; --border:#33322e;
  --text-primary:#ffffff; --text-secondary:#c3c2b7; --text-muted:#8f8e85;
  --series-A:#3987e5; --series-B:#d95926; --series-C:#199e70;
  --good:#4ac26b; --bad:#f0745c; --grid:#2a2a27;
}}
:root[data-theme="dark"] .viz-root{
  --bg:#111110; --surface-1:#1a1a19; --border:#33322e;
  --text-primary:#ffffff; --text-secondary:#c3c2b7; --text-muted:#8f8e85;
  --series-A:#3987e5; --series-B:#d95926; --series-C:#199e70;
  --good:#4ac26b; --bad:#f0745c; --grid:#2a2a27;
}
.wrap{max-width:1120px;margin:0 auto;padding:32px 20px 80px}
header{display:flex;justify-content:space-between;align-items:flex-start;gap:16px;flex-wrap:wrap;margin-bottom:8px}
h1{font-size:26px;margin:0 0 4px;letter-spacing:-.02em}
h2{font-size:19px;margin:38px 0 4px;letter-spacing:-.01em}
h3{font-size:15px;margin:26px 0 10px;color:var(--text-secondary);font-weight:600}
.sub{color:var(--text-secondary);font-size:13px;margin:0}
button.tg{background:var(--surface-1);border:1px solid var(--border);color:var(--text-secondary);
  border-radius:7px;padding:6px 11px;font-size:12.5px;cursor:pointer}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(190px,1fr));gap:12px;margin:20px 0 4px}
.tile{background:var(--surface-1);border:1px solid var(--border);border-radius:11px;padding:14px 16px}
.tile .k{font-size:11.5px;text-transform:uppercase;letter-spacing:.05em;color:var(--text-muted)}
.tile .v{font-size:27px;font-weight:600;letter-spacing:-.02em;margin:5px 0 2px;font-variant-numeric:tabular-nums}
.tile .n{font-size:12px;color:var(--text-secondary)}
.card{background:var(--surface-1);border:1px solid var(--border);border-radius:11px;padding:16px 18px 10px;margin:14px 0}
.legend{display:flex;gap:16px;flex-wrap:wrap;font-size:12.5px;color:var(--text-secondary);margin:2px 0 10px}
.legend i{width:9px;height:9px;border-radius:2px;display:inline-block;margin-right:6px;vertical-align:baseline}
table{border-collapse:collapse;width:100%;font-size:13px;font-variant-numeric:tabular-nums}
th,td{text-align:right;padding:6px 9px;border-bottom:1px solid var(--border)}
th:first-child,td:first-child{text-align:left}
th{color:var(--text-muted);font-weight:500;font-size:11.5px;text-transform:uppercase;letter-spacing:.04em}
.good{color:var(--good)} .bad{color:var(--bad)}
.note{color:var(--text-secondary);font-size:13px;max-width:74ch}
.tip{position:fixed;pointer-events:none;background:var(--surface-1);border:1px solid var(--border);
  border-radius:8px;padding:7px 10px;font-size:12.5px;box-shadow:0 6px 20px rgba(0,0,0,.16);
  opacity:0;transition:opacity .1s;z-index:9;white-space:nowrap}
svg{display:block;width:100%;height:auto;overflow:visible}
.ax{fill:var(--text-muted);font-size:11px}
.gl{stroke:var(--grid);stroke-width:1}
.dl{font-size:11.5px;font-weight:600}
details{margin-top:10px}summary{cursor:pointer;font-size:13px;color:var(--text-secondary)}
</style></head>
<body class="viz-root"><div class="wrap">
<header>
  <div><h1>PgCache × OpenFGA</h1>
  <p class="sub">__SUB__</p></div>
  <button class="tg" onclick="var r=document.documentElement;r.dataset.theme=r.dataset.theme==='dark'?'light':'dark'">tema</button>
</header>
__BODY__
</div>
<div class="tip" id="tip"></div>
<script>
const DATA = __DATA__;
const C = {A:'var(--series-A)',B:'var(--series-B)',C:'var(--series-C)'};
const NAME = {A:'baseline',B:'pgcache',C:'appcache'};
const tip = document.getElementById('tip');
function hover(el, txt){
  el.addEventListener('mousemove', e=>{tip.textContent=txt;tip.style.opacity=1;
    tip.style.left=Math.min(e.clientX+14, innerWidth-tip.offsetWidth-8)+'px';
    tip.style.top=(e.clientY-34)+'px';});
  el.addEventListener('mouseleave', ()=>{tip.style.opacity=0});
}
const NS='http://www.w3.org/2000/svg';
function mk(t,a){const e=document.createElementNS(NS,t);for(const k in a)e.setAttribute(k,a[k]);return e}

// Barras agrupadas: cats no eixo X, uma barra por path.
function grouped(host, cats, series, fmt, unit){
  const W=760,H=250,ml=54,mr=14,mt=14,mb=34;
  const iw=W-ml-mr, ih=H-mt-mb;
  const max=Math.max(...series.flatMap(s=>s.vals.filter(v=>v!=null)),0.0001)*1.15;
  const svg=mk('svg',{viewBox:`0 0 ${W} ${H}`,role:'img'});
  for(let i=0;i<=4;i++){const y=mt+ih-ih*i/4;
    svg.appendChild(mk('line',{x1:ml,x2:W-mr,y1:y,y2:y,class:'gl'}));
    const t=mk('text',{x:ml-8,y:y+4,class:'ax','text-anchor':'end'});
    t.textContent=fmt(max*i/4);svg.appendChild(t)}
  const gw=iw/cats.length, bw=Math.min(34,(gw-14)/series.length);
  cats.forEach((c,ci)=>{
    const gx=ml+gw*ci+(gw-bw*series.length-2*(series.length-1))/2;
    series.forEach((s,si)=>{
      const v=s.vals[ci]; if(v==null)return;
      const h=Math.max(2,ih*v/max), x=gx+si*(bw+2), y=mt+ih-h;
      const r=mk('rect',{x,y,width:bw,height:h,rx:4,fill:C[s.id],
        stroke:'var(--surface-1)','stroke-width':2});
      hover(r,`${c} · ${NAME[s.id]}: ${fmt(v)}${unit}`);
      svg.appendChild(r);
      if(series.length<=3){const t=mk('text',{x:x+bw/2,y:y-5,class:'dl',
        'text-anchor':'middle',fill:C[s.id]});t.textContent=fmt(v);svg.appendChild(t)}
    });
    const t=mk('text',{x:ml+gw*ci+gw/2,y:H-12,class:'ax','text-anchor':'middle'});
    t.textContent=c;svg.appendChild(t);
  });
  host.appendChild(svg);
}

// Linhas: eixo X categorico (degraus da escada), uma linha por path.
function lines(host, cats, series, fmt, unit){
  const W=760,H=250,ml=54,mr=64,mt=14,mb=34;
  const iw=W-ml-mr, ih=H-mt-mb;
  const all=series.flatMap(s=>s.vals.filter(v=>v!=null));
  const max=Math.max(...all,0.0001)*1.15;
  const svg=mk('svg',{viewBox:`0 0 ${W} ${H}`,role:'img'});
  for(let i=0;i<=4;i++){const y=mt+ih-ih*i/4;
    svg.appendChild(mk('line',{x1:ml,x2:W-mr,y1:y,y2:y,class:'gl'}));
    const t=mk('text',{x:ml-8,y:y+4,class:'ax','text-anchor':'end'});
    t.textContent=fmt(max*i/4);svg.appendChild(t)}
  const X=i=>cats.length<2?ml+iw/2:ml+iw*i/(cats.length-1);
  cats.forEach((c,i)=>{const t=mk('text',{x:X(i),y:H-12,class:'ax','text-anchor':'middle'});
    t.textContent=c;svg.appendChild(t)});
  series.forEach(s=>{
    const pts=s.vals.map((v,i)=>v==null?null:[X(i),mt+ih-ih*v/max]).filter(Boolean);
    if(!pts.length)return;
    svg.appendChild(mk('path',{d:'M'+pts.map(p=>p.join(' ')).join(' L '),fill:'none',
      stroke:C[s.id],'stroke-width':2,'stroke-linejoin':'round','stroke-linecap':'round'}));
    s.vals.forEach((v,i)=>{if(v==null)return;
      const cx=X(i),cy=mt+ih-ih*v/max;
      svg.appendChild(mk('circle',{cx,cy,r:5,fill:C[s.id],stroke:'var(--surface-1)','stroke-width':2}));
      const hit=mk('circle',{cx,cy,r:13,fill:'transparent'});
      hover(hit,`${cats[i]} · ${NAME[s.id]}: ${fmt(v)}${unit}`);svg.appendChild(hit)});
    const li=s.vals.map((v,i)=>v==null?-1:i).filter(i=>i>=0).pop();
    const t=mk('text',{x:X(li)+11,y:mt+ih-ih*s.vals[li]/max+4,class:'dl',fill:C[s.id]});
    t.textContent=NAME[s.id];svg.appendChild(t);
  });
  host.appendChild(svg);
}
document.querySelectorAll('[data-chart]').forEach(el=>{
  const c=DATA.charts[el.dataset.chart]; if(!c)return;
  const fmt=v=>v>=100?v.toFixed(0):v>=10?v.toFixed(1):v.toFixed(2);
  (c.kind==='lines'?lines:grouped)(el,c.cats,c.series,fmt,c.unit||'');
});
</script></body></html>"""


def build_html(A, meta, charts):
    body = []
    b = body.append

    b('<div class="tiles">')
    for k, v, n in meta["tiles"]:
        b(f'<div class="tile"><div class="k">{html.escape(k)}</div>'
          f'<div class="v">{html.escape(v)}</div><div class="n">{html.escape(n)}</div></div>')
    b("</div>")

    if meta["headline"]:
        b('<h2>Leitura dos resultados</h2><ul class="note">')
        for h in meta["headline"]:
            b(f"<li>{h}</li>")
        b("</ul>")

    b('<div class="legend">')
    for pid, pname, _, _ in PATHS:
        b(f'<span><i style="background:var(--series-{pid})"></i>{pid} · {pname}</span>')
    b("</div>")

    for cid, c in charts.items():
        b(f'<h3>{html.escape(c["title"])}</h3>')
        if c.get("note"):
            b(f'<p class="note">{c["note"]}</p>')
        b(f'<div class="card"><div data-chart="{cid}"></div></div>')

    # Tabela completa — obriga o modo relevo do aqua em tema claro e permite
    # reanalise sem abrir o CSV.
    b("<h2>Tabela completa</h2>")
    b('<div class="card"><table><thead><tr>'
      "<th>Degrau</th><th>Workload</th><th>Path</th><th>reps</th>"
      "<th>rps alvo</th><th>rps</th><th>descarte</th>"
      "<th>p50 ms</th><th>p95 ms</th><th>p99 ms</th><th>p99.9 ms</th>"
      "<th>erros</th><th>q/req</th><th>hit ratio</th></tr></thead><tbody>")
    for (rung, wl, pid), m in sorted(A.items(),
                                     key=lambda kv: (RUNGS.index(kv[0][0]) if kv[0][0] in RUNGS else 99,
                                                     kv[0][1], kv[0][2])):
        hr = f"{m['pgcache_hit_ratio']*100:.1f}%" if m["pgcache_hit_ratio"] else "—"
        qr = f"{m['queries_per_request']:.2f}" if m["queries_per_request"] else "—"
        mark = " ⚠" if m["collapsed"] else ""
        b(f"<tr><td>{rung}</td><td>{wl}</td><td>{pid} · {PATH_NAME[pid]}{mark}</td>"
          f"<td>{m['reps']}</td><td>{m['rate_target']:.0f}</td><td>{m['rate_achieved']:.0f}</td>"
          f"<td>{m['drop_ratio']*100:.0f}%</td>"
          f"<td>{m['p50_ms']:.2f}</td><td>{m['p95_ms']:.2f}</td><td>{m['p99_ms']:.2f}</td>"
          f"<td>{m['p999_ms']:.2f}</td><td>{m['errors']:.0f}</td><td>{qr}</td><td>{hr}</td></tr>")
    b("</tbody></table></div>")

    b('<h2>Metodologia</h2><ul class="note">'
      "<li>Gerador <b>open-loop</b>: a latência é medida do instante pretendido de chegada, "
      "não de quando um worker ficou livre — sem <i>coordinated omission</i>.</li>"
      "<li>Caches internos do OpenFGA desligados nos paths A e B, incluindo "
      "<code>--experimentals=\"\"</code>, que <b>não</b> é vazio por default.</li>"
      "<li>PgCache com <code>ALLOWED_TABLES=tuple,authorization_model</code>: a tabela "
      "<code>changelog</code> fica fora do cache porque sua query embute <code>NOW()</code> "
      "e não é determinística.</li>"
      "<li>Massa validada contra um oráculo analítico antes de medir, e correção diferencial "
      "A vs B (W7) como gate de entrada.</li>"
      "<li>Valores são a <b>mediana</b> entre repetições <b>da mesma taxa alvo</b>: "
      "execuções em taxas diferentes não são agregadas juntas.</li>"
      f"<li>⚠ marca combinações com descarte acima de {DROP_LIMIT*100:.0f}%. Nelas o "
      "caminho não sustentou a taxa e os percentis cobrem só as requisições que "
      "completaram — leia como saturação, não como latência.</li></ul>")

    return (HTML_TMPL
            .replace("__SUB__", html.escape(meta["sub"]))
            .replace("__BODY__", "\n".join(body))
            .replace("__DATA__", json.dumps({"charts": charts}, ensure_ascii=False)))


def build_charts(A):
    charts = {}
    rungs = sorted({k[0] for k in A}, key=lambda r: RUNGS.index(r) if r in RUNGS else 99)
    wls = sorted({k[1] for k in A})

    def series_for(wl, field, cats):
        out = []
        for pid, _, _, _ in PATHS:
            vals = [A[(r, wl, pid)][field] if (r, wl, pid) in A else None for r in cats]
            if any(v is not None for v in vals):
                out.append({"id": pid, "vals": vals})
        return out

    for wl in wls:
        cats = [r for r in rungs if any((r, wl, p) in A for p, _, _, _ in PATHS)]
        if not cats:
            continue
        for field, label, unit in (("p95_ms", "p95", " ms"), ("p99_ms", "p99", " ms")):
            s = series_for(wl, field, cats)
            if not s:
                continue
            charts[f"{wl}-{field}"] = {
                "title": f"{WL_TITLE.get(wl, wl)} — latência {label} por degrau",
                "kind": "lines" if len(cats) > 1 else "bars",
                "cats": cats, "series": s, "unit": unit,
                "note": "Menor é melhor. Cada ponto é a mediana das repetições daquele degrau.",
            }
        s = series_for(wl, "queries_per_request", cats)
        if s and any(any(v for v in x["vals"] if v) for x in s):
            charts[f"{wl}-ampl"] = {
                "title": f"{WL_TITLE.get(wl, wl)} — amplificação (queries no datastore por requisição)",
                "kind": "bars", "cats": cats, "series": s, "unit": "",
                "note": "Métrica do próprio OpenFGA. É a variável que a hipótese H2 diz "
                        "controlar o tamanho do ganho do cache.",
            }
        s = series_for(wl, "rate_achieved", cats)
        if s:
            charts[f"{wl}-rps"] = {
                "title": f"{WL_TITLE.get(wl, wl)} — throughput alcançado",
                "kind": "bars", "cats": cats, "series": s, "unit": " rps",
                "note": "Maior é melhor. Divergência entre alvo e alcançado indica saturação.",
            }
    # hit ratio do PgCache, so' path B
    cats = rungs
    vals_by_wl = []
    for wl in wls:
        v = [A[(r, wl, "B")]["pgcache_hit_ratio"] * 100 if (r, wl, "B") in A else None for r in cats]
        if any(x for x in v if x):
            vals_by_wl.append({"id": "B", "vals": v, "wl": wl})
    if vals_by_wl:
        charts["hit-ratio"] = {
            "title": "PgCache — hit ratio por degrau (path B)",
            "kind": "lines" if len(cats) > 1 else "bars",
            "cats": cats, "series": vals_by_wl[:1], "unit": "%",
            "note": "Fração das consultas servidas do cache local em vez da origem.",
        }
    return charts


def build_meta(rows, A):
    runs = len(rows)
    reps = {v["reps"] for v in A.values()} or {0}
    tiles, headline = [], []

    # Melhor ganho observado de B sobre A em p99
    best = None
    for (rung, wl, pid), m in A.items():
        if pid != "B" or m["collapsed"]:
            continue
        a = A.get((rung, wl, "A"))
        if not a or not a["p99_ms"] or a["collapsed"]:
            continue
        g = gain(a["p99_ms"], m["p99_ms"])
        if best is None or g > best[0]:
            best = (g, rung, wl, a["p99_ms"], m["p99_ms"])

    if best:
        g, rung, wl, pa, pb = best
        tiles.append(("melhor ganho p99", f"{g:+.0f}%", f"{rung} · {wl} — {pa:.1f} ms → {pb:.1f} ms"))
        verb = "reduz" if g > 0 else "aumenta"
        headline.append(f"No melhor caso medido ({rung}, {wl}) o PgCache <b>{verb} o p99 em "
                        f"{abs(g):.0f}%</b> ({pa:.1f} ms → {pb:.1f} ms).")

    # Amplificação máxima
    amp = max(((m["queries_per_request"], k) for k, m in A.items() if m["queries_per_request"]),
              default=None)
    if amp:
        tiles.append(("amplificação máx.", f"{amp[0]:.1f}×",
                      f"{amp[1][0]} · {amp[1][1]} · path {amp[1][2]} — queries por requisição"))

    hr = max((m["pgcache_hit_ratio"] for m in A.values() if m["pgcache_hit_ratio"]), default=0)
    if hr:
        tiles.append(("hit ratio máx. PgCache", f"{hr*100:.1f}%", "consultas servidas do cache"))

    errs = sum(m["errors"] for m in A.values())
    tiles.append(("erros totais", f"{errs:.0f}", "em todas as execuções"))
    tiles.append(("execuções", str(runs), f"{len(A)} combinações (degrau × workload × path)"))

    ncol = sum(1 for m in A.values() if m["collapsed"])
    if ncol:
        tiles.append(("combinações colapsadas", str(ncol),
                      f"descarte > {DROP_LIMIT*100:.0f}% — percentis sem referente"))
        headline.append(f"<b>{ncol} combinações colapsaram</b> (descarte acima de "
                        f"{DROP_LIMIT*100:.0f}%). Nelas o caminho não sustentou a taxa "
                        "oferecida; os percentis descrevem só quem passou pela fila e não "
                        "entram em nenhuma comparação de latência.")

    dists = {m["doc_dist"] for m in A.values()}
    if "uniform" in dists:
        headline.append("Há execuções com <code>-doc-dist=uniform</code>: o eixo documento "
                        "não tem localidade nenhuma, então <b>não existe WHERE repetido</b> "
                        "para o cache acertar. É o modo legado, útil como controle — não "
                        "como medida do PgCache.")

    # Onde o cache NAO ganha — a hipotese nula, publicada.
    losses = [(gain(A[(r, w, 'A')]["p99_ms"], A[(r, w, 'B')]["p99_ms"]), r, w)
              for (r, w, p) in A
              if p == "B" and not A[(r, w, p)]["collapsed"]
              and (r, w, "A") in A and A[(r, w, "A")]["p99_ms"]
              and not A[(r, w, "A")]["collapsed"]]
    neg = sorted([x for x in losses if x[0] < 0])
    if neg:
        headline.append("O PgCache <b>perde</b> em " +
                        ", ".join(f"{r}/{w} ({g:.0f}%)" for g, r, w in neg[:4]) +
                        " — publicado deliberadamente: quando a massa cabe na RAM da origem, "
                        "o hop extra do proxy não se paga (hipótese H0 do plano).")

    return {
        "generated": datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M UTC"),
        "runs": runs,
        "reps_note": f"{min(reps)}–{max(reps)} repetições por combinação, mediana reportada",
        "sub": f"Relatório gerado em {datetime.now(timezone.utc).strftime('%Y-%m-%d %H:%M UTC')} · "
               f"{runs} execuções · mediana de {min(reps)}–{max(reps)} repetições",
        "tiles": tiles,
        "headline": headline,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--results", default="results")
    ap.add_argument("--out", default="report")
    a = ap.parse_args()

    rows = load(a.results)
    A = agg(rows)
    meta = build_meta(rows, A)
    charts = build_charts(A)

    # `agg` elege UMA condicao por (degrau, workload) — a de maior cobertura de
    # paths. Nao ha' nada de errado nisso: e' o unico recorte em que A, B e C sao
    # comparaveis entre si. O errado era nao dizer QUAL condicao venceu.
    #
    # Na campanha r5, w1 e w2 foram reportados em `zipf` e w3 em `uniform`, na
    # mesma tabela, sem nenhuma indicacao — as duas distribuicoes tinham cobertura
    # e numero de repeticoes empatados em w3, e o desempate por execucao mais
    # recente entregou a vitoria a' `uniform`. Um leitor comparando linhas dessa
    # tabela estaria comparando dois experimentos diferentes.
    picked = {}
    for (rung, wl, p), m in A.items():
        picked[(rung, wl)] = (m["doc_dist"], m["rate_target"], m["campaign_id"])
    if len({v[:2] for v in picked.values()}) > 1:
        print("aviso: as combinacoes agregadas NAO estao todas na mesma condicao:")
        for (rung, wl), (dist, rate, camp) in sorted(picked.items()):
            print(f"  {rung}/{wl}: doc-dist={dist} taxa={rate:.0f} rps campanha={camp}")
        print("  comparar linhas de condicoes diferentes e' comparar experimentos diferentes.")

    off = discarded(rows, A)
    if off:
        # A mensagem antiga dizia "fora da taxa alvo comparavel", o que era falso
        # para quase todas: elas atingiram a taxa alvo exatamente. Ficaram de fora
        # por pertencerem a uma condicao que nao foi eleita, ou por faltar algum
        # path dentro daquela condicao.
        print(f"aviso: {len(off)} execucoes fora da condicao eleita, nao agregadas:")
        for r in off:
            print(f"  {r['rung']}/{r['workload']}/{r['path']} run={r['run_id']} "
                  f"dist={r.get('doc_dist') or '?'} alvo={r['rate_target']:.0f} rps "
                  f"alcancado={r['rate_achieved']:.1f} rps")

    os.makedirs(a.out, exist_ok=True)
    with open(os.path.join(a.out, "RELATORIO.md"), "w") as f:
        f.write(markdown(A, meta))
    with open(os.path.join(a.out, "relatorio.html"), "w") as f:
        f.write(build_html(A, meta, charts))
    with open(os.path.join(a.out, "dados-consolidados.csv"), "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["rung", "workload", "path", "path_name", "reps", "rate_target",
                    "rate_achieved", "samples", "drop_ratio", "collapsed", "doc_dist",
                    "p50_ms", "p95_ms", "p99_ms", "p999_ms", "max_ms", "mean_ms",
                    "errors", "dropped", "queries_per_request", "pgcache_hit_ratio"])
        for (r, wl, p), m in sorted(A.items()):
            w.writerow([r, wl, p, PATH_NAME[p], m["reps"], f"{m['rate_target']:.0f}",
                        f"{m['rate_achieved']:.2f}", f"{m['samples']:.0f}",
                        f"{m['drop_ratio']:.4f}", int(m["collapsed"]), m["doc_dist"],
                        f"{m['p50_ms']:.4f}", f"{m['p95_ms']:.4f}", f"{m['p99_ms']:.4f}",
                        f"{m['p999_ms']:.4f}", f"{m['max_ms']:.4f}", f"{m['mean_ms']:.4f}",
                        f"{m['errors']:.0f}", f"{m['dropped']:.0f}",
                        f"{m['queries_per_request']:.4f}", f"{m['pgcache_hit_ratio']:.6f}"])
    print(f"relatorio gerado em {a.out}/ ({len(A)} combinacoes, {len(rows)} execucoes)")


if __name__ == "__main__":
    main()

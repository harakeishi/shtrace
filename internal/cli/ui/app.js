"use strict";

/* ============================================================
   Data — loaded from the local shtrace API.
   ============================================================ */
let SPANS = [];            // newest-first, decorated with derived fields
let SESSIONS = [];         // session rows referenced by SPANS
const SESS_BY_ID = {};
let MAXDUR = 1;
let capped = false;

const el = id => document.getElementById(id);
const esc = s => String(s).replace(/[&<>"]/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;"}[c]));
const p2 = n => String(n).padStart(2, "0");

function fmtDur(ms){
  if(ms == null) return "—";
  if(ms < 1000) return ms + "ms";
  if(ms < 60000) return (ms/1000).toFixed(ms < 10000 ? 2 : 1) + "s";
  return Math.floor(ms/60000) + "m" + p2(Math.round(ms%60000/1000)) + "s";
}
function clock(ms){ const d = new Date(ms); return p2(d.getHours())+":"+p2(d.getMinutes())+":"+p2(d.getSeconds()); }
function hhmm(ms){ const d = new Date(ms); return p2(d.getHours())+":"+p2(d.getMinutes()); }
function iso(ms){
  if(ms == null) return "—";
  const d = new Date(ms);
  const off = -d.getTimezoneOffset();
  const sign = off < 0 ? "-" : "+";
  const oh = p2(Math.floor(Math.abs(off)/60)), om = p2(Math.abs(off)%60);
  return `${d.getFullYear()}-${p2(d.getMonth()+1)}-${p2(d.getDate())}T${p2(d.getHours())}:${p2(d.getMinutes())}:${p2(d.getSeconds())}.${String(d.getMilliseconds()).padStart(3,"0")}${sign}${oh}:${om}`;
}
function ymd(ms){ const d = new Date(ms); return `${d.getFullYear()}-${p2(d.getMonth()+1)}-${p2(d.getDate())}`; }
function shortId(id){ return id.length > 16 ? id.slice(0, 16) + "…" : id; }
// chips sit in a narrow column and must leave room for the label
function chipId(id){ return id.length > 10 ? id.slice(0, 10) + "…" : id; }

// stable hue per session id so the chip colour survives re-renders and view switches
function sessHue(id){
  let h = 0;
  for(let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) % 360;
  return h;
}
// A strict CSP forbids style attributes, so every computed dimension is
// emitted as a data-* hook and applied here after the markup lands.
function applyStyles(root){
  root.querySelectorAll(".schip[data-hue]").forEach(n => {
    const h = n.dataset.hue;
    n.style.setProperty("--sc-fg", `hsl(${h} 70% 72%)`);
    n.style.setProperty("--sc-bg", `hsl(${h} 42% 15%)`);
    n.style.setProperty("--sc-line", `hsl(${h} 38% 30%)`);
  });
  root.querySelectorAll("[data-h]").forEach(n => {
    n.style.height = n.dataset.h + "%";
    n.style.minHeight = "2px";
  });
  root.querySelectorAll("[data-w]").forEach(n => { n.style.width = n.dataset.w + "%"; });
  root.querySelectorAll("[data-left]").forEach(n => { n.style.left = n.dataset.left + "%"; });
  root.querySelectorAll(".durlbl[data-pos]").forEach(n => {
    if(n.dataset.side === "right"){ n.style.right = n.dataset.pos + "%"; n.style.marginRight = "6px"; }
    else { n.style.left = n.dataset.pos + "%"; n.style.marginLeft = "6px"; }
  });
}
function sessLabel(id){
  const s = SESS_BY_ID[id];
  return s && s.label ? s.label : "";
}
function sessChip(id, opts){
  const arrow = (opts && opts.arrow === false) ? "" : `<span class="arw">›</span>`;
  const label = sessLabel(id);
  const text = label ? `${esc(chipId(id))} · <span class="lbl">${esc(label)}</span>` : esc(shortId(id));
  const title = label ? `${id} · ${label}` : id;
  return `<button class="schip" data-sess="${esc(id)}" data-hue="${sessHue(id)}" title="${esc(title)} の waterfall を開く">
    <span class="sq"></span>${text}${arrow}</button>`;
}

/* ---------- ingest ---------- */
function ingest(page){
  SESSIONS = page.sessions || [];
  for(const k in SESS_BY_ID) delete SESS_BY_ID[k];
  SESSIONS.forEach(s => {
    s.tags = s.tags || {};
    s.startedMs = new Date(s.started_at).getTime();
    SESS_BY_ID[s.id] = s;
  });

  SPANS = (page.spans || []).map(sp => {
    const running = sp.exit_code === null || sp.exit_code === undefined;
    const start = new Date(sp.started_at).getTime();
    const end = new Date(sp.ended_at).getTime();
    return {
      id: sp.id,
      sess: sp.session_id,
      parent: sp.parent_span_id || null,
      cmd: (sp.argv && sp.argv.length ? sp.argv.join(" ") : sp.command),
      argv: sp.argv || [sp.command],
      group: sp.group || sp.command,
      cwd: sp.cwd || "",
      mode: sp.mode,
      exit: running ? null : sp.exit_code,
      absStart: start,
      absEnd: running ? null : end,
      dur: running ? Math.max(0, Date.now() - start) : Math.max(0, end - start),
      st: running ? "running" : (sp.exit_code === 0 ? "success" : "failed")
    };
  });
  SPANS.sort((a,b) => b.absStart - a.absStart);
  MAXDUR = Math.max(1, ...SPANS.map(s => s.dur));

  // waterfall works off the session's own spans, grouped from the flat list
  SESSIONS.forEach(s => { s.spans = []; });
  SPANS.forEach(sp => {
    const s = SESS_BY_ID[sp.sess];
    if(s) s.spans.push(sp);
  });
  SESSIONS.forEach(s => s.spans.sort((a,b) => a.absStart - b.absStart));
}

/* ============================================================
   View A — explorer state
   ============================================================ */
const FACETS = [
  {key:"st",   name:"Status",       vals:["success","failed","running"], dot:true},
  {key:"code", name:"Exit code",    get:s => s.exit === null ? "—" : String(s.exit)},
  {key:"mode", name:"Mode",         vals:["pty","pipe"]},
  {key:"bin",  name:"Command",      get:s => s.group},
  {key:"cwd",  name:"Cwd",          get:s => s.cwd},
  {key:"tag",  name:"Session tags", multi:s => sessTags(s.sess)}
];
const sel = {}; FACETS.forEach(f => sel[f.key] = new Set());
let query = "";
let selectedId = null;
let tab = "output";
let rangeMs = 0;
let tailTimer = null;

function sessTags(id){
  const s = SESS_BY_ID[id];
  if(!s) return [];
  return Object.entries(s.tags).map(([k,v]) => k+":"+v);
}

function valuesOf(f,s){
  if(f.multi) return f.multi(s);
  if(f.get) return [f.get(s)];
  return [s[f.key]];
}
function inRange(s){
  if(!rangeMs) return true;
  return s.absStart >= Date.now() - rangeMs;
}
function matchQuery(s){
  if(!query.trim()) return true;
  const tags = sessTags(s.sess);
  const hay = [s.cmd, s.cwd, s.st, s.mode, s.sess, sessLabel(s.sess), "exit:"+s.exit, ...tags].join(" ").toLowerCase();
  return query.toLowerCase().split(/\s+/).filter(Boolean).every(tok => {
    const m = tok.match(/^(status|command|cwd|mode|session|exit|tag):(.+)$/);
    if(m){
      const v = m[2].replace(/\*/g,"");
      if(m[1] === "status") return s.st.includes(v);
      if(m[1] === "command") return s.cmd.toLowerCase().includes(v) || s.group.toLowerCase().includes(v);
      if(m[1] === "cwd") return s.cwd.toLowerCase().includes(v);
      if(m[1] === "mode") return s.mode === v;
      if(m[1] === "session") return s.sess.toLowerCase().includes(v);
      if(m[1] === "exit") return String(s.exit) === v;
      if(m[1] === "tag") return tags.some(t => t.toLowerCase().includes(v));
    }
    return hay.includes(tok);
  });
}
// a facet's own selection is excluded when counting its own values
function passes(s, skipKey){
  if(!inRange(s)) return false;
  if(!matchQuery(s)) return false;
  return FACETS.every(f => {
    if(f.key === skipKey) return true;
    if(!sel[f.key].size) return true;
    return valuesOf(f,s).some(v => sel[f.key].has(v));
  });
}
const filtered = () => SPANS.filter(s => passes(s, null));
const isFiltering = () => query.trim() !== "" || rangeMs !== 0 || FACETS.some(f => sel[f.key].size > 0);

function renderFacets(){
  const box = el("facetList");
  const openState = {};
  box.querySelectorAll(".facet").forEach(n => openState[n.dataset.k] = n.classList.contains("collapsed"));
  box.innerHTML = FACETS.map(f => {
    const scope = SPANS.filter(s => passes(s, f.key));
    const counts = new Map();
    scope.forEach(s => valuesOf(f,s).forEach(v => { if(v !== "" && v != null) counts.set(v, (counts.get(v)||0)+1); }));
    sel[f.key].forEach(v => { if(!counts.has(v)) counts.set(v, 0); });
    const list = [...counts.entries()];
    list.sort((a,b) => f.vals ? f.vals.indexOf(a[0]) - f.vals.indexOf(b[0]) : (b[1]-a[1] || String(a[0]).localeCompare(String(b[0]))));
    const items = list.map(([v,c]) => {
      const on = sel[f.key].has(v);
      const label = f.key === "cwd" ? v.split("/").slice(-2).join("/") : v;
      return `<label class="fitem ${on?"on":""}" title="${esc(v)}">
        <input type="checkbox" data-f="${f.key}" data-v="${esc(String(v))}" ${on?"checked":""}>
        ${f.dot?`<span class="sdot s-${esc(v)}"></span>`:""}
        <span class="lbl">${esc(label)}</span><span class="cnt">${c}</span></label>`;
    }).join("");
    return `<div class="facet ${openState[f.key]?"collapsed":""}" data-k="${f.key}">
      <button class="facet-h"><span class="caret">▼</span>${f.name}</button>
      <div class="facet-body">${items || '<div class="facet-none">—</div>'}</div></div>`;
  }).join("");
  applyStyles(box);
}

function renderHisto(){
  const rows = filtered();
  const bars = el("bars"), axis = el("histoAxis");
  if(!SPANS.length){ bars.innerHTML = ""; axis.innerHTML = ""; return; }
  const lo = Math.min(...SPANS.map(s => s.absStart));
  const hi = Math.max(...SPANS.map(s => s.absStart)) + 1;
  const N = 40, w = Math.max(1, (hi - lo) / N);
  const b = Array.from({length:N}, () => ({ok:0, fail:0}));
  rows.forEach(s => {
    let i = Math.floor((s.absStart - lo) / w);
    i = Math.max(0, Math.min(N-1, i));
    if(s.st === "failed") b[i].fail++; else b[i].ok++;
  });
  const max = Math.max(1, ...b.map(x => x.ok + x.fail));
  bars.innerHTML = b.map((x,i) => {
    const tot = x.ok + x.fail;
    return `<div class="bar-h">
      <div class="tip">${hhmm(lo + i*w)} · ${tot} spans${x.fail?` · <span class="u-err">${x.fail} failed</span>`:""}</div>
      <div class="track"></div>
      ${x.fail?`<div class="fail" data-h="${(x.fail/max*66).toFixed(3)}"></div>`:""}
      ${x.ok?`<div class="ok" data-h="${(x.ok/max*66).toFixed(3)}"></div>`:""}
    </div>`;
  }).join("");
  axis.innerHTML = [0, .25, .5, .75, 1].map(f => `<span>${hhmm(lo + (hi-lo)*f)}</span>`).join("");
  applyStyles(bars);
}

function renderSummary(){
  const rows = filtered();
  const sessCount = new Set(rows.map(s => s.sess)).size;
  el("summaryLead").innerHTML =
    `<b>${rows.length}</b> span${rows.length===1?"":"s"} across <b>${sessCount}</b> session${sessCount===1?"":"s"}`;
  const allSess = new Set(SPANS.map(s => s.sess)).size;
  el("summaryFrom").textContent =
    isFiltering() ? `· filtered from ${SPANS.length} spans / ${allSess} sessions` : "";
}

function renderChips(){
  const parts = [];
  FACETS.forEach(f => sel[f.key].forEach(v => {
    const label = f.key === "cwd" ? v.split("/").pop() : v;
    parts.push(`<span class="chip">${f.name.toLowerCase().replace(/ /g,"_")}:${esc(label)}<button data-f="${f.key}" data-v="${esc(String(v))}">✕</button></span>`);
  }));
  if(query.trim()) parts.push(`<span class="chip">q:${esc(query.trim())}<button data-clearq="1">✕</button></span>`);
  const c = el("chips");
  c.innerHTML = parts.length ? parts.join("") : `<span class="u-mute">なし — 全 span を表示中</span>`;
  applyStyles(c);
}

function renderRows(){
  const rows = filtered();
  el("empty").style.display = rows.length ? "none" : "block";
  el("rows").innerHTML = rows.map(s => {
    const code = s.exit === null ? "—" : "exit " + s.exit;
    return `<tr data-id="${esc(s.id)}" class="${s.id===selectedId?"sel":""}">
      <td class="c-bar"><div class="b-${s.st}"></div></td>
      <td class="c-time">${clock(s.absStart)}</td>
      <td class="c-status"><span class="stat ${s.st}"><span class="sdot s-${s.st}"></span><span class="code">${code}</span></span></td>
      <td class="c-dur"><span class="durwrap ${s.st}"><span class="num">${fmtDur(s.dur)}</span>
        <span class="track"><span class="fill" data-w="${Math.max(3, Math.sqrt(s.dur/MAXDUR)*100).toFixed(3)}"></span></span></span></td>
      <td class="c-cmd" title="${esc(s.cmd)}">${s.parent?'<span class="kidmark">└</span>':""}${esc(s.cmd)}${s.mode==="pty"?'<span class="tag">pty</span>':""}</td>
      <td class="c-cwd" title="${esc(s.cwd)}">…/${esc(s.cwd.split("/").slice(-2).join("/"))}</td>
      <td class="c-sess">${sessChip(s.sess)}</td>
    </tr>`;
  }).join("");
  applyStyles(el("rows"));
}

/* ---------- output loading ---------- */
const outputCache = new Map();
function fetchOutput(sessID, spanID){
  const key = sessID + "/" + spanID;
  if(outputCache.has(key)) return Promise.resolve(outputCache.get(key));
  return fetch(`/api/output/${encodeURIComponent(sessID)}/${encodeURIComponent(spanID)}`)
    .then(r => {
      if(r.status === 404) return "(no output captured)";
      if(!r.ok) throw new Error("HTTP " + r.status);
      return r.text();
    })
    .then(txt => { outputCache.set(key, txt); return txt; });
}
// a finished span's output never changes; only running ones need re-fetching
function evictRunningOutput(){
  SPANS.forEach(s => { if(s.st === "running") outputCache.delete(s.sess + "/" + s.id); });
}
// classify a plain output line so real logs get the mock's colour treatment
function lineClass(t){
  if(/^\s*(FAIL|ERROR|error:|E\s|panic:|\-\-\- FAIL|✗)/.test(t) || /\berror\b/i.test(t)) return "o-err";
  if(/^\s*(ok\s|PASS|--- PASS|✓)/.test(t) || /\b(succeeded|success)\b/i.test(t)) return "o-ok";
  if(/^\s*(WARN|warning:|hint:)/i.test(t)) return "o-warn";
  return "o-plain";
}
let termGen = 0;
function renderTerm(container, sessID, spanID){
  // a slower earlier fetch must not paint over whatever is current now
  const gen = ++termGen;
  container.dataset.termGen = String(gen);
  const stale = () => container.dataset.termGen !== String(gen);
  container.innerHTML = `<div class="termbar">GET /api/output/${esc(sessID)}/${esc(spanID)}<span class="spacer"></span>
      <button data-term-raw="1">raw</button></div><div class="loading">Loading output…</div>`;
  fetchOutput(sessID, spanID).then(txt => {
    if(stale()) return;
    const lines = txt.replace(/\n$/, "").split("\n");
    const body = lines.length === 1 && lines[0] === ""
      ? `<div class="ln"><span class="no">1</span><span class="tx o-dim">(empty)</span></div>`
      : lines.map((t,i) => `<div class="ln"><span class="no">${i+1}</span><span class="tx ${lineClass(t)}">${esc(t)||"&nbsp;"}</span></div>`).join("");
    const bar = container.querySelector(".termbar");
    container.innerHTML = "";
    if(bar) container.appendChild(bar);
    const term = document.createElement("div");
    term.className = "term";
    term.innerHTML = body;
    container.appendChild(term);
  }).catch(e => {
    if(stale()) return;
    const box = container.querySelector(".loading");
    if(box){ box.className = "errbox"; box.textContent = "output load failed: " + e.message; }
  });
}

/* ---------- detail panel ---------- */
function openDetail(id){
  selectedId = id;
  const s = SPANS.find(x => x.id === id);
  if(!s) return;
  el("detail").classList.add("open");
  el("dCmd").textContent = s.cmd;
  const st = s.st === "success" ? "ok" : s.st === "failed" ? "err" : "run";
  el("dMeta").innerHTML = `
    <span class="pill ${st}">${s.st}${s.exit===null?"":" · exit "+s.exit}</span>
    <span class="pill">${fmtDur(s.dur)}</span>
    <span class="pill">${esc(s.mode)}</span>
    ${sessChip(s.sess)}
    <span class="pill">${esc(shortId(s.id))}</span>`;
  applyStyles(el("dMeta"));
  renderTab();
  renderRows();
}
function closeDetail(){
  selectedId = null;
  el("detail").classList.remove("open");
  el("dCmd").textContent = "";
  el("dMeta").innerHTML = "";
  el("dBody").innerHTML = "";
}
function renderTab(){
  const s = SPANS.find(x => x.id === selectedId);
  const body = el("dBody");
  if(!s){ body.innerHTML = ""; return; }
  document.querySelectorAll("#dTabs button").forEach(b => b.classList.toggle("on", b.dataset.t === tab));

  if(tab === "output"){
    renderTerm(body, s.sess, s.id);
  } else if(tab === "meta"){
    const sess = SESS_BY_ID[s.sess] || {tags:{}, startedMs:null};
    const sect = (title, kv) => `<div class="sec">${title}</div><table class="kv">${
      kv.map(([k,v]) => `<tr><th>${k}</th><td>${v}</td></tr>`).join("")}</table>`;
    body.innerHTML =
      sect("Span", [
        ["id", esc(s.id)],
        ["session_id", sessChip(s.sess)],
        ["parent_span_id", s.parent ? esc(s.parent) : "<span class='u-mute'>null (root)</span>"],
        ["command", esc(s.cmd)],
        ["argv", esc(JSON.stringify(s.argv))],
        ["command_group", esc(s.group)],
        ["cwd", esc(s.cwd)],
        ["mode", esc(s.mode)],
        ["started_at", iso(s.absStart)],
        ["ended_at", s.absEnd === null ? "<span class='u-run'>null (running)</span>" : iso(s.absEnd)],
        ["duration", fmtDur(s.dur)],
        ["exit_code", s.exit === null ? "<span class='u-run'>null</span>"
          : `<span class="${s.exit?"u-err":"u-ok"}">${s.exit}</span>`]
      ]) +
      sect("Session", [
        ["id", sessChip(s.sess)],
        ["label", sessLabel(s.sess) ? esc(sessLabel(s.sess)) : "<span class='u-mute'>—</span>"],
        ["started_at", sess.startedMs ? iso(sess.startedMs) : "—"],
        ...Object.entries(sess.tags).map(([k,v]) => ["tags."+esc(k), esc(v)])
      ]);
  } else {
    const sibs = (SESS_BY_ID[s.sess] || {spans:[]}).spans;
    body.innerHTML = `<div class="rel-head">session ${sessChip(s.sess)} の span (${sibs.length})
        <span class="u-grow"></span>
        <button class="actionbtn" data-open-sess="${esc(s.sess)}">waterfall で見る →</button></div>
      <div class="rel">${sibs.map(x => `<div class="rel-item ${x.id===s.id?"self":""}" data-id="${esc(x.id)}">
        <span class="sdot s-${x.st}"></span>
        <span class="ind">${x.parent?"└─ ":""}</span>
        <span class="c">${esc(x.cmd)}</span><span class="d">${fmtDur(x.dur)}</span></div>`).join("")}</div>`;
  }
  applyStyles(body);
}

function renderA(){ renderFacets(); renderChips(); renderHisto(); renderSummary(); renderRows(); }

/* ============================================================
   View B — waterfall
   ============================================================ */
let curSession = null;
let curSpan = null;
let wTab = "output";

const isRunning = sp => sp.exit === null;
const statusOf = sp => isRunning(sp) ? "run" : (sp.exit === 0 ? "ok" : "err");
function sessionBase(s){ return Math.min(s.startedMs || Infinity, ...s.spans.map(sp => sp.absStart)); }
function totalOf(s){
  const base = sessionBase(s);
  return Math.max(1, ...s.spans.map(sp => (sp.absStart - base) + sp.dur));
}

function tree(spans){
  const byId = new Set(spans.map(sp => sp.id));
  const byParent = new Map();
  spans.forEach(sp => {
    // an unknown parent (span outside this page) is treated as a root
    const k = (sp.parent && byId.has(sp.parent)) ? sp.parent : "__root__";
    if(!byParent.has(k)) byParent.set(k, []);
    byParent.get(k).push(sp);
  });
  byParent.forEach(v => v.sort((a,b) => a.absStart - b.absStart));
  const out = [];
  const seen = new Set();
  (function walk(key, depth, ancestorsLast){
    (byParent.get(key) || []).forEach((sp,i,arr) => {
      if(seen.has(sp.id)) return;
      seen.add(sp.id);
      const last = i === arr.length - 1;
      out.push({sp, depth, ancestorsLast: ancestorsLast.slice(), last, hasKids: byParent.has(sp.id)});
      walk(sp.id, depth+1, ancestorsLast.concat(last));
    });
  })("__root__", 0, []);
  return out;
}
function ticks(totalMs){
  const targets = [50,100,250,500,1000,2000,5000,10000,15000,20000,30000,60000,120000,300000,600000];
  const step = targets.find(t => totalMs/t <= 9) || 900000;
  const arr = [];
  for(let t = 0; t <= totalMs; t += step) arr.push(t);
  return arr;
}

function renderWaterfallHeader(){
  const s = curSession, t = totalOf(s), base = sessionBase(s);
  const fail = s.spans.filter(sp => sp.exit != null && sp.exit !== 0).length;
  const run = s.spans.filter(isRunning).length;
  const label = sessLabel(s.id);
  el("crumbCur").textContent = label ? `${shortId(s.id)} · ${label}` : shortId(s.id);
  el("traceHeader").innerHTML = `
    <div class="th-top">
      <span class="th-title">${esc(shortId(s.id))}${label?` <span class="u-dim">· ${esc(label)}</span>`:""}</span>
      ${Object.entries(s.tags).map(([k,v]) => `<span class="tagchip"><span class="kk">${esc(k)}:</span>${esc(v)}</span>`).join("")}
    </div>
    <div class="th-stats">
      <div class="tstat"><div class="lbl">Started</div><div class="val">${clock(base)} <small>${ymd(base)}</small></div></div>
      <div class="tstat"><div class="lbl">Duration</div><div class="val">${fmtDur(t)}</div></div>
      <div class="tstat"><div class="lbl">Spans</div><div class="val">${s.spans.length}</div></div>
      <div class="tstat"><div class="lbl">Failed</div><div class="val ${fail?"err":"ok"}">${fail}</div></div>
      <div class="tstat"><div class="lbl">Status</div><div class="val ${run?"run":(fail?"err":"ok")}">${run?"running":(fail?"error":"ok")}</div></div>
    </div>`;
  applyStyles(el("traceHeader"));
}

function renderWaterfall(){
  const s = curSession, t = totalOf(s), base = sessionBase(s);
  const tk = ticks(t);
  el("axis").innerHTML = tk.map(v =>
    `<i class="tick" data-left="${(v/t*100).toFixed(3)}"><span>${v===0?"0":fmtDur(v)}</span></i>`).join("");
  const grid = tk.map(v => `<i data-left="${(v/t*100).toFixed(3)}"></i>`).join("");

  const rows = tree(s.spans);
  el("wfHint").textContent = `${rows.length} spans · 行クリックで詳細 · バー hover で概要`;
  el("wfBody").innerHTML = rows.map(({sp, depth, ancestorsLast, hasKids}) => {
    const st = statusOf(sp);
    const left = (sp.absStart - base) / t * 100;
    const width = Math.max(0.35, sp.dur / t * 100);
    const labelRight = left + width > 78;
    const guides = ancestorsLast.map(l => `<i class="${l?"":"v"}"></i>`).join("");
    const branch = depth > 0 ? `<i class="b"></i>` : "";
    const argTail = sp.argv.slice(1).join(" ");
    return `<div class="wrow ${curSpan && curSpan.id === sp.id ? "sel" : ""}" data-span="${esc(sp.id)}">
      <div class="r-name">
        <span class="tree">${guides}${branch}</span>
        <span class="r-dot ${st==="ok"?"":st}"></span>
        <span class="r-cmd" title="${esc(sp.cmd)}">${esc(sp.argv[0] || sp.cmd)}${argTail?` <span class="arg">${esc(argTail)}</span>`:""}</span>
        <span class="r-mode">${esc(sp.mode)}</span>
      </div>
      <div class="r-track">
        <div class="grid">${grid}</div>
        <div class="wbar ${st==="ok"?"":st} ${hasKids?"parent":""}" data-left="${left.toFixed(3)}" data-w="${width.toFixed(3)}"></div>
        <div class="durlbl" data-side="${labelRight?"right":"left"}" data-pos="${labelRight
          ? Math.max(0, 100-left-width).toFixed(3)
          : (left+width).toFixed(3)}">${isRunning(sp)?"running…":fmtDur(sp.dur)}</div>
      </div>
    </div>`;
  }).join("");
  applyStyles(el("axis"));
  applyStyles(el("wfBody"));
}

function renderWDetail(){
  const head = el("wdHead"), body = el("wdBody");
  if(!curSpan){
    head.innerHTML = `<span class="d-cmd u-mute">span を選択すると Output / Metadata を表示</span>`;
    body.innerHTML = "";
    return;
  }
  const sp = curSpan, st = statusOf(sp);
  head.innerHTML = `
    <span class="pill ${st}">${isRunning(sp)?"running":"exit "+sp.exit}</span>
    <span class="d-cmd">${esc(sp.cmd)}</span>
    <span class="wtabs">
      <button data-wtab="output" class="${wTab==="output"?"on":""}">Output</button>
      <button data-wtab="meta" class="${wTab==="meta"?"on":""}">Metadata</button>
    </span>
    <span class="spacer"></span>
    <button class="actionbtn" id="otherRuns">⤺ このコマンドの他の実行を見る</button>
    <button class="d-close" id="wdClose">×</button>`;
  applyStyles(head);

  if(wTab === "output"){
    renderTerm(body, curSession.id, sp.id);
  } else {
    body.innerHTML = `<div class="meta"><table>
      <tr><td class="k">span_id</td><td class="v">${esc(sp.id)}</td></tr>
      <tr><td class="k">session_id</td><td class="v">${esc(curSession.id)}</td></tr>
      <tr><td class="k">parent_span_id</td><td class="v">${sp.parent ? esc(sp.parent) : '<span class="u-mute">null (root span)</span>'}</td></tr>
      <tr><td class="k">command</td><td class="v">${esc(sp.cmd)}</td></tr>
      <tr><td class="k">command_group</td><td class="v">${esc(sp.group)}</td></tr>
      <tr><td class="k">argv</td><td class="v"><span class="argv">${sp.argv.map(a => `<span>${esc(a)}</span>`).join("")}</span></td></tr>
      <tr><td class="k">cwd</td><td class="v">${esc(sp.cwd)}</td></tr>
      <tr><td class="k">mode</td><td class="v">${esc(sp.mode)}</td></tr>
      <tr><td class="k">started_at</td><td class="v">${iso(sp.absStart)}</td></tr>
      <tr><td class="k">ended_at</td><td class="v">${sp.absEnd===null?'<span class="u-run">null (実行中)</span>':iso(sp.absEnd)}</td></tr>
      <tr><td class="k">duration</td><td class="v">${isRunning(sp)?"—":fmtDur(sp.dur)}</td></tr>
      <tr><td class="k">exit_code</td><td class="v ${st==="err"?"u-err":st==="ok"?"u-ok":"u-run"}">${sp.exit===null?"null":sp.exit}</td></tr>
    </table><div class="api">GET /api/output/${esc(curSession.id)}/${esc(sp.id)}</div></div>`;
    applyStyles(body);
  }
}
function renderB(){ renderWaterfallHeader(); renderWaterfall(); renderWDetail(); }

/* ============================================================
   Routing — hash drives which view is shown; explorer state
   lives in module scope so it survives the round trip.
   ============================================================ */
function showView(name){
  el("viewA").classList.toggle("on", name === "A");
  el("viewB").classList.toggle("on", name === "B");
  el("tip").style.display = "none";
}
function routedSessionID(){
  const m = location.hash.match(/^#\/session\/(.+)$/);
  return m ? decodeURIComponent(m[1]) : null;
}
const cappedNotice = () => capped ? "Showing the newest 1000 spans." : "";
// the cap is usually why a session is missing, so keep that context in the message
function notFoundNotice(id){
  return `session ${id} は読み込んだ範囲に見つかりません`
    + (capped ? "（最新 1000 span のみ読み込み済み）" : "");
}
function applyRoute(){
  const id = routedSessionID();
  if(id && SESS_BY_ID[id]){
    const same = curSession && curSession.id === id;
    const keepSpanID = same && curSpan ? curSpan.id : null;
    curSession = SESS_BY_ID[id];
    if(!same) wTab = "output";
    // ingest() rebuilds every span object, so re-resolve the selection by id
    curSpan = keepSpanID ? (curSession.spans.find(sp => sp.id === keepSpanID) || null) : null;
    el("notice").textContent = cappedNotice();
    showView("B");
    renderB();
    el("wDetail").classList.toggle("collapsed", !curSpan);
  } else {
    el("notice").textContent = id ? notFoundNotice(id) : cappedNotice();
    showView("A");
    renderA();
  }
}
function gotoSession(id){
  if(!SESS_BY_ID[id]) return;
  location.hash = "#/session/" + encodeURIComponent(id);
}
function gotoList(){
  if(location.hash) location.hash = ""; else applyRoute();
}

/* filter reset used by the reverse link */
function applyCommandFilter(group){
  FACETS.forEach(f => sel[f.key].clear());
  sel.bin.add(group);
  query = "";
  el("q").value = "";
  closeDetail();
}

/* ---------- events: view A ---------- */
el("facetList").addEventListener("change", e => {
  const i = e.target;
  if(i.tagName !== "INPUT") return;
  const set = sel[i.dataset.f];
  if(i.checked) set.add(i.dataset.v); else set.delete(i.dataset.v);
  renderA();
});
el("facetList").addEventListener("click", e => {
  const h = e.target.closest(".facet-h");
  if(h) h.parentElement.classList.toggle("collapsed");
});
el("chips").addEventListener("click", e => {
  const b = e.target.closest("button");
  if(!b) return;
  if(b.dataset.clearq){ query = ""; el("q").value = ""; }
  else if(b.dataset.f) sel[b.dataset.f].delete(b.dataset.v);
  renderA();
});
el("clearAll").addEventListener("click", () => {
  FACETS.forEach(f => sel[f.key].clear());
  el("q").value = ""; query = "";
  renderA();
});
el("q").addEventListener("input", e => { query = e.target.value; renderA(); });
el("rows").addEventListener("click", e => {
  const chip = e.target.closest(".schip");
  if(chip){ e.stopPropagation(); gotoSession(chip.dataset.sess); return; }
  const tr = e.target.closest("tr[data-id]");
  if(tr) openDetail(tr.dataset.id);
});
el("dClose").addEventListener("click", () => { closeDetail(); renderRows(); });
el("dTabs").addEventListener("click", e => {
  const b = e.target.closest("button[data-t]");
  if(b){ tab = b.dataset.t; renderTab(); }
});
el("dMeta").addEventListener("click", e => {
  const chip = e.target.closest(".schip");
  if(chip) gotoSession(chip.dataset.sess);
});
el("dBody").addEventListener("click", e => {
  const chip = e.target.closest(".schip");
  if(chip){ gotoSession(chip.dataset.sess); return; }
  const btn = e.target.closest("[data-open-sess]");
  if(btn){ gotoSession(btn.dataset.openSess); return; }
  const raw = e.target.closest("[data-term-raw]");
  if(raw){
    const s = SPANS.find(x => x.id === selectedId);
    if(s) window.open(`/api/output/${encodeURIComponent(s.sess)}/${encodeURIComponent(s.id)}`, "_blank");
    return;
  }
  const it = e.target.closest(".rel-item[data-id]");
  if(it) openDetail(it.dataset.id);
});
el("range").addEventListener("click", e => {
  const b = e.target.closest("button");
  if(!b) return;
  document.querySelectorAll("#range button").forEach(x => x.classList.remove("on"));
  b.classList.add("on");
  rangeMs = Number(b.dataset.ms) || 0;
  renderA();
});
el("tail").addEventListener("click", e => {
  const on = e.currentTarget.classList.toggle("on");
  if(tailTimer){ clearInterval(tailTimer); tailTimer = null; }
  if(on) tailTimer = setInterval(() => { evictRunningOutput(); load(); }, 3000);
});

/* ---------- events: view B ---------- */
el("crumbBack").addEventListener("click", gotoList);
el("backToList").addEventListener("click", gotoList);

el("wfBody").addEventListener("click", e => {
  const row = e.target.closest(".wrow[data-span]");
  if(!row) return;
  curSpan = curSession.spans.find(x => x.id === row.dataset.span);
  el("wDetail").classList.remove("collapsed");
  renderWaterfall(); renderWDetail();
});
el("wfBody").addEventListener("mousemove", e => {
  const bar = e.target.closest(".wbar");
  const tip = el("tip");
  if(!bar){ tip.style.display = "none"; return; }
  const sp = curSession.spans.find(x => x.id === bar.closest(".wrow").dataset.span);
  if(!sp){ tip.style.display = "none"; return; }
  tip.innerHTML = `<div class="t-cmd">${esc(sp.cmd)}</div>
    <div class="t-line"><b>span</b>${esc(sp.id)}</div>
    <div class="t-line"><b>duration</b>${isRunning(sp)?"running…":fmtDur(sp.dur)}</div>
    <div class="t-line"><b>offset</b>+${fmtDur(sp.absStart - sessionBase(curSession))} from session start</div>
    <div class="t-line"><b>exit_code</b>${sp.exit === null ? "null" : sp.exit}</div>
    <div class="t-line"><b>mode</b>${esc(sp.mode)}</div>
    <div class="t-line"><b>cwd</b>${esc(sp.cwd)}</div>`;
  tip.style.display = "block";
  tip.style.left = Math.min(e.clientX + 14, innerWidth - tip.offsetWidth - 10) + "px";
  tip.style.top = Math.max(8, Math.min(e.clientY + 16, innerHeight - tip.offsetHeight - 10)) + "px";
});
el("wfBody").addEventListener("mouseleave", () => { el("tip").style.display = "none"; });
el("wdHead").addEventListener("click", e => {
  const t = e.target.closest("button[data-wtab]");
  if(t){ wTab = t.dataset.wtab; renderWDetail(); return; }
  if(e.target.closest("#wdClose")){ el("wDetail").classList.toggle("collapsed"); return; }
  if(e.target.closest("#otherRuns") && curSpan){
    applyCommandFilter(curSpan.group);
    gotoList();
  }
});
el("wdBody").addEventListener("click", e => {
  const raw = e.target.closest("[data-term-raw]");
  if(raw && curSpan) window.open(`/api/output/${encodeURIComponent(curSession.id)}/${encodeURIComponent(curSpan.id)}`, "_blank");
});

document.addEventListener("keydown", e => {
  if(e.key === "Escape"){
    if(el("viewB").classList.contains("on")){ gotoList(); return; }
    closeDetail(); renderRows();
  }
  if(e.key === "/" && document.activeElement.id !== "q" && el("viewA").classList.contains("on")){
    e.preventDefault(); el("q").focus();
  }
});

window.addEventListener("hashchange", applyRoute);

/* ---------- boot ---------- */
function load(){
  return fetch("/api/spans")
    .then(r => {
      if(!r.ok) throw new Error("HTTP " + r.status);
      capped = r.headers.get("X-Shtrace-Spans-Capped") === "true";
      return r.json();
    })
    .then(page => {
      ingest(page);
      if(selectedId && !SPANS.some(s => s.id === selectedId)) closeDetail();
      applyRoute();
      // ingest() rebuilds every span object, so redraw the panel from the fresh one
      if(selectedId) openDetail(selectedId);
    })
    .catch(e => {
      el("rows").innerHTML = "";
      el("empty").style.display = "none";
      el("notice").textContent = "API 読み込みに失敗しました: " + e.message;
    });
}

el("hostLabel").textContent = "local only · " + location.host;
load();

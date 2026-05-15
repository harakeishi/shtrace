package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/harakeishi/shtrace/internal/storage"
)

const defaultServePort = 7474

// serveUsage is the canonical help line for shtrace serve.
const serveUsage = "usage: shtrace serve [--port <port>]"

func parseServeArgs(args []string) (port int, err error) {
	port = defaultServePort
	for i := 0; i < len(args); i++ {
		a := args[i]
		var raw string
		switch {
		case a == "--port":
			if i+1 >= len(args) {
				return 0, fmt.Errorf("--port requires a value")
			}
			raw = args[i+1]
			i++
		case strings.HasPrefix(a, "--port="):
			raw = strings.TrimPrefix(a, "--port=")
		default:
			return 0, fmt.Errorf("unknown serve flag %q", a)
		}
		n, parseErr := strconv.Atoi(raw)
		if parseErr != nil || n < 1 || n > 65535 {
			return 0, fmt.Errorf("--port %q is not a valid port number", raw)
		}
		port = n
	}
	return port, nil
}

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	port, err := parseServeArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		_, _ = fmt.Fprintln(stderr, serveUsage)
		return 2
	}

	env := envMap()
	dataDir, err := storage.ResolveDataDir(env, runtime.GOOS)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: %v\n", err)
		return 1
	}

	store, err := storage.Open(filepath.Join(dataDir, "sessions.db"))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: open store: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: migrate: %v\n", err)
		return 1
	}

	var fts *storage.FTSStore
	ftsPath := storage.FTSPath(dataDir)
	if _, statErr := os.Stat(ftsPath); statErr == nil {
		ftsStore, ftsErr := storage.OpenFTS(ftsPath)
		if ftsErr != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: open fts (search disabled): %v\n", ftsErr)
		} else if migrErr := ftsStore.MigrateFTS(ctx); migrErr != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: fts migrate (search disabled): %v\n", migrErr)
			_ = ftsStore.Close()
		} else {
			fts = ftsStore
			defer func() { _ = fts.Close() }()
		}
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "shtrace: listen %s: %v\n", addr, err)
		return 1
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/sessions", makeSessionsHandler(store))
	mux.HandleFunc("/api/sessions/", makeSpansHandler(store))
	mux.HandleFunc("/api/output/", makeOutputHandler(store, dataDir))
	mux.HandleFunc("/api/search", makeSearchHandler(fts))
	mux.HandleFunc("/", makeUIHandler())

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	_, _ = fmt.Fprintf(stdout, "shtrace serve: listening on http://%s  (Ctrl-C to stop)\n", addr)

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutErr := srv.Shutdown(shutCtx); shutErr != nil {
			_, _ = fmt.Fprintf(stderr, "shtrace: serve shutdown: %v\n", shutErr)
		}
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		_, _ = fmt.Fprintf(stderr, "shtrace: serve: %v\n", err)
		return 1
	}
	return 0
}

// writeJSON sends v as JSON with a 200 status. Marshalling errors are reported
// as 500 because they indicate a programming bug, not a client error.
func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

type apiSession struct {
	ID        string            `json:"id"`
	StartedAt string            `json:"started_at"`
	EndedAt   *string           `json:"ended_at"`
	Tags      map[string]string `json:"tags"`
}

type apiSpan struct {
	ID        string   `json:"id"`
	SessionID string   `json:"session_id"`
	Command   string   `json:"command"`
	Argv      []string `json:"argv"`
	Cwd       string   `json:"cwd"`
	Mode      string   `json:"mode"`
	StartedAt string   `json:"started_at"`
	EndedAt   string   `json:"ended_at"`
	ExitCode  *int     `json:"exit_code"`
}

type apiSearchResult struct {
	SpanID    string `json:"span_id"`
	SessionID string `json:"session_id"`
	Snippet   string `json:"snippet"`
}

func makeSessionsHandler(store *storage.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		const sessionCap = 500
		// Request one extra to detect whether the list was capped.
		sessions, err := store.ListSessions(r.Context(), sessionCap+1, nil)
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		capped := len(sessions) > sessionCap
		if capped {
			sessions = sessions[:sessionCap]
		}
		out := make([]apiSession, 0, len(sessions))
		for _, s := range sessions {
			a := apiSession{
				ID:        s.ID,
				StartedAt: s.StartedAt.Format(time.RFC3339),
				Tags:      s.Tags,
			}
			if s.EndedAt != nil {
				t := s.EndedAt.Format(time.RFC3339)
				a.EndedAt = &t
			}
			out = append(out, a)
		}
		// Marshal before setting any headers so that a marshal failure
		// (http.Error → 500) does not emit X-Shtrace-Sessions-Capped
		// alongside a non-200 status, which would be contradictory.
		b, marshalErr := json.Marshal(out)
		if marshalErr != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if capped {
			w.Header().Set("X-Shtrace-Sessions-Capped", "true")
		}
		_, _ = w.Write(b)
	}
}

func makeSpansHandler(store *storage.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// path: /api/sessions/{id}/spans
		path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
		if !strings.HasSuffix(path, "/spans") {
			http.NotFound(w, r)
			return
		}
		sessionID := strings.TrimSuffix(path, "/spans")
		if sessionID == "" || strings.Contains(sessionID, "/") {
			http.Error(w, "invalid session id", http.StatusBadRequest)
			return
		}

		spans, err := store.SpansForSession(r.Context(), sessionID, nil)
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		out := make([]apiSpan, 0, len(spans))
		for _, sp := range spans {
			out = append(out, apiSpan{
				ID:        sp.ID,
				SessionID: sp.SessionID,
				Command:   sp.Command,
				Argv:      sp.Argv,
				Cwd:       sp.Cwd,
				Mode:      sp.Mode,
				StartedAt: sp.StartedAt.Format(time.RFC3339),
				EndedAt:   sp.EndedAt.Format(time.RFC3339),
				ExitCode:  sp.ExitCode,
			})
		}
		writeJSON(w, out)
	}
}

func makeOutputHandler(store *storage.Store, dataDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// path: /api/output/{sessionID}/{spanID}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/output/"), "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, "usage: /api/output/{sessionID}/{spanID}", http.StatusBadRequest)
			return
		}
		sessionID, spanID := parts[0], parts[1]
		// SplitN(…, 2) guarantees parts[0] (sessionID) never contains "/".
		// parts[1] (spanID) may contain "/" when extra path segments are present.
		if strings.Contains(spanID, "/") {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}

		// Validate the span belongs to the session before serving the file.
		// Uses an indexed single-row lookup to prevent path traversal via
		// crafted IDs without loading all spans into memory.
		ok, err := store.SpanExists(r.Context(), sessionID, spanID)
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "span not found", http.StatusNotFound)
			return
		}

		logPath := storage.OutputPath(dataDir, sessionID, spanID)
		f, err := os.Open(logPath)
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "output file not found", http.StatusNotFound)
				return
			}
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		defer func() { _ = f.Close() }()

		const maxLogBytes = 10 << 20 // 10 MiB
		b, err := io.ReadAll(io.LimitReader(f, maxLogBytes+1))
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		truncated := len(b) > maxLogBytes
		if truncated {
			// Trim to the last complete newline so we never feed a
			// half-written JSON line to the decoder, which would
			// incorrectly increment the corrupt counter.
			if nl := bytes.LastIndexByte(b[:maxLogBytes], '\n'); nl >= 0 {
				b = b[:nl+1]
			} else {
				b = b[:maxLogBytes]
			}
		}

		// Decode JSON Lines and return plain text.
		var sb strings.Builder
		corrupt := 0
		for _, line := range splitLines(b) {
			if len(line) == 0 {
				continue
			}
			var c storage.Chunk
			if err := json.Unmarshal(line, &c); err != nil {
				corrupt++
				continue
			}
			sb.WriteString(c.Data)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if corrupt > 0 {
			w.Header().Set("X-Shtrace-Corrupt-Lines", strconv.Itoa(corrupt))
		}
		if truncated {
			w.Header().Set("X-Shtrace-Truncated", "true")
		}
		_, _ = io.WriteString(w, sb.String())
	}
}

func makeSearchHandler(fts *storage.FTSStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if fts == nil {
			http.Error(w, "search index not available — run 'shtrace reindex' first", http.StatusServiceUnavailable)
			return
		}
		q := r.URL.Query().Get("q")
		if q == "" {
			writeJSON(w, []apiSearchResult{})
			return
		}
		results, err := fts.Search(r.Context(), q, 50)
		if err != nil {
			http.Error(w, "search error", http.StatusInternalServerError)
			return
		}
		out := make([]apiSearchResult, 0, len(results))
		for _, res := range results {
			out = append(out, apiSearchResult{
				SpanID:    res.SpanID,
				SessionID: res.SessionID,
				Snippet:   res.Snippet,
			})
		}
		writeJSON(w, out)
	}
}

func makeUIHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, serveUI)
	}
}

// serveUI is the embedded single-page web UI served at "/".
// It uses only standard JS (no template literals) so the raw string works without escaping.
const serveUI = "<!DOCTYPE html>\n" +
	"<html lang=\"en\">\n" +
	"<head>\n" +
	"<meta charset=\"UTF-8\">\n" +
	"<title>shtrace</title>\n" +
	"<style>\n" +
	"*{box-sizing:border-box;margin:0;padding:0}\n" +
	"body{font-family:system-ui,sans-serif;background:#111;color:#eee;display:flex;height:100vh;overflow:hidden}\n" +
	"#sidebar{width:280px;min-width:180px;background:#1a1a1a;border-right:1px solid #333;display:flex;flex-direction:column;overflow:hidden}\n" +
	"#sidebar h2{padding:12px;font-size:13px;color:#888;text-transform:uppercase;letter-spacing:.08em;border-bottom:1px solid #333}\n" +
	"#sessions{flex:1;overflow-y:auto}\n" +
	".sess{padding:10px 12px;cursor:pointer;border-bottom:1px solid #222;font-size:12px}\n" +
	".sess:hover,.sess.active{background:#252525}\n" +
	".sess .sid{font-family:monospace;color:#7ee8a2;word-break:break-all}\n" +
	".sess .stime{color:#666;margin-top:2px}\n" +
	".sess .stags{color:#888;margin-top:2px;font-size:11px}\n" +
	"#main{flex:1;display:flex;flex-direction:column;overflow:hidden}\n" +
	"#toolbar{padding:10px 12px;border-bottom:1px solid #333;display:flex;gap:8px;align-items:center}\n" +
	"#toolbar input{flex:1;background:#222;border:1px solid #444;border-radius:4px;padding:6px 10px;color:#eee;font-size:13px;outline:none}\n" +
	"#toolbar input:focus{border-color:#666}\n" +
	"#toolbar button{background:#333;border:1px solid #555;color:#ccc;padding:6px 12px;border-radius:4px;cursor:pointer;font-size:13px}\n" +
	"#toolbar button:hover{background:#444}\n" +
	"#content{flex:1;overflow:auto;padding:12px}\n" +
	"#content h3{font-size:12px;color:#888;text-transform:uppercase;letter-spacing:.08em;margin-bottom:8px}\n" +
	".span-card{background:#1a1a1a;border:1px solid #333;border-radius:6px;margin-bottom:8px;overflow:hidden}\n" +
	".span-header{padding:8px 12px;display:flex;align-items:center;gap:8px;cursor:pointer;background:#1e1e1e}\n" +
	".span-header:hover{background:#252525}\n" +
	".span-cmd{font-family:monospace;font-size:13px;color:#7ee8a2}\n" +
	".span-meta{font-size:11px;color:#666;margin-left:auto;white-space:nowrap}\n" +
	".exit-ok{color:#4caf50}.exit-fail{color:#f44336}.exit-unk{color:#888}\n" +
	".span-output{display:none;padding:10px 12px;font-family:monospace;font-size:12px;white-space:pre-wrap;word-break:break-all;color:#ccc;border-top:1px solid #333;max-height:400px;overflow:auto;background:#111}\n" +
	".span-output.open{display:block}\n" +
	".search-result{background:#1a1a1a;border:1px solid #333;border-radius:6px;margin-bottom:8px;padding:10px 12px}\n" +
	".search-result .sr-ids{font-size:11px;color:#666;margin-bottom:4px;font-family:monospace}\n" +
	".search-result .sr-snippet{font-family:monospace;font-size:12px;color:#ccc;white-space:pre-wrap;word-break:break-all}\n" +
	"#loading{color:#666;font-size:13px;padding:20px}\n" +
	"#empty{color:#666;font-size:13px;padding:20px}\n" +
	"</style>\n" +
	"</head>\n" +
	"<body>\n" +
	"<div id=\"sidebar\">\n" +
	"  <h2>Sessions</h2>\n" +
	"  <div id=\"sessions\"><div id=\"loading\">Loading...</div></div>\n" +
	"</div>\n" +
	"<div id=\"main\">\n" +
	"  <div id=\"toolbar\">\n" +
	"    <input id=\"searchbox\" type=\"text\" placeholder=\"Search output (Enter)\">\n" +
	"    <button id=\"searchbtn\">Search</button>\n" +
	"    <button id=\"refreshbtn\" title=\"Refresh sessions\">Refresh</button>\n" +
	"  </div>\n" +
	"  <div id=\"content\"><div id=\"empty\">Select a session or search.</div></div>\n" +
	"</div>\n" +
	"<script>\n" +
	"var activeSessEl=null;\n" +
	"var activeSessionID=null;\n" +
	"\n" +
	"function esc(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/\"/g,'&quot;').replace(/'/g,'&#39;');}\n" +
	"\n" +
	"function durationStr(start,end){\n" +
	"  var ms=new Date(end)-new Date(start);\n" +
	"  if(ms<1000) return ms+'ms';\n" +
	"  if(ms<60000) return (ms/1000).toFixed(1)+'s';\n" +
	"  return Math.floor(ms/60000)+'m'+(Math.floor(ms/1000)%60)+'s';\n" +
	"}\n" +
	"\n" +
	"function loadSessions(){\n" +
	"  fetch('/api/sessions').then(function(resp){\n" +
	"    var capped=resp.headers.get('X-Shtrace-Sessions-Capped')==='true';\n" +
	"    return resp.json().then(function(sessions){return{sessions:sessions,capped:capped};});\n" +
	"  }).then(function(data){\n" +
	"    var sessions=data.sessions,capped=data.capped;\n" +
	"    var el=document.getElementById('sessions');\n" +
	"    if(!sessions.length){el.innerHTML='<div style=\"padding:12px;color:#666;font-size:12px\">No sessions recorded yet.</div>';return;}\n" +
	"    el.innerHTML='';\n" +
	"    if(capped){var cap=document.createElement('div');cap.style='padding:6px 12px;font-size:11px;color:#f0a000;border-bottom:1px solid #333;';cap.textContent='Showing newest 500 sessions.';el.appendChild(cap);}\n" +
	"    sessions.forEach(function(s){\n" +
	"      var d=document.createElement('div');\n" +
	"      d.className='sess';\n" +
	"      var tags=Object.entries(s.tags||{}).map(function(kv){return kv[0]+'='+kv[1];}).join(' ');\n" +
	"      d.dataset.sid=s.id;\n" +
	"      var html='<div class=\"sid\">'+esc(s.id.slice(0,20))+'</div>';\n" +
	"      html+='<div class=\"stime\">'+esc(s.started_at.replace('T',' ').slice(0,16))+'</div>';\n" +
	"      if(tags) html+='<div class=\"stags\">'+esc(tags)+'</div>';\n" +
	"      d.innerHTML=html;\n" +
	"      d.onclick=(function(sid,el){return function(){selectSession(sid,el);};})(s.id,d);\n" +
	"      el.appendChild(d);\n" +
	"    });\n" +
	"  }).catch(function(e){document.getElementById('sessions').innerHTML='<div style=\"padding:12px;color:#f44\">Error: '+esc(String(e))+'</div>';});\n" +
	"}\n" +
	"\n" +
	"function selectSession(id,el){\n" +
	"  if(activeSessEl) activeSessEl.classList.remove('active');\n" +
	"  activeSessEl=el; el.classList.add('active');\n" +
	"  activeSessionID=id;\n" +
	"  document.getElementById('searchbox').value='';\n" +
	"  var content=document.getElementById('content');\n" +
	"  content.innerHTML='<div id=\"loading\">Loading...</div>';\n" +
	"  fetch('/api/sessions/'+encodeURIComponent(id)+'/spans').then(function(r){return r.json();}).then(function(spans){\n" +
	"    if(!spans.length){content.innerHTML='<div id=\"empty\">No spans in this session.</div>';return;}\n" +
	"    content.innerHTML='<h3>Spans ('+spans.length+')</h3>';\n" +
	"    spans.forEach(function(sp){\n" +
	"      var card=document.createElement('div');\n" +
	"      card.className='span-card';\n" +
	"      var exitHtml=sp.exit_code==null?'<span class=\"exit-unk\">?</span>':\n" +
	"        sp.exit_code===0?'<span class=\"exit-ok\">OK</span>':\n" +
	"        '<span class=\"exit-fail\">exit:'+esc(String(sp.exit_code))+'</span>';\n" +
	"      var dur=durationStr(sp.started_at,sp.ended_at);\n" +
	"      var hdrHtml='<div class=\"span-header\">';\n" +
	"      hdrHtml+='<span class=\"span-cmd\">'+esc(sp.argv.join(' '))+'</span>';\n" +
	"      hdrHtml+='<span class=\"span-meta\">'+exitHtml+' '+esc(sp.mode)+' '+esc(dur)+'</span>';\n" +
	"      hdrHtml+='</div><div class=\"span-output\"></div>';\n" +
	"      card.innerHTML=hdrHtml;\n" +
	"      var hdr=card.querySelector('.span-header');\n" +
	"      var out=card.querySelector('.span-output');\n" +
	"      var loaded=false;\n" +
	"      hdr.onclick=(function(sessID,spanID,outEl){\n" +
	"        return function(){\n" +
	"          outEl.classList.toggle('open');\n" +
	"          if(outEl.classList.contains('open')&&!loaded){\n" +
	"            loaded=true;\n" +
	"            outEl.textContent='Loading...';\n" +
	"            fetch('/api/output/'+encodeURIComponent(sessID)+'/'+encodeURIComponent(spanID))\n" +
	"              .then(function(res){return res.text();})\n" +
	"              .then(function(txt){outEl.textContent=txt;})\n" +
	"              .catch(function(e){outEl.textContent='Error: '+e;});\n" +
	"          }\n" +
	"        };\n" +
	"      })(id,sp.id,out);\n" +
	"      content.appendChild(card);\n" +
	"    });\n" +
	"  }).catch(function(e){content.innerHTML='<div id=\"empty\">Error: '+esc(String(e))+'</div>';});\n" +
	"}\n" +
	"\n" +
	"function doSearch(){\n" +
	"  var q=document.getElementById('searchbox').value.trim();\n" +
	"  if(!q)return;\n" +
	"  if(activeSessEl){activeSessEl.classList.remove('active');activeSessEl=null;}\n" +
	"  var content=document.getElementById('content');\n" +
	"  content.innerHTML='<div id=\"loading\">Searching...</div>';\n" +
	"  fetch('/api/search?q='+encodeURIComponent(q)).then(function(r){\n" +
	"    if(r.status===503){content.innerHTML='<div id=\"empty\">Search index not available -- run shtrace reindex first.</div>';return Promise.reject('503');}\n" +
	"    return r.json();\n" +
	"  }).then(function(results){\n" +
	"    if(!results.length){content.innerHTML='<div id=\"empty\">No results for &quot;'+esc(q)+'&quot;.</div>';return;}\n" +
	"    content.innerHTML='<h3>Search results ('+results.length+')</h3>';\n" +
	"    results.forEach(function(res){\n" +
	"      var d=document.createElement('div');\n" +
	"      d.className='search-result';\n" +
	"      var snippet=esc(res.snippet).replace(/\\[([^\\]]*?)\\]/g,'<span style=\"color:#f0c040;font-weight:bold\">$1</span>');\n" +
	"      d.innerHTML='<div class=\"sr-ids\">session '+esc(res.session_id)+' / span '+esc(res.span_id)+'</div><div class=\"sr-snippet\">'+snippet+'</div>';\n" +
	"      d.style.cursor='pointer';\n" +
	"      d.onclick=(function(sessID){\n" +
	"        return function(){\n" +
	"          var sessEls=document.querySelectorAll('.sess');\n" +
	"          for(var i=0;i<sessEls.length;i++){\n" +
	"            if(sessEls[i].dataset.sid===sessID){selectSession(sessID,sessEls[i]);break;}\n" +
	"          }\n" +
	"        };\n" +
	"      })(res.session_id);\n" +
	"      content.appendChild(d);\n" +
	"    });\n" +
	"  }).catch(function(e){if(e!=='503')content.innerHTML='<div id=\"empty\">Error: '+esc(String(e))+'</div>';});\n" +
	"}\n" +
	"\n" +
	"document.getElementById('searchbtn').onclick=doSearch;\n" +
	"document.getElementById('searchbox').addEventListener('keydown',function(e){if(e.key==='Enter')doSearch();});\n" +
	"document.getElementById('refreshbtn').onclick=loadSessions;\n" +
	"\n" +
	"loadSessions();\n" +
	"</script>\n" +
	"</body>\n" +
	"</html>\n"

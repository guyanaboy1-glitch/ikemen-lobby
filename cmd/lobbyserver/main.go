// Ikemen Online Lobby server.
//
// The "phonebook" that every copy of Ikemen Party checks to see who is
// hosting. Sessions live in memory and vanish if their host stops sending
// heartbeats. No accounts, no database.
//
//	lobbyserver -addr :8080            (or set PORT, as cloud hosts do)
//	lobbyserver -trust-proxy           (behind Render/Fly/nginx: read X-Forwarded-For)
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"ikemenparty/lobbyapi"
)

var maxSessionsPerIP = 3

const (
	maxSessions  = 1000
	maxBodyBytes = 16 << 10
	serverName   = "Ikemen Online Lobby"
)

type entry struct {
	id       string
	secret   string
	ip       string
	info     lobbyapi.SessionInfo
	thumb    []byte
	created  time.Time
	lastSeen time.Time
}

type server struct {
	mu         sync.Mutex
	sessions   map[string]*entry
	trustProxy bool
	limiter    *limiter
}

func newServer(trustProxy bool) *server {
	return &server{sessions: map[string]*entry{}, trustProxy: trustProxy, limiter: newLimiter(20, 40)}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *server) clientIP(r *http.Request) string {
	if s.trustProxy {
		if v := r.Header.Get("Fly-Client-IP"); v != "" {
			return strings.TrimSpace(v)
		}
		if v := r.Header.Get("Cf-Connecting-Ip"); v != "" {
			return strings.TrimSpace(v)
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			return strings.TrimSpace(strings.Split(v, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *server) expireLocked(now time.Time) {
	for id, e := range s.sessions {
		if now.Sub(e.lastSeen) > lobbyapi.SessionTTL {
			delete(s.sessions, id)
		}
	}
}

func (s *server) auth(r *http.Request, id string) *entry {
	e := s.sessions[id]
	if e == nil {
		return nil
	}
	got := r.Header.Get("X-Session-Secret")
	if subtle.ConstantTimeCompare([]byte(got), []byte(e.secret)) != 1 {
		return nil
	}
	return e
}

func (s *server) publicList(now time.Time) []lobbyapi.Session {
	out := make([]lobbyapi.Session, 0, len(s.sessions))
	for _, e := range s.sessions {
		out = append(out, lobbyapi.Session{
			ID: e.id, SessionInfo: e.info, HasThumb: len(e.thumb) > 0,
			AgeSeconds: int64(now.Sub(e.created).Seconds()),
		})
	}
	// Live sessions with room first, then most viewers, then newest.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra < rb
		}
		if a.Counts.InLobby != b.Counts.InLobby {
			return a.Counts.InLobby > b.Counts.InLobby
		}
		return a.AgeSeconds < b.AgeSeconds
	})
	for i := range out {
		// Netplay addresses are only handed out through /join.
		out[i].JoinURL = ""
		if out[i].Mode == lobbyapi.ModeStream {
			out[i].NetplayPort, out[i].RollbackPort = 0, 0
		}
	}
	return out
}

func rank(s lobbyapi.Session) int {
	switch {
	case s.Locked:
		return 3
	case s.Status == "full" || s.Status == "in_match":
		return 2
	case s.Status == "live" || s.Status == "open":
		return 0
	}
	return 1
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	mux.HandleFunc("GET /api/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		s.mu.Lock()
		s.expireLocked(now)
		list := s.publicList(now)
		s.mu.Unlock()
		w.Header().Set("Access-Control-Allow-Origin", "*")
		writeJSON(w, 200, lobbyapi.ListResponse{Sessions: list, Server: serverName})
	})

	mux.HandleFunc("POST /api/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		var info lobbyapi.SessionInfo
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&info); err != nil {
			writeErr(w, 400, "bad json")
			return
		}
		if err := info.Validate(); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		ip := s.clientIP(r)
		now := time.Now()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.expireLocked(now)
		if len(s.sessions) >= maxSessions {
			writeErr(w, 503, "lobby is full, try again later")
			return
		}
		n := 0
		for _, e := range s.sessions {
			if e.ip == ip {
				n++
			}
		}
		if n >= maxSessionsPerIP {
			writeErr(w, 429, "too many sessions from your network")
			return
		}
		e := &entry{id: randHex(6), secret: randHex(16), ip: ip, info: info, created: now, lastSeen: now}
		s.sessions[e.id] = e
		log.Printf("+ %s %q (%s) from %s", e.id, info.Name, info.Mode, ip)
		writeJSON(w, 201, lobbyapi.RegisterResponse{ID: e.id, Secret: e.secret, PublicIP: ip})
	})

	mux.HandleFunc("PUT /api/v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		var info lobbyapi.SessionInfo
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&info); err != nil {
			writeErr(w, 400, "bad json")
			return
		}
		if err := info.Validate(); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		e := s.auth(r, r.PathValue("id"))
		if e == nil {
			writeErr(w, 404, "session not found")
			return
		}
		e.info, e.lastSeen = info, time.Now()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("DELETE /api/v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		e := s.auth(r, r.PathValue("id"))
		if e == nil {
			writeErr(w, 404, "session not found")
			return
		}
		delete(s.sessions, e.id)
		log.Printf("- %s %q", e.id, e.info.Name)
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("PUT /api/v1/sessions/{id}/thumb", func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, lobbyapi.MaxThumbBytes))
		if err != nil || len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
			writeErr(w, 400, "thumbnail must be a JPEG under 100 KB")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		e := s.auth(r, r.PathValue("id"))
		if e == nil {
			writeErr(w, 404, "session not found")
			return
		}
		e.thumb = data
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /api/v1/sessions/{id}/thumb.jpg", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		e := s.sessions[r.PathValue("id")]
		var t []byte
		if e != nil {
			t = e.thumb
		}
		s.mu.Unlock()
		if t == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "max-age=5")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write(t)
	})

	mux.HandleFunc("POST /api/v1/sessions/{id}/join", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		e := s.sessions[r.PathValue("id")]
		if e == nil {
			writeErr(w, 404, "that session has ended")
			return
		}
		if e.info.Locked {
			writeErr(w, 403, "that session is closed to new people")
			return
		}
		resp := lobbyapi.JoinResponse{Mode: e.info.Mode}
		if e.info.Mode == lobbyapi.ModeStream {
			resp.JoinURL = e.info.JoinURL
		} else {
			resp.Address = e.ip
			resp.NetplayPort, resp.RollbackPort = e.info.NetplayPort, e.info.RollbackPort
		}
		writeJSON(w, 200, resp)
	})

	mux.HandleFunc("GET /{$}", s.page)
	return s.limiter.wrap(s.clientIP, mux)
}

var pageTmpl = template.Must(template.New("p").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="10"><title>Ikemen Online Lobby</title>
<style>
:root{color-scheme:dark;--bg:#0b0f1a;--p:#131a2a;--l:#26314a;--t:#e6ebf5;--m:#8d99b3;--a:#4cc3ff;--g:#44d39a}
body{margin:0;background:var(--bg);color:var(--t);font:14px/1.45 system-ui,"Segoe UI",sans-serif}
main{max-width:960px;margin:0 auto;padding:20px 16px}
h1{font-size:20px;margin:0 0 4px}h1 b{color:var(--a)} p{color:var(--m);margin:0 0 18px}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(260px,1fr));gap:14px}
.card{background:var(--p);border:1px solid var(--l);border-radius:10px;overflow:hidden;display:flex;flex-direction:column}
.thumb{aspect-ratio:16/9;background:#000 center/cover no-repeat;display:flex;align-items:center;justify-content:center;color:var(--m)}
.body{padding:12px;display:flex;flex-direction:column;gap:6px;flex:1}
.name{font-weight:700;font-size:15px}.meta{color:var(--m);font-size:12.5px}
.tag{font-size:11px;padding:1px 6px;border-radius:4px;background:var(--l);color:var(--m);margin-right:4px}
.tag.s{background:rgba(76,195,255,.15);color:var(--a)}.tag.n{background:rgba(68,211,154,.15);color:var(--g)}
button{margin-top:auto;font:inherit;padding:8px;border-radius:8px;border:0;background:var(--a);color:#04121c;font-weight:600;cursor:pointer}
.empty{color:var(--m);padding:40px 0;text-align:center}
</style></head><body><main>
<h1>Ikemen <b>Online Lobby</b></h1>
<p>{{len .}} live session(s). Stream sessions open in your browser. Netplay sessions join from inside Ikemen (Online Lobby menu).</p>
{{if .}}<div class="grid">{{range .}}
<div class="card">
 <div class="thumb" {{if .HasThumb}}style="background-image:url('/api/v1/sessions/{{.ID}}/thumb.jpg')"{{end}}>{{if not .HasThumb}}no preview{{end}}</div>
 <div class="body">
  <div class="name">{{.Name}}</div>
  <div class="meta">{{if eq .Mode "stream"}}<span class="tag s">STREAM</span>{{else}}<span class="tag n">NETPLAY</span>{{end}}{{.Game}} · host {{.HostName}}</div>
  <div class="meta">👁 {{.Counts.Watching}} watching · 🎮 {{.Counts.Playing}}/{{.Counts.PlayerCapacity}} playing · 👥 {{.Counts.InLobby}} in lobby</div>
  {{if eq .Mode "stream"}}{{if not .Locked}}<button onclick="join('{{.ID}}')">Watch / Join</button>{{end}}{{end}}
 </div>
</div>{{end}}</div>{{else}}<div class="empty">Nobody is hosting right now.</div>{{end}}
</main>
<script>
async function join(id){
  const r = await fetch('/api/v1/sessions/'+id+'/join',{method:'POST'});
  const j = await r.json();
  if (j.join_url) location.href = j.join_url; else alert(j.error || 'Could not join.');
}
</script></body></html>`))

func (s *server) page(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	s.mu.Lock()
	s.expireLocked(now)
	list := s.publicList(now)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = pageTmpl.Execute(w, list)
}

// ---------------------------------------------------------- rate limiting --

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
}

func newLimiter(rate, burst float64) *limiter {
	l := &limiter{rate: rate, burst: burst, buckets: map[string]*bucket{}}
	go func() {
		for range time.Tick(time.Minute) {
			l.mu.Lock()
			for k, b := range l.buckets {
				if time.Since(b.last) > 5*time.Minute {
					delete(l.buckets, k)
				}
			}
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *limiter) wrap(ipOf func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(ipOf(r)) {
			writeErr(w, 429, "slow down")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	addr := flag.String("addr", "", "listen address (default :$PORT or :8080)")
	perIP := flag.Int("max-per-ip", 3, "most sessions one network (IP) may list at once")
	trust := flag.Bool("trust-proxy", os.Getenv("TRUST_PROXY") == "1", "trust X-Forwarded-For (only behind a reverse proxy)")
	flag.Parse()
	if *perIP > 0 {
		maxSessionsPerIP = *perIP
	}
	if *addr == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		*addr = ":" + port
	}
	s := newServer(*trust)
	srv := &http.Server{Addr: *addr, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 20 * time.Second, WriteTimeout: 20 * time.Second}
	log.Printf("%s listening on %s (trust proxy: %v)", serverName, *addr, *trust)
	log.Fatal(srv.ListenAndServe())
}

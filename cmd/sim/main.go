// Command sim is the v2-route portal stand-in. It now hosts the SAME three
// portal pages as the B1 route's WeKnora-portal-proxy (login / knowledge
// bases / chat), but wired the v2 way: a real WeKnora JWT obtained through
// the tenantless bridge, every permission decision made by WeKnora
// (grants engine + per-request RBAC), zero filtering in this service.
//
// Architecture ("B1 skin, v2 core"):
//
//	browser ──login──▶ sim ──bridge(platform key)──▶ WeKnora JWT
//	   │                   │ (cookie session, httponly)
//	   ├── /kb /chat pages ──▶ sim templated UI
//	   └── /api/v1/* ──▶ transparent reverse proxy (+ Authorization from
//	                       cookie when the request has none; SSE passthrough)
//
// Discipline: NO permission logic may appear here (README 纪律1). Grouping
// in pages is presentation of server-provided fields (space_type/my_role).
package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
)

//go:embed templates
var templateFS embed.FS

type config struct {
	Addr        string
	WeKnoraBase string
	FrontendURL string
	PlatformKey string
	DBDSN       string
	TenantHint  string
}

func loadConfig(envFile string) config {
	fileVals := map[string]string{}
	if f, err := os.Open(envFile); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok {
				fileVals[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	get := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		if v := fileVals[key]; v != "" {
			return v
		}
		return def
	}
	return config{
		Addr:        get("SIM_ADDR", ":8082"),
		WeKnoraBase: strings.TrimRight(get("WEKNORA_BASE_URL", ""), "/"),
		FrontendURL: strings.TrimRight(get("WEKNORA_FRONTEND_URL", ""), "/"),
		PlatformKey: get("WEKNORA_PLATFORM_KEY", ""),
		DBDSN:       get("PORTAL_DB_DSN", ""),
		TenantHint:  get("WEKNORA_TENANT_HINT", "1"),
	}
}

// ── session store (cookie id → bridge-issued JWT) ──────────────────────
//
// Session plumbing only — the JWT itself is WeKnora's judgment of who this
// is and what they may do. Expiry tracks the bridge token's 24h lifetime.

type portalSession struct {
	Token       string
	UUMUserID   string
	DisplayName string
	Expires     time.Time
}

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*portalSession
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: map[string]*portalSession{}}
}

func (s *sessionStore) put(id string, sess *portalSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = sess
	for k, v := range s.sessions { // opportunistic sweep
		if v.Expires.Before(time.Now()) {
			delete(s.sessions, k)
		}
	}
}

func (s *sessionStore) get(id string) *portalSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		if sess.Expires.After(time.Now()) {
			return sess
		}
		delete(s.sessions, id)
	}
	return nil
}

const sessionCookie = "sim_portal_session"

type server struct {
	cfg      config
	db       *sql.DB
	tpl      *template.Template
	sessions *sessionStore
	proxy    *httputil.ReverseProxy
}

func main() {
	envFile := flag.String("env", ".env", "env file path")
	flag.Parse()

	cfg := loadConfig(*envFile)
	if cfg.WeKnoraBase == "" || cfg.PlatformKey == "" || cfg.DBDSN == "" {
		log.Fatal("missing config: WEKNORA_BASE_URL / WEKNORA_PLATFORM_KEY / PORTAL_DB_DSN (see .env.example)")
	}
	db, err := sql.Open("pgx", cfg.DBDSN)
	if err != nil {
		log.Fatalf("portal db: %v", err)
	}
	if err := db.Ping(); err != nil {
		log.Fatalf("portal db ping: %v", err)
	}

	target, _ := url.Parse(cfg.WeKnoraBase)
	proxy := httputil.NewSingleHostReverseProxy(target)
	baseDirector := proxy.Director
	sessions := newSessionStore()
	proxy.Director = func(r *http.Request) {
		baseDirector(r)
		// Session plumbing (not permission logic): attach the bridge-issued
		// JWT when the caller didn't bring its own Authorization (the admin
		// page logs in separately and does).
		if r.Header.Get("Authorization") == "" {
			if c, err := r.Cookie(sessionCookie); err == nil {
				if sess := sessions.get(c.Value); sess != nil {
					r.Header.Set("Authorization", "Bearer "+sess.Token)
				}
			}
		}
	}
	proxy.FlushInterval = -1 // SSE: flush every chunk immediately

	s := &server{
		cfg: cfg, db: db, sessions: sessions, proxy: proxy,
		tpl: template.Must(template.ParseFS(templateFS, "templates/*.html")),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})

	// portal pages
	mux.HandleFunc("GET /{$}", s.loginPage)
	mux.HandleFunc("POST /sso/authorize", s.ssoAuthorize) // portal session → /kb
	mux.HandleFunc("GET /sso/native", s.ssoNative)        // bridge → #bridge_result → native frontend
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /kb", s.requireSession(s.kbPage))
	mux.HandleFunc("GET /kb/{id}", s.requireSession(s.kbDetailPage))
	mux.HandleFunc("GET /chat", s.requireSession(s.chatPage))
	mux.HandleFunc("GET /admin", s.adminPage)

	// transparent passthrough to WeKnora (Authorization injected above)
	mux.Handle("/api/", s.proxy)

	log.Printf("portal-sim listening on %s (weknora=%s) — B1 skin, v2 core, ZERO permission logic", cfg.Addr, cfg.WeKnoraBase)
	srv := &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// ── session helpers ──────────────────────────────────────────────────

func (s *server) requireSession(next func(http.ResponseWriter, *http.Request, *portalSession)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err == nil {
			if sess := s.sessions.get(c.Value); sess != nil {
				next(w, r, sess)
				return
			}
		}
		http.Redirect(w, r, "/?error="+url.QueryEscape("请先登录"), http.StatusFound)
	}
}

func (s *server) startSession(w http.ResponseWriter, uum, token string) {
	var name string
	_ = s.db.QueryRowContext(context.Background(),
		`SELECT display_name FROM employees WHERE uum_user_id = $1`, uum).Scan(&name)
	id := fmt.Sprintf("s%d", time.Now().UnixNano())
	s.sessions.put(id, &portalSession{
		Token: token, UUMUserID: uum, DisplayName: name,
		Expires: time.Now().Add(23 * time.Hour),
	})
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int((23 * time.Hour).Seconds()),
	})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.put(c.Value, &portalSession{Token: "", Expires: time.Now().Add(-time.Second)})
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusFound)
}

// ── auth pages ───────────────────────────────────────────────────────

func (s *server) render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name+".html", data); err != nil {
		log.Printf("template error: %v", err)
	}
}

func (s *server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login", map[string]any{
		"Title": "统一认证（v2 门户）", "Error": r.URL.Query().Get("error"),
	})
}

// ssoAuthorize validates the employee (same test accounts as B1), exchanges
// the user id for a real WeKnora JWT via the tenantless bridge, and opens a
// portal session. WeKnora did everything: personal-space bootstrap, grant
// materialization, token minting.
func (s *server) ssoAuthorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	uum := strings.TrimSpace(r.FormValue("uum_user_id"))
	password := r.FormValue("password")
	deny := func(reason string) {
		http.Redirect(w, r, "/?error="+url.QueryEscape(reason), http.StatusFound)
	}
	var hash string
	var isActive bool
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT password_hash, is_active FROM employees WHERE uum_user_id = $1`, uum,
	).Scan(&hash, &isActive); err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		deny("工号或密码错误")
		return
	}
	if !isActive {
		deny("账号已禁用")
		return
	}
	token, _, err := s.bridge(r.Context(), uum)
	if err != nil {
		deny("WeKnora 换票失败：" + err.Error())
		return
	}
	s.startSession(w, uum, token)
	http.Redirect(w, r, "/kb", http.StatusFound)
}

// ssoNative keeps the earlier demo path: bridge + #bridge_result redirect
// into the native WeKnora frontend (decision 022 contract).
func (s *server) ssoNative(w http.ResponseWriter, r *http.Request) {
	uum := strings.TrimSpace(r.URL.Query().Get("uum_user_id"))
	if uum == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	token, refresh, err := s.bridge(r.Context(), uum)
	if err != nil {
		http.Redirect(w, r, "/?error="+url.QueryEscape("换票失败："+err.Error()), http.StatusFound)
		return
	}
	payload, _ := json.Marshal(map[string]string{"token": token, "refresh_token": refresh})
	frag := base64.RawURLEncoding.EncodeToString(payload)
	http.Redirect(w, r, s.cfg.FrontendURL+"/#bridge_result="+frag, http.StatusFound)
}

// bridge calls POST /identity/bridge WITHOUT tenant_id: WeKnora resolves the
// login space (personal bootstrap + active grants) per the v2 contract.
func (s *server) bridge(ctx context.Context, uumUserID string) (token, refreshToken string, err error) {
	body, _ := json.Marshal(map[string]any{"uum_user_id": uumUserID})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cfg.WeKnoraBase+"/api/v1/identity/bridge", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", s.cfg.PlatformKey)
	// header only satisfies the platform-key gate (must be a REAL tenant);
	// bridge logic itself is driven by the request body
	req.Header.Set("X-Tenant-ID", s.cfg.TenantHint)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Success bool `json:"success"`
		Data    struct {
			Token        string `json:"token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if jerr := json.Unmarshal(raw, &env); jerr != nil || !env.Success {
		msg := "unexpected response"
		if env.Error != nil {
			msg = env.Error.Message
		}
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	return env.Data.Token, env.Data.RefreshToken, nil
}

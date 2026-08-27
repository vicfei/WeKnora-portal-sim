// Command sim is the v2-route portal stand-in: it plays ONLY the parts of
// the future Java portal that face WeKnora — SSO login entry, bridge
// token exchange, and the #bridge_result redirect (decision 022). It
// deliberately contains ZERO permission logic: every authorization
// decision happens inside WeKnora (grants engine + per-request RBAC).
// This is the architectural contrast to the B1 route's WeKnora-portal-proxy.
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
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
)

//go:embed templates
var templateFS embed.FS

type config struct {
	Addr         string
	WeKnoraBase  string // http://localhost:8080
	FrontendURL  string // http://localhost:5173 (redirect target)
	PlatformKey  string
	DBDSN        string // read-only access to portal_proxy.employees
	// TenantHint fills the platform-key gate's mandatory X-Tenant-ID header.
	// The bridge endpoint ignores it logically (body-driven), but the
	// middleware rejects values that don't resolve to a REAL tenant
	// (auth.go attachAPIKeyAuthContext). Any existing tenant id works.
	TenantHint string
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

type server struct {
	cfg  config
	db   *sql.DB
	tpl  *template.Template
	proxy http.Handler
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

	s := &server{
		cfg:   cfg,
		db:    db,
		tpl:   template.Must(template.ParseFS(templateFS, "templates/*.html")),
		proxy: proxy,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /{$}", s.loginPage)
	mux.HandleFunc("POST /sso/authorize", s.ssoAuthorize)
	mux.HandleFunc("GET /admin", s.adminPage)
	// transparent passthrough — zero filtering by design (see README 纪律1)
	mux.Handle("/api/", s.proxy)
	mux.Handle("/auth/", s.proxy)

	log.Printf("portal-sim listening on %s (weknora=%s frontend=%s) — ZERO permission logic by design",
		cfg.Addr, cfg.WeKnoraBase, cfg.FrontendURL)
	srv := &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func (s *server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login", map[string]any{
		"Title": "统一认证（v2 门户替身）", "Error": r.URL.Query().Get("error"),
	})
}

func (s *server) render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name+".html", data); err != nil {
		log.Printf("template error: %v", err)
	}
}

// ssoAuthorize: validate employee credentials (portal_proxy.employees,
// read-only — the same test accounts the B1 route uses), then exchange the
// user id for a real WeKnora JWT via the tenantless bridge. WeKnora does
// the rest: personal-space bootstrap, grant materialization, token minting.
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
	if err := s.db.QueryRowContext(
		r.Context(),
		`SELECT password_hash, is_active FROM employees WHERE uum_user_id = $1`, uum,
	).Scan(&hash, &isActive); err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		deny("工号或密码错误")
		return
	}
	if !isActive {
		deny("账号已禁用")
		return
	}

	token, refresh, err := s.bridge(r.Context(), uum)
	if err != nil {
		deny("WeKnora 换票失败：" + err.Error())
		return
	}
	payload, _ := json.Marshal(map[string]string{"token": token, "refresh_token": refresh})
	// decision 022 / 外部接口.md §19.1: hash fragment, never a query param
	frag := base64.RawURLEncoding.EncodeToString(payload)
	http.Redirect(w, r, s.cfg.FrontendURL+"/#bridge_result="+frag, http.StatusFound)
}

// bridge calls POST /identity/bridge WITHOUT tenant_id: WeKnora resolves
// the login space (personal bootstrap + active grants) per the v2 contract.
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

func (s *server) adminPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "admin", map[string]any{"Title": "v2 管理台（经 portal-sim 反代）"})
}

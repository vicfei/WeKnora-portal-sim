package main

import "net/http"

// Page handlers are thin shells: every page loads its data in the browser
// via the transparent /api proxy (Authorization injected from the portal
// session cookie), so the server stays free of any permission logic.

func (s *server) kbPage(w http.ResponseWriter, r *http.Request, sess *portalSession) {
	s.render(w, "kb", map[string]any{"Title": "知识库门户", "UserName": sess.DisplayName})
}

func (s *server) kbDetailPage(w http.ResponseWriter, r *http.Request, sess *portalSession) {
	s.render(w, "kb_detail", map[string]any{
		"Title": "知识库详情", "UserName": sess.DisplayName, "KbID": r.PathValue("id"),
	})
}

func (s *server) chatPage(w http.ResponseWriter, r *http.Request, sess *portalSession) {
	s.render(w, "chat", map[string]any{"Title": "知识问答", "UserName": sess.DisplayName})
}

func (s *server) adminPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "admin", map[string]any{"Title": "v2 管理台"})
}

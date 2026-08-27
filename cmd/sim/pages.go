package main

import "net/http"

// Page handlers are thin shells: every page loads its data in the browser
// via the transparent /api proxy (Authorization injected from the portal
// session cookie), so the server stays free of any permission logic.

func (s *server) kbPage(w http.ResponseWriter, r *http.Request, sess *portalSession) {
	s.render(w, "kb", map[string]any{
		"Title": "知识库", "Nav": "kb", "UserName": sess.DisplayName, "Avatar": sess.avatar(),
	})
}

func (s *server) kbDetailPage(w http.ResponseWriter, r *http.Request, sess *portalSession) {
	s.render(w, "kb_detail", map[string]any{
		"Title": "知识库详情", "Nav": "kb", "UserName": sess.DisplayName, "Avatar": sess.avatar(),
		"KbID": r.PathValue("id"),
	})
}

func (s *server) chatPage(w http.ResponseWriter, r *http.Request, sess *portalSession) {
	s.render(w, "chat", map[string]any{
		"Title": "智能问答", "Nav": "chat", "UserName": sess.DisplayName, "Avatar": sess.avatar(),
	})
}

func (s *server) adminPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "admin", map[string]any{"Title": "v2 管理台"})
}

// avatar returns the first rune of the display name for the chip.
func (s *portalSession) avatar() string {
	if s.DisplayName == "" {
		return "?"
	}
	for _, r := range s.DisplayName {
		return string(r)
	}
	return "?"
}

package console

import (
	"net/http"
	"strings"

	"freehold/contract/avatar"
)

// agentAvatar serves the deterministic identicon a managed agent's kind-0
// profile points at. The path is /avatars/<64-hex-pubkey>.png; the image is
// derived from the key, so no per-agent file or upload is needed.
func (s *Server) agentAvatar(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/avatars/")
	if !strings.HasSuffix(name, ".png") {
		http.NotFound(w, r)
		return
	}
	pubkey := strings.TrimSuffix(name, ".png")
	png, err := avatar.PNG(pubkey)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

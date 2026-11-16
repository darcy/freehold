package console

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentAvatarPublicRoute(t *testing.T) {
	s := &Server{}
	pk := strings.Repeat("ab", 32)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/avatars/"+pk+".png", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q, want image/png", ct)
	}
	if _, err := png.Decode(bytes.NewReader(rec.Body.Bytes())); err != nil {
		t.Fatalf("body is not a PNG: %v", err)
	}
}

func TestAgentAvatarRejectsBadPath(t *testing.T) {
	s := &Server{}
	for _, path := range []string{"/avatars/nothex.png", "/avatars/" + strings.Repeat("ab", 32), "/avatars/" + strings.Repeat("ab", 32) + ".jpg"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

package server

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTargetFromPath(t *testing.T) {
	got, ok := targetFromPath("/https:/x.com/a/b", "id=1")
	if !ok || got != "https://x.com/a/b?id=1" {
		t.Fatalf("got %q %v", got, ok)
	}
	if _, ok := targetFromPath("/foo", ""); ok {
		t.Fatal("expected no match")
	}
	if got, ok := targetFromPath("/https://x.com/a%20b", ""); !ok || got != "https://x.com/a b" {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestShareRedirect(t *testing.T) {
	s, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	req := httptest.NewRequest(http.MethodGet, "/share?text=Read%20this%20https://x.com/a", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/read?url=https%3A%2F%2Fx.com%2Fa" {
		t.Fatalf("location = %q", loc)
	}
}

func TestShareWithoutLink(t *testing.T) {
	s, _ := New(nil, nil, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/share?text=nothing", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestScriptletJS(t *testing.T) {
	s, _ := New(nil, nil, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/scriptlets.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Fatalf("content type = %q", ct)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("uBlock Origin")) {
		t.Fatal("missing uBO header")
	}
}

func TestIcons(t *testing.T) {
	s, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/icon-512.png", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	img, err := png.Decode(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 512 || b.Dy() != 512 {
		t.Fatalf("size = %v", b)
	}
}

func TestIndex(t *testing.T) {
	s, _ := New(nil, nil, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Update rules now") {
		t.Fatal("index missing rules section")
	}
	if !strings.Contains(rec.Body.String(), "javascript:location.href=") {
		t.Fatal("index missing bookmarklet")
	}
}

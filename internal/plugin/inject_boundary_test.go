package plugin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTMLInjectorLeavesFileDataPlaneUnwrapped(t *testing.T) {
	manager := &Manager{injects: []*injectEntry{{short: "fixture", head: `<meta name="fixture" content="enabled">`}}}
	handler := manager.htmlInjectHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, wrapped := w.(*htmlInjectWriter); wrapped {
			t.Error("file data plane must receive the original writer")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("ordinary file contents"))
	}))
	for _, path := range []string{
		"/api/admin/client/fixture/file/download",
		"/api/preview/client/fixture/file/download",
		"/api/clients/transfer/fixture",
		"/komari.file.v1.FileService/ListDirectory",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Body.String() != "ordinary file contents" {
			t.Fatal("file bytes changed")
		}
	}
}

func TestHTMLInjectorStillAddsOrdinaryPageMetadata(t *testing.T) {
	manager := &Manager{injects: []*injectEntry{{short: "fixture", head: `<meta name="fixture" content="enabled">`}}}
	handler := manager.htmlInjectHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><head></head><body>Application</body></html>"))
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if !strings.Contains(recorder.Body.String(), `<meta name="fixture" content="enabled"></head>`) {
		t.Fatal("normal page metadata injection missing")
	}
	if recorder.Header().Get("Content-Security-Policy") != "" {
		t.Fatal("injector changed the application's content policy")
	}
}

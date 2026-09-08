package public

import (
	"strings"
	"testing"
)

func TestHTTPErrorNoticeGoesIntoHead(t *testing.T) {
	document := "<html><head><title>x</title></head><body><div id=\"root\"></div></body></html>"
	got := injectHTTPErrorNotice(document)
	head := strings.Index(got, "</head>")
	marker := strings.Index(got, httpErrorNoticeMarker)
	if marker < 0 {
		t.Fatal("notice was not injected")
	}
	// The hooks have to be installed before any bundle issues a request, so
	// the script belongs in the head rather than after the application.
	if marker > head {
		t.Fatalf("notice landed after </head> (marker %d, head %d)", marker, head)
	}
}

func TestHTTPErrorNoticeIsInjectedOnce(t *testing.T) {
	document := "<html><head></head><body></body></html>"
	once := injectHTTPErrorNotice(document)
	twice := injectHTTPErrorNotice(once)
	if once != twice {
		t.Fatal("notice was injected a second time")
	}
	if count := strings.Count(twice, httpErrorNoticeMarker); count != 1 {
		t.Fatalf("marker appears %d times", count)
	}
}

func TestHTTPErrorNoticeMarkersDoNotOverlap(t *testing.T) {
	// The idempotence check counts the script marker, so the DOM host's
	// attribute must not contain it as a substring - otherwise a single
	// injection reads as two and the guard cannot be trusted.
	if strings.Contains(httpErrorNoticeHostMarker, httpErrorNoticeMarker) {
		t.Fatalf("host marker %q contains the script marker %q",
			httpErrorNoticeHostMarker, httpErrorNoticeMarker)
	}
	if strings.Contains(httpErrorNoticeMarker, httpErrorNoticeHostMarker) {
		t.Fatalf("script marker %q contains the host marker %q",
			httpErrorNoticeMarker, httpErrorNoticeHostMarker)
	}
	if count := strings.Count(injectHTTPErrorNotice("<html><head></head><body></body></html>"), httpErrorNoticeMarker); count != 1 {
		t.Fatalf("one injection counted as %d", count)
	}
}

func TestHTTPErrorNoticeFallsBackWithoutHead(t *testing.T) {
	// A theme is free to ship an unusual document. The notice must still land
	// somewhere rather than being silently dropped.
	if got := injectHTTPErrorNotice("<body><div id=\"root\"></div></body>"); !strings.Contains(got, httpErrorNoticeMarker) {
		t.Fatal("notice was dropped for a document without a head")
	}
	if got := injectHTTPErrorNotice("<div id=\"root\"></div>"); !strings.Contains(got, httpErrorNoticeMarker) {
		t.Fatal("notice was dropped for a fragment")
	}
}

func TestHTTPErrorNoticePreservesDocumentContent(t *testing.T) {
	document := "<html><head><title>Panel</title></head><body><div id=\"root\">keep</div></body></html>"
	got := injectHTTPErrorNotice(document)
	for _, fragment := range []string{"<title>Panel</title>", "<div id=\"root\">keep</div>", "</body></html>"} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("injection lost %q", fragment)
		}
	}
}

func TestHTTPErrorNoticeHandlesUppercaseHead(t *testing.T) {
	got := injectHTTPErrorNotice("<HTML><HEAD></HEAD><BODY></BODY></HTML>")
	if !strings.Contains(got, httpErrorNoticeMarker) {
		t.Fatal("notice was dropped for an uppercase document")
	}
	if strings.Index(got, httpErrorNoticeMarker) > strings.Index(strings.ToLower(got), "</head>") {
		t.Fatal("notice landed after an uppercase </HEAD>")
	}
}

func TestHTTPErrorNoticeExposesThemeEntryPoint(t *testing.T) {
	// A replacement theme has no access to this repository's modules, so the
	// documented global is the only way it can report its own failures.
	if !strings.Contains(httpErrorNoticeScript, "notifyHttpError") {
		t.Fatal("the notice does not expose window.__komari.notifyHttpError")
	}
}

func TestHTTPErrorNoticeHooksEveryTransport(t *testing.T) {
	// Connect RPC and plain REST calls both go through fetch; uploads go
	// through XMLHttpRequest. Missing either leaves a whole class of rejection
	// invisible in every theme.
	for _, hook := range []string{"window.fetch", "XMLHttpRequest"} {
		if !strings.Contains(httpErrorNoticeScript, hook) {
			t.Fatalf("the notice does not hook %s", hook)
		}
	}
}

func TestHTTPErrorNoticeIsStyleIsolated(t *testing.T) {
	// A theme's stylesheet must not be able to restyle or hide the notice, and
	// the notice must not leak styles into the theme.
	if !strings.Contains(httpErrorNoticeScript, "attachShadow") {
		t.Fatal("the notice does not use a shadow root")
	}
	// A theme's palette is unknown, so both schemes are painted explicitly
	// rather than inherited.
	if !strings.Contains(httpErrorNoticeScript, "prefers-color-scheme") {
		t.Fatal("the notice does not paint a dark scheme")
	}
}

func TestHTTPErrorNoticeScriptTagIsWellFormed(t *testing.T) {
	// The script is assembled by concatenation around the marker constant; a
	// mistake there would emit a broken tag into every document.
	if !strings.HasPrefix(httpErrorNoticeScript, "<script "+httpErrorNoticeMarker+">") {
		t.Fatalf("script prefix is malformed: %.80q", httpErrorNoticeScript)
	}
	if !strings.HasSuffix(strings.TrimSpace(httpErrorNoticeScript), "</script>") {
		t.Fatal("script is not closed")
	}
	// A stray closing tag inside the body would truncate the script.
	if strings.Count(httpErrorNoticeScript, "</script>") != 1 {
		t.Fatal("script body contains an extra closing tag")
	}
	if strings.Count(httpErrorNoticeScript, "<script") != 1 {
		t.Fatal("script body contains a nested opening tag")
	}
}

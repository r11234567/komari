package plugin

import (
	"net/http"
	"strings"
)

// injectEntry is one plugin's registered public theme HTML fragments.
type injectEntry struct {
	short string
	head  string
	body  string
}

// InjectPublicThemeHTML inserts registered HTML into a rendered document from
// the dedicated public theme route. The caller verifies the public origin.
func InjectPublicThemeHTML(r *http.Request, document []byte) []byte {
	return global.injectPublicThemeHTML(r, document)
}

func (m *Manager) injectPublicThemeHTML(r *http.Request, document []byte) []byte {
	if isUpgradeRequest(r) || protectedWorkspacePath(r.URL.Path) || len(document) > maxHTMLInjectBufferBytes {
		return document
	}
	return injectHTMLContent(document, m.injectsSnapshot())
}

// injectHTMLContent inserts the plugin head/body fragments into an HTML
// document: head before </head>, body before </body> with fallbacks to
// </html> and the document end. Tag matching is case-insensitive.
func injectHTMLContent(body []byte, fragments []*injectEntry) []byte {
	if len(fragments) == 0 {
		return body
	}
	var head, tail strings.Builder
	for _, fragment := range fragments {
		head.WriteString(fragment.head)
		tail.WriteString(fragment.body)
	}
	if head.Len() == 0 && tail.Len() == 0 {
		return body
	}
	text := string(body)
	headHTML, tailHTML := head.String(), tail.String()
	if headHTML != "" {
		if idx := indexFold(text, "</head>"); idx >= 0 {
			text = text[:idx] + headHTML + text[idx:]
		} else {
			text = headHTML + text
		}
	}
	if tailHTML != "" {
		switch {
		case indexFold(text, "</body>") >= 0:
			text = text[:indexFold(text, "</body>")] + tailHTML + text[indexFold(text, "</body>"):]
		case indexFold(text, "</html>") >= 0:
			text = text[:indexFold(text, "</html>")] + tailHTML + text[indexFold(text, "</html>"):]
		default:
			text += tailHTML
		}
	}
	return []byte(text)
}

func indexFold(text, needle string) int {
	return strings.Index(strings.ToLower(text), needle)
}

package public

import (
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/internal/datadir"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/utils/fsmove"
	websecurity "github.com/komari-monitor/komari/web/security"
)

var legacyDefaultFaviconSHA256 = [32]byte{
	0xbd, 0x28, 0xc1, 0xec, 0x58, 0x09, 0x26, 0xbc,
	0x3f, 0x5e, 0x9c, 0xb2, 0x72, 0x19, 0x76, 0xd9,
	0xeb, 0xad, 0xee, 0xfe, 0x22, 0x1b, 0xae, 0x77,
	0x29, 0x4b, 0xf3, 0x38, 0x85, 0xc7, 0x1a, 0x69,
}

//go:embed systemUI bundledThemes
var PublicFS embed.FS

// 常量定义
const (
	DataDir                = "./data"
	ThemesDir              = "theme"
	FaviconFile            = "favicon.ico"
	DefaultTheme           = "default"
	LegacyLiteDefaultTheme = "nezha"
	LanguageCookieName     = "language"

	// 主题内部结构定义
	DistDir   = "dist"       // 静态资源存放目录
	IndexFile = "index.html" // 相对于 DistDir
)

const (
	defaultThemeBundle      = "bundledThemes/default"
	themeBundleMigrationKey = "theme_bundle_migration_v1"
	defaultSiteDescription  = "A simple server monitor tool."
)

func renderPublicDescription(document string, value any) string {
	description, ok := value.(string)
	if !ok {
		return document
	}
	content := ` content="` + html.EscapeString(description) + `"`
	for _, name := range []string{`name="description"`, `property="og:description"`, `name="twitter:description"`} {
		prefix := "<meta " + name
		document = strings.ReplaceAll(document, prefix+` content="`+defaultSiteDescription+`"`, prefix+content)
	}
	return document
}

// Increment whenever the embedded official default theme must replace an
// already-installed bundle. Version 12 ships the billing-cycle display and
// decimal GB formatting. Version 14 keeps the default theme login inline and
// adds the dedicated third-party password-to-factor verification page;
// Version 15 removes raw-only history and unsupported scalar aggregations;
// existing installations must receive the matching frontend bundle.
const currentThemeBundleMigration = 15

const themeChangeReloadScript = `<script>(()=>{window.addEventListener("storage",(event)=>{if(event.key==="komari-active-theme-changed"){window.location.reload();}});})();</script>`

const documentTitleSyncMarker = "data-komari-title-sync"

var (
	documentTitlePattern = regexp.MustCompile(`(?is)<title(?:\s[^>]*)?>.*?</title\s*>`)
	headOpenPattern      = regexp.MustCompile(`(?i)<head(?:\s[^>]*)?>`)
	headClosePattern     = regexp.MustCompile(`(?i)</head\s*>`)
	bodyClosePattern     = regexp.MustCompile(`(?i)</body\s*>`)
)

func injectThemeChangeReload(html string) string {
	if strings.Contains(html, themeChangeReloadScript) {
		return html
	}
	if strings.Contains(html, "</body>") {
		return strings.Replace(html, "</body>", themeChangeReloadScript+"</body>", 1)
	}
	return html + themeChangeReloadScript
}

func injectCustomHTML(htmlStr, customHead, customBody string) string {
	if location := bodyClosePattern.FindStringIndex(htmlStr); location != nil {
		htmlStr = htmlStr[:location[0]] + customBody + htmlStr[location[0]:]
	} else {
		htmlStr += customBody
	}
	if location := headClosePattern.FindStringIndex(htmlStr); location != nil {
		return htmlStr[:location[0]] + customHead + htmlStr[location[0]:]
	}
	return customHead + htmlStr
}

func renderPublicDocumentTitle(htmlStr, title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Komari Monitor"
	}

	titleTag := "<title>" + html.EscapeString(title) + "</title>"
	if location := documentTitlePattern.FindStringIndex(htmlStr); location != nil {
		htmlStr = htmlStr[:location[0]] + titleTag + htmlStr[location[1]:]
	} else if location := headClosePattern.FindStringIndex(htmlStr); location != nil {
		htmlStr = htmlStr[:location[0]] + titleTag + htmlStr[location[0]:]
	} else {
		htmlStr = titleTag + htmlStr
	}

	if strings.Contains(htmlStr, documentTitleSyncMarker) {
		return htmlStr
	}

	encodedTitle, _ := json.Marshal(title)
	script := `<script ` + documentTitleSyncMarker + `>(()=>{const expectedTitle=` + string(encodedTitle) + `;const syncTitle=()=>{if(document.title!==expectedTitle){document.title=expectedTitle;}};syncTitle();if(document.head){new MutationObserver(syncTitle).observe(document.head,{childList:true,subtree:true,characterData:true});}})();</script>`
	if location := bodyClosePattern.FindStringIndex(htmlStr); location != nil {
		return htmlStr[:location[0]] + script + htmlStr[location[0]:]
	}
	return htmlStr + script
}

func init() {
	_ = os.MkdirAll(datadir.Resolve("./data/theme"), 0755)
	if _, err := removeFaviconIfHashMatches(
		filepath.Join(datadir.Resolve(DataDir), FaviconFile),
		legacyDefaultFaviconSHA256,
	); err != nil {
		log.Printf("Failed to migrate legacy default favicon: %v", err)
	}
}

func removeFaviconIfHashMatches(filePath string, expectedHash [32]byte) (bool, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	if sha256.Sum256(data) != expectedHash {
		return false, nil
	}
	if err := os.Remove(filePath); err != nil {
		return false, err
	}
	return true, nil
}

func normalizeHTMLLanguage(language string) string {
	language = strings.TrimSpace(strings.ReplaceAll(language, "_", "-"))
	if len(language) < 2 || len(language) > 32 {
		return ""
	}

	for _, r := range language {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return ""
	}

	return language
}

func replaceHTMLLanguage(htmlStr, language string) string {
	language = normalizeHTMLLanguage(language)
	if language == "" {
		return htmlStr
	}

	replacements := []struct {
		old string
		new string
	}{
		{`<html lang="en">`, `<html lang="` + language + `">`},
		{`<html lang='en'>`, `<html lang='` + language + `'>`},
		{`<html>`, `<html lang="` + language + `">`},
	}

	for _, replacement := range replacements {
		if strings.Contains(htmlStr, replacement.old) {
			return strings.Replace(htmlStr, replacement.old, replacement.new, 1)
		}
	}

	return htmlStr
}

func embeddedFileContent(root, relativePath string) ([]byte, string, bool) {
	cleanPath := path.Clean(strings.TrimPrefix(filepath.ToSlash(relativePath), "/"))
	if cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
		return nil, "", false
	}
	content, err := fs.ReadFile(PublicFS, path.Join(root, cleanPath))
	if err != nil {
		return nil, "", false
	}
	return content, contentTypeForPath(cleanPath), true
}

func contentTypeForPath(filePath string) string {
	if strings.EqualFold(filepath.Ext(filePath), ".ico") {
		return "image/x-icon"
	}
	return mime.TypeByExtension(filepath.Ext(filePath))
}

func validThemeID(themeID string) bool {
	return themeID != "" && !strings.Contains(themeID, "..") &&
		!strings.ContainsAny(themeID, `/\\`)
}

func localThemeFileContent(themeID, relativePath string) ([]byte, string, bool) {
	if !validThemeID(themeID) {
		return nil, "", false
	}
	themeBasePath := filepath.Join(datadir.Resolve(DataDir), ThemesDir, themeID)
	themeRoot, err := os.OpenRoot(themeBasePath)
	if err != nil {
		return nil, "", false
	}
	defer themeRoot.Close()
	cleanPath := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(relativePath, "/")))
	file, err := themeRoot.Open(cleanPath)
	if err != nil {
		return nil, "", false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		return nil, "", false
	}
	content, err := io.ReadAll(file)
	if err == nil {
		return content, contentTypeForPath(cleanPath), true
	}
	return nil, "", false
}

func IsLocalThemeUsable(themeID string) bool {
	if !validThemeID(themeID) {
		return false
	}
	base := filepath.Join(datadir.Resolve(DataDir), ThemesDir, themeID)
	for _, relativePath := range []string{"komari-theme.json", filepath.Join(DistDir, IndexFile)} {
		info, err := os.Stat(filepath.Join(base, relativePath))
		if err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

func installEmbeddedTheme(root, themeID string) error {
	return installEmbeddedThemeWithReplace(root, themeID, false)
}

func installEmbeddedThemeWithReplace(root, themeID string, replace bool) error {
	if !replace && IsLocalThemeUsable(themeID) {
		return nil
	}
	themesDir := filepath.Join(datadir.Resolve(DataDir), ThemesDir)
	if err := os.MkdirAll(themesDir, 0o755); err != nil {
		return err
	}
	stageDir, err := os.MkdirTemp(themesDir, "."+themeID+"-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stageDir)
	if err := fs.WalkDir(PublicFS, root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative := strings.TrimPrefix(name, root+"/")
		if relative == name || relative == "" {
			return nil
		}
		target := filepath.Join(stageDir, filepath.FromSlash(relative))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := fs.ReadFile(PublicFS, name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	}); err != nil {
		return err
	}

	finalDir := filepath.Join(themesDir, themeID)
	backupDir := finalDir + ".previous"
	_ = os.RemoveAll(backupDir)
	if _, err := os.Stat(finalDir); err == nil {
		if err := fsmove.Directory(finalDir, backupDir); err != nil {
			return err
		}
	}
	if err := fsmove.Directory(stageDir, finalDir); err != nil {
		_ = fsmove.Directory(backupDir, finalDir)
		return err
	}
	_ = os.RemoveAll(backupDir)
	return nil
}

func localThemeFallback() string {
	if IsLocalThemeUsable(DefaultTheme) {
		return DefaultTheme
	}
	entries, err := os.ReadDir(filepath.Join(datadir.Resolve(DataDir), ThemesDir))
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		themeID := entry.Name()
		if themeID == LegacyLiteDefaultTheme || strings.HasPrefix(themeID, ".") {
			continue
		}
		if entry.IsDir() && IsLocalThemeUsable(themeID) {
			return themeID
		}
	}
	return ""
}

// EnsureBundledThemes performs the one-time transition from the former
// inseparable default frontend to managed public themes.
func EnsureBundledThemes() error {
	migrated, err := config.GetAs[int](themeBundleMigrationKey, 0)
	if err != nil {
		return err
	}
	currentTheme, err := config.GetAs[string](config.ThemeKey, DefaultTheme)
	if err != nil {
		return err
	}
	if currentTheme == "" {
		currentTheme = DefaultTheme
	}
	if migrated < currentThemeBundleMigration {
		replaceDefault := migrated > 0 && IsLocalThemeUsable(DefaultTheme)
		if err := installEmbeddedThemeWithReplace(defaultThemeBundle, DefaultTheme, replaceDefault); err != nil {
			return fmt.Errorf("install bundled default theme: %w", err)
		}
		// Snapshots up to migration 3 selected Nezha automatically. Move those
		// installations to the official Komari theme.
		if migrated < 4 && currentTheme == LegacyLiteDefaultTheme {
			currentTheme = DefaultTheme
		}
	}
	if !IsLocalThemeUsable(currentTheme) {
		currentTheme = localThemeFallback()
		if currentTheme == "" {
			if err := installEmbeddedTheme(defaultThemeBundle, DefaultTheme); err != nil {
				return fmt.Errorf("restore bundled default theme: %w", err)
			}
			currentTheme = DefaultTheme
		}
	}
	return config.SetMany(map[string]any{
		config.ThemeKey:         currentTheme,
		themeBundleMigrationKey: currentThemeBundleMigration,
	})
}

func publicThemeFileContent(themeID, relativePath string) ([]byte, string, bool) {
	if IsLocalThemeUsable(themeID) {
		return localThemeFileContent(themeID, relativePath)
	}
	return embeddedFileContent(defaultThemeBundle, relativePath)
}

// EffectiveThemeID reports the theme a deployed public site can actually serve.
func EffectiveThemeID(configured string) string {
	if (flags.SplitPanelEnabled() || flags.SharedOriginThemesEnabled()) && IsLocalThemeUsable(configured) {
		return configured
	}
	return DefaultTheme
}

// Static serves the management UI and public application on the shared listener.
// Only an explicit operator opt-in allows an installed public theme here.
func Static(r *gin.RouterGroup, noRoute func(handlers ...gin.HandlerFunc), sharedThemes bool) {
	getConfig := func() map[string]any {
		cfg, _ := config.GetMany(map[string]any{
			config.DescriptionKey: defaultSiteDescription,
			config.SitenameKey:    "Komari Monitor",
		})
		return cfg
	}
	getPublicFile := func(c *gin.Context, relativePath string) ([]byte, string, bool) {
		if sharedThemes && c.GetHeader("Service-Worker") != "script" {
			themeID, _ := config.GetAs[string](config.ThemeKey, DefaultTheme)
			if themeID != DefaultTheme && IsLocalThemeUsable(themeID) {
				return localThemeFileContent(themeID, relativePath)
			}
		}
		return embeddedFileContent(defaultThemeBundle, relativePath)
	}

	// 核心逻辑：渲染 Index.html
	serveIndex := func(c *gin.Context) {
		reqPath := c.Request.URL.Path
		// index.html contains live site metadata and must never be served stale.
		setNoStoreHeaders(c)
		cfg := getConfig()

		privateApplication := isPrivateApplicationPath(reqPath)
		targetFile := path.Join(DistDir, IndexFile)
		var content []byte
		var exists bool
		if privateApplication {
			content, _, exists = embeddedFileContent("systemUI", targetFile)
		} else {
			content, _, exists = getPublicFile(c, targetFile)
			if !exists {
				content, _, exists = embeddedFileContent(defaultThemeBundle, targetFile)
			}
		}

		if !exists {
			c.String(http.StatusNotFound, "Application index is unavailable.")
			return
		}

		htmlStr := string(content)
		if language, err := c.Cookie(LanguageCookieName); err == nil {
			htmlStr = replaceHTMLLanguage(htmlStr, language)
		}

		if privateApplication {
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(injectHTTPErrorNotice(htmlStr)))
			return
		}

		sitename, _ := cfg[config.SitenameKey].(string)
		rendered := renderPublicDocumentTitle(renderPublicDescription(htmlStr, cfg[config.DescriptionKey]), sitename)
		c.Data(http.StatusOK, "text/html; charset=utf-8",
			[]byte(injectNativeLoginBridge(injectHTTPErrorNotice(rendered))))
	}

	// ================= 路由定义 =================

	// 1. Favicon 优先策略
	r.GET("/favicon.ico", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
		c.Header("Pragma", "no-cache")
		c.Header("Expires", "0")

		// 优先：./data/favicon.ico
		localFavicon := filepath.Join(datadir.Resolve(DataDir), FaviconFile)
		if _, err := os.Stat(localFavicon); err == nil {
			c.Header("Content-Type", contentTypeForPath(localFavicon))
			c.File(localFavicon)
			return
		}

		themeFaviconPath := path.Join(DistDir, FaviconFile)
		content, mimeType, exists := getPublicFile(c, themeFaviconPath)
		if exists {
			c.Data(http.StatusOK, mimeType, content)
			return
		}

		// Fresh installations and themes without their own favicon use the
		// system UI icon instead of returning a broken image.
		content, mimeType, exists = embeddedFileContent("systemUI", themeFaviconPath)
		if exists {
			c.Data(http.StatusOK, mimeType, content)
			return
		}

		c.Status(http.StatusNotFound)
	})

	// System application assets are immutable and independent from public themes.
	r.GET("/system-assets/*path", func(c *gin.Context) {
		filePath := path.Join(DistDir, c.Param("path"))
		content, mimeType, exists := embeddedFileContent("systemUI", filePath)
		if !exists {
			c.Status(http.StatusNotFound)
			return
		}
		setStaticCacheHeaders(c, c.Request.URL.Path)
		c.Data(http.StatusOK, mimeType, content)
	})

	noRoute(func(c *gin.Context) {
		if c.Request.Method != http.MethodGet {
			c.Status(http.StatusNotFound)
			return
		}
		reqPath := c.Request.URL.Path
		if isUnmountedAPIOrConnectPath(reqPath) || reqPath == "/themes" || strings.HasPrefix(reqPath, "/themes/") || reqPath == "/system-assets" {
			c.Status(http.StatusNotFound)
			return
		}
		if reqPath == "/index.html" {
			serveIndex(c)
			return
		}

		distPath := path.Join(DistDir, reqPath)

		var content []byte
		var mimeType string
		var exists bool
		if !isPrivateApplicationPath(reqPath) {
			content, mimeType, exists = getPublicFile(c, distPath)
		}
		if exists {
			if sharedThemes {
				setNoStoreHeaders(c)
			} else {
				setStaticCacheHeaders(c, reqPath)
			}
			c.Data(http.StatusOK, mimeType, content)
			return
		}

		if ext := filepath.Ext(reqPath); !isPrivateApplicationPath(reqPath) && ext != "" && ext != ".html" {
			c.Status(http.StatusNotFound)
			return
		}

		// 路由 (如 /dashboard, /settings) -> 返回 index.html
		serveIndex(c)
	})
}

func setNoStoreHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
}

func isUnmountedAPIOrConnectPath(requestPath string) bool {
	return requestPath == "/api" ||
		strings.HasPrefix(requestPath, "/api/") ||
		strings.HasPrefix(requestPath, "/komari.")
}

func isPrivateApplicationPath(requestPath string) bool {
	for _, prefix := range []string{"/admin", "/login", "/terminal", "/install", "/manage", "/bind", "/bind-login", "/enroll", "/database-recovery"} {
		if requestPath == prefix || strings.HasPrefix(requestPath, prefix+"/") {
			return true
		}
	}
	return false
}

func renderPrivateSitePublicNotice(sitename, panelOrigin string) []byte {
	title := strings.TrimSpace(sitename)
	if title == "" {
		title = "Komari Monitor"
	}
	escapedTitle := html.EscapeString(title)
	actionMarkup := `<p class="hint">Please sign in from the dedicated management panel domain configured by your administrator.</p>`
	if strings.TrimSpace(panelOrigin) != "" {
		loginURL := html.EscapeString(strings.TrimSuffix(strings.TrimSpace(panelOrigin), "/") + "/login")
		actionMarkup = `<p><a class="btn" href="` + loginURL + `" rel="noopener noreferrer">Proceed to Management Panel</a></p>`
	}
	doc := `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + escapedTitle + `</title>` +
		`<style>:root{color-scheme:light dark;font-family:system-ui,-apple-system,sans-serif}body{margin:0;min-height:100vh;display:grid;place-items:center;padding:24px}.card{max-width:460px;padding:28px;border:1px solid rgba(128,128,128,.3);border-radius:12px}h1{margin:0 0 12px;font-size:20px}p{margin:0 0 16px;line-height:1.5;opacity:.85}.btn{display:inline-block;padding:10px 16px;border-radius:8px;background:#2563eb;color:#fff;text-decoration:none;font-weight:600}.hint{font-size:14px;margin-bottom:0}</style></head>` +
		`<body><main class="card"><h1>` + escapedTitle + `</h1><p>Private site mode is enabled. Credentials and Passkeys are never accepted on the public status listener.</p>` + actionMarkup + `</main></body></html>`
	return []byte(doc)
}

// StaticPublic mounts only public theme assets and SPA routes on the dedicated
// Public Listener. Management routes (/admin, /login, /terminal, /bind, etc.)
// and /system-assets/* are intentionally unmounted and return 404.
func StaticPublic(r *gin.RouterGroup, noRoute func(handlers ...gin.HandlerFunc), decorateTheme func(*http.Request, []byte) []byte) {
	getConfig := func() map[string]any {
		cfg, _ := config.GetMany(map[string]any{
			config.DescriptionKey: defaultSiteDescription,
			config.CustomHeadKey:  "",
			config.CustomBodyKey:  "",
			config.SitenameKey:    "Komari Monitor",
			config.ThemeKey:       DefaultTheme,
			config.PrivateSiteKey: false,
		})
		return cfg
	}

	servePublicIndex := func(c *gin.Context, cfg map[string]any, tempShareValid bool) {
		setNoStoreHeaders(c)
		privateSite, _ := cfg[config.PrivateSiteKey].(bool)
		if privateSite && !tempShareValid {
			c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
			c.Header("X-Content-Type-Options", "nosniff")
			sitename, _ := cfg[config.SitenameKey].(string)
			c.Data(http.StatusOK, "text/html; charset=utf-8", renderPrivateSitePublicNotice(sitename, flags.EffectivePanelOrigin()))
			return
		}

		themeName, _ := cfg[config.ThemeKey].(string)
		content, _, exists := publicThemeFileContent(themeName, path.Join(DistDir, IndexFile))
		if !exists {
			c.String(http.StatusNotFound, "Application index is unavailable.")
			return
		}

		htmlStr := string(content)
		if language, err := c.Cookie(LanguageCookieName); err == nil {
			htmlStr = replaceHTMLLanguage(htmlStr, language)
		}
		customHead, _ := cfg[config.CustomHeadKey].(string)
		customBody, _ := cfg[config.CustomBodyKey].(string)
		sitename, _ := cfg[config.SitenameKey].(string)

		htmlStr = renderPublicDescription(htmlStr, cfg[config.DescriptionKey])
		htmlStr = injectCustomHTML(htmlStr, customHead, customBody)
		rendered := renderPublicDocumentTitle(htmlStr, sitename)
		// Never inject native_login_bridge.js on the Public Listener: public
		// status origins never accept passwords or Passkeys.
		document := []byte(injectHTTPErrorNotice(injectThemeChangeReload(rendered)))
		if decorateTheme != nil {
			document = decorateTheme(c.Request, document)
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", document)
	}

	r.GET("/favicon.ico", func(c *gin.Context) {
		setNoStoreHeaders(c)
		localFavicon := filepath.Join(datadir.Resolve(DataDir), FaviconFile)
		if _, err := os.Stat(localFavicon); err == nil {
			c.Header("Content-Type", contentTypeForPath(localFavicon))
			c.File(localFavicon)
			return
		}
		cfg := getConfig()
		themeName, _ := cfg[config.ThemeKey].(string)
		themeFaviconPath := path.Join(DistDir, FaviconFile)
		if content, mimeType, exists := publicThemeFileContent(themeName, themeFaviconPath); exists {
			c.Data(http.StatusOK, mimeType, content)
			return
		}
		if content, mimeType, exists := embeddedFileContent("systemUI", themeFaviconPath); exists {
			c.Data(http.StatusOK, mimeType, content)
			return
		}
		c.Status(http.StatusNotFound)
	})

	r.GET("/themes/:id/*path", func(c *gin.Context) {
		content, mimeType, exists := localThemeFileContent(c.Param("id"), c.Param("path"))
		if !exists {
			c.Status(http.StatusNotFound)
			return
		}
		c.Data(http.StatusOK, mimeType, content)
	})

	noRoute(func(c *gin.Context) {
		if c.Request.Method != http.MethodGet {
			c.Status(http.StatusNotFound)
			return
		}
		reqPath := c.Request.URL.Path
		if isPrivateApplicationPath(reqPath) ||
			strings.HasPrefix(reqPath, "/system-assets/") ||
			isUnmountedAPIOrConnectPath(reqPath) {
			c.Status(http.StatusNotFound)
			return
		}
		tempShareValid := websecurity.HasTempAccess(c)
		cfg := getConfig()
		if reqPath == "/index.html" {
			servePublicIndex(c, cfg, tempShareValid)
			return
		}
		themeName, _ := cfg[config.ThemeKey].(string)
		if content, mimeType, exists := publicThemeFileContent(themeName, path.Join(DistDir, reqPath)); exists {
			setStaticCacheHeaders(c, reqPath)
			c.Data(http.StatusOK, mimeType, content)
			return
		}
		if ext := filepath.Ext(reqPath); ext != "" && ext != ".html" {
			c.Status(http.StatusNotFound)
			return
		}
		servePublicIndex(c, cfg, tempShareValid)
	})
}

// StaticPanel mounts only the built-in trusted management UI (systemUI) on the
// dedicated Panel Listener. Third-party theme files (/themes/*) and public SPA
// routes are never mounted and return 404.
func StaticPanel(r *gin.RouterGroup, noRoute func(handlers ...gin.HandlerFunc)) {
	serveSystemIndex := func(c *gin.Context) {
		setNoStoreHeaders(c)
		content, _, exists := embeddedFileContent("systemUI", path.Join(DistDir, IndexFile))
		if !exists {
			c.String(http.StatusNotFound, "Application index is unavailable.")
			return
		}
		htmlStr := string(content)
		if language, err := c.Cookie(LanguageCookieName); err == nil {
			htmlStr = replaceHTMLLanguage(htmlStr, language)
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(injectHTTPErrorNotice(htmlStr)))
	}

	r.GET("/favicon.ico", func(c *gin.Context) {
		setNoStoreHeaders(c)
		localFavicon := filepath.Join(datadir.Resolve(DataDir), FaviconFile)
		if _, err := os.Stat(localFavicon); err == nil {
			c.Header("Content-Type", contentTypeForPath(localFavicon))
			c.File(localFavicon)
			return
		}
		if content, mimeType, exists := embeddedFileContent("systemUI", path.Join(DistDir, FaviconFile)); exists {
			c.Data(http.StatusOK, mimeType, content)
			return
		}
		c.Status(http.StatusNotFound)
	})

	r.GET("/system-assets/*path", func(c *gin.Context) {
		filePath := path.Join(DistDir, c.Param("path"))
		content, mimeType, exists := embeddedFileContent("systemUI", filePath)
		if !exists {
			c.Status(http.StatusNotFound)
			return
		}
		setStaticCacheHeaders(c, c.Request.URL.Path)
		c.Data(http.StatusOK, mimeType, content)
	})

	noRoute(func(c *gin.Context) {
		if c.Request.Method != http.MethodGet {
			c.Status(http.StatusNotFound)
			return
		}
		reqPath := c.Request.URL.Path
		if reqPath == "/" {
			setNoStoreHeaders(c)
			c.Redirect(http.StatusTemporaryRedirect, "/admin")
			return
		}
		if !isPrivateApplicationPath(reqPath) {
			c.Status(http.StatusNotFound)
			return
		}
		serveSystemIndex(c)
	})
}

func setStaticCacheHeaders(c *gin.Context, requestPath string) {
	name := strings.ToLower(path.Base(requestPath))
	switch name {
	case "index.html", "sw.js", "service-worker.js", "registersw.js", "manifest.webmanifest":
		setNoStoreHeaders(c)
		return
	}
	if (strings.HasPrefix(requestPath, "/assets/") || strings.HasPrefix(requestPath, "/system-assets/assets/")) &&
		strings.Contains(strings.TrimSuffix(name, filepath.Ext(name)), "-") {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	}
}

package plugin

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var iconMediaTypes = map[string]string{
	".avif": "image/avif",
	".gif":  "image/gif",
	".ico":  "image/x-icon",
	".jpeg": "image/jpeg",
	".jpg":  "image/jpeg",
	".png":  "image/png",
	".svg":  "image/svg+xml",
	".webp": "image/webp",
}

// OpenIcon opens a manifest-declared image for the administration UI, even
// when the plugin is disabled or has no iframe page.
func OpenIcon(short, name string) (*os.File, string, error) {
	mediaType := iconMediaTypes[strings.ToLower(path.Ext(name))]
	if mediaType == "" || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:") {
		return nil, "", fmt.Errorf("invalid plugin icon path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return nil, "", fmt.Errorf("invalid plugin icon path %q", name)
		}
	}
	info, err := Manifest(short)
	if err != nil {
		return nil, "", err
	}
	matches := func(icon string) bool {
		return icon != "" && filepath.ToSlash(filepath.Clean(icon)) == name
	}
	declared := matches(info.Icon) || matches(info.Configuration.Icon)
	for _, page := range info.Pages {
		declared = declared || matches(page.Icon)
	}
	if !declared || matches(info.Entry) {
		return nil, "", fmt.Errorf("plugin icon %q is not available", name)
	}
	file, err := OpenFile(short, filepath.FromSlash(name))
	return file, mediaType, err
}

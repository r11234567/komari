package plugin

import "strings"

// Workspace credentials, OTPs and file bytes must never enter generic plugin
// hooks. Custom plugin routes and the existing RPC2 compatibility gateway keep
// their previous scopes; this does not turn RPC2 into a management transport.
func protectedWorkspacePath(path string) bool {
	path = strings.ToLower(path)
	for _, prefix := range []string{"/komari.workspace.v1.workspaceservice/", "/komari.webssh.v1.websshservice/", "/komari.file.v1.fileservice/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return (strings.HasPrefix(path, "/api/admin/client/") || strings.HasPrefix(path, "/api/preview/client/") || strings.HasPrefix(path, "/api/clients/transfer/")) && strings.Contains(path, "/file/") || strings.HasPrefix(path, "/api/clients/transfer/")
}

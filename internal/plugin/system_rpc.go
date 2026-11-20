package plugin

import (
	"strings"

	"github.com/komari-monitor/komari/pkg/rpc"
	logger "github.com/komari-monitor/komari/utils/log"
)

func isForbiddenPluginRPCNamespace(method string) bool {
	ns, _, ok := strings.Cut(strings.ToLower(strings.TrimSpace(method)), ":")
	if !ok {
		return true
	}
	switch ns {
	case "admin", "client", "rpc":
		return true
	default:
		return false
	}
}

// SetSystemRPCAllowlist replaces the explicit allowlist of non-privileged RPC
// methods that plugins with allowSystemRPC may invoke via server.call. The
// default allowlist is empty so deployments without consumers keep system RPC
// closed. Privileged namespaces (admin:*, client:*, rpc:*) are always ignored.
func SetSystemRPCAllowlist(methods ...string) {
	global.SetSystemRPCAllowlist(methods...)
}

func (m *Manager) SetSystemRPCAllowlist(methods ...string) {
	next := make(map[string]struct{}, len(methods))
	for _, raw := range methods {
		method := strings.TrimSpace(raw)
		if method == "" || isForbiddenPluginRPCNamespace(method) {
			continue
		}
		next[method] = struct{}{}
	}
	m.rpcAllowMu.Lock()
	m.rpcAllowlist = next
	m.rpcAllowMu.Unlock()
}

func (m *Manager) isSystemRPCAllowed(method string) bool {
	if isForbiddenPluginRPCNamespace(method) {
		return false
	}
	m.rpcAllowMu.RLock()
	_, ok := m.rpcAllowlist[method]
	m.rpcAllowMu.RUnlock()
	return ok
}

// authorizePluginRPC enforces pre-dispatch least-privilege authorization for
// plugin server.call invocations. Plugins never receive administrator or agent
// principals; they execute only explicitly allowlisted guest/read-only methods.
func (m *Manager) authorizePluginRPC(inst *Instance, method string) (*rpc.ContextMeta, *rpc.JsonRpcError) {
	if inst == nil || !inst.info.Permissions.AllowSystemRPC {
		return nil, rpc.MakeError(rpc.PermissionDenied, "server.call requires the \"system RPC\" permission (allowSystemRPC)", nil)
	}
	if m.instanceFor(inst.info.Short) != inst {
		return nil, rpc.MakeError(rpc.PermissionDenied, "plugin is not loaded", nil)
	}
	if !rpc.HasMethod(method) {
		return nil, rpc.MakeError(rpc.MethodNotFound, "Method not found", nil)
	}
	principal := rpc.NewAnonymousPrincipal()
	permitted := m.isSystemRPCAllowed(method) && (rpc.CheckPrincipal(principal, method) || rpc.IsPluginMethod(method))
	if !permitted {
		logger.Warnf("plugin-rpc", "denied plugin %q RPC call to %q", inst.info.Short, method)
		return nil, rpc.MakeError(rpc.PermissionDenied, "Permission denied", nil)
	}
	logger.Infof("plugin-rpc", "allowed plugin %q RPC call to %q", inst.info.Short, method)
	return &rpc.ContextMeta{
		Permission: rpc.RoleGuest,
		Principal:  principal,
		RemoteIP:   "plugin:" + inst.info.Short,
	}, nil
}

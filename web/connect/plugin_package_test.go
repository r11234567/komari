package connectapi

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/credentialaction"
	pluginapp "github.com/komari-monitor/komari/internal/plugin"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/rpc"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	pluginv1 "github.com/r11234567/komari-proto/gen/go/komari/plugin/v1"
	"github.com/stretchr/testify/require"
)

func pluginApprovalFixture(t *testing.T) pluginapp.PackageReview {
	t.Helper()
	require.NoError(t, pluginapp.CloseAll())
	oldDir, oldStorage := pluginapp.DataDir, pluginapp.StorageDir
	pluginapp.DataDir = t.TempDir()
	pluginapp.StorageDir = t.TempDir()
	pluginapp.Init(gin.New())
	t.Cleanup(func() { _ = pluginapp.CloseAll(); pluginapp.DataDir = oldDir; pluginapp.StorageDir = oldStorage })
	path := filepath.Join(t.TempDir(), "plugin.zip")
	f, err := os.Create(path)
	require.NoError(t, err)
	w := zip.NewWriter(f)
	for name, body := range map[string]string{"komari-plugin.json": `{"short":"proof","name":"2FA fixture","version":"1"}`, "script.js": "function load(){}"} {
		entry, err := w.Create(name)
		require.NoError(t, err)
		_, err = entry.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	require.NoError(t, f.Close())
	_, err = pluginapp.StageZip(path, "Synthetic upload", "proof", "1")
	require.NoError(t, err)
	rows, err := pluginapp.ListPackages()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0]
}

func TestPluginPackageApprovalRequiresFreshScoped2FA(t *testing.T) {
	for _, scenario := range []string{"missing", "not-configured", "raw-code-only", "wrong-package", "wrong-permissions", "wrong-action", "wrong-session", "success-and-replay"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, owner, _ := actionFixture(t)
			row := pluginApprovalFixture(t)
			req := &pluginv1.DecidePluginPackageRequest{ShortName: "proof", Digest: row.Digest, ExpectedCurrentDigest: row.CurrentDigest, Action: "activate"}
			if scenario == "raw-code-only" {
				req.TwoFactor = &commonv1.TwoFactorProof{Code: "123456"}
			} else if scenario != "missing" && scenario != "not-configured" {
				targets := row.Targets()
				action := "plugin-package:activate"
				if scenario == "wrong-package" {
					targets[1] = "different-digest"
				}
				if scenario == "wrong-permissions" {
					targets[3] = "different-permissions"
				}
				if scenario == "wrong-action" {
					action = "plugin-package:revoke"
				}
				req.TwoFactor = &commonv1.TwoFactorProof{AuthorizationToken: actionAuthorization(t, ctx, credential.SensitiveOperation, credentialaction.ScopedResource(action, targets...))}
			}
			if scenario == "wrong-session" {
				meta := *rpc.MetaFromContext(ctx)
				meta.SessionToken = "other-session-" + uuid.NewString()
				require.NoError(t, dbcore.GetDBInstance().Create(&models.Session{UUID: owner, Session: meta.SessionToken, Expires: time.Now().Add(time.Hour)}).Error)
				ctx = rpc.NewContextWithMeta(ctx, &meta)
			}
			if scenario == "not-configured" {
				require.NoError(t, dbcore.GetDBInstance().Model(&models.User{}).Where("uuid = ?", owner).Update("two_factor", "").Error)
			}
			service := &pluginService{}
			_, err := service.DecidePluginPackage(ctx, connect.NewRequest(req))
			if scenario != "success-and-replay" {
				require.Error(t, err)
				require.Empty(t, pluginapp.List())
				return
			}
			require.NoError(t, err)
			installed := pluginapp.List()
			require.Len(t, installed, 1)
			require.True(t, installed[0].Running)
			_, err = service.DecidePluginPackage(ctx, connect.NewRequest(req))
			require.Error(t, err)
		})
	}
}

func TestPluginPackageApprovalRejectsTokenAgentAndPluginPrincipals(t *testing.T) {
	for _, principal := range []*rpc.Principal{rpc.NewAnonymousPrincipal(), rpc.NewAgentPrincipal("node"), rpc.NewAPIKeyPrincipal(), rpc.PrincipalFromRole(rpc.RoleAdmin), rpc.NewUserPrincipal("no-session")} {
		ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: principal})
		service := &pluginService{}
		_, err := service.ListPluginPackages(ctx, connect.NewRequest(&pluginv1.ListPluginPackagesRequest{}))
		require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
		_, err = service.DecidePluginPackage(ctx, connect.NewRequest(&pluginv1.DecidePluginPackageRequest{Action: "activate", TwoFactor: &commonv1.TwoFactorProof{AuthorizationToken: "not-an-approval"}}))
		require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	}
}

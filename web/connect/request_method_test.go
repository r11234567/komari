package connectapi

import (
	"net/http"
	"testing"

	"connectrpc.com/connect"
	browserv1 "github.com/r11234567/komari-proto/gen/go/komari/browser/v1"
	"github.com/r11234567/komari-proto/gen/go/komari/browser/v1/browserv1connect"
	enrollmentv1 "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1"
	"github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1/enrollmentv1connect"
	"github.com/r11234567/komari-proto/gen/go/komari/file/v1/filev1connect"
	"github.com/r11234567/komari-proto/gen/go/komari/metrics/v1/metricsv1connect"
	"github.com/stretchr/testify/require"
)

func TestEntryBoundaryRestrictsSensitiveConnectGETAndKeepsPOST(t *testing.T) {
	entryPrivateSite(t, false)
	server := entryBoundaryServer(t)
	for _, procedure := range []string{
		enrollmentv1connect.EnrollmentServiceGetTrustBundleProcedure,
		enrollmentv1connect.EnrollmentAdminServiceGetPendingEnrollmentProcedure,
		filev1connect.FileServiceListDirectoryProcedure,
		metricsv1connect.MetricsServiceQueryMetricsProcedure,
		browserv1connect.BrowserServiceGetSessionProcedure,
	} {
		response, err := server.Client().Get(server.URL + procedure)
		require.NoError(t, err)
		_ = response.Body.Close()
		require.Equal(t, http.StatusMethodNotAllowed, response.StatusCode, procedure)
		require.Equal(t, http.MethodPost, response.Header.Get("Allow"))
		require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	}
	client := enrollmentv1connect.NewEnrollmentServiceClient(server.Client(), server.URL)
	trust, err := client.GetTrustBundle(t.Context(), connect.NewRequest(&enrollmentv1.GetTrustBundleRequest{}))
	require.NoError(t, err)
	require.NotEmpty(t, trust.Msg.GetSigningKeys())
	require.Equal(t, "no-store", trust.Header().Get("Cache-Control"))
	public := browserv1connect.NewBrowserServiceClient(server.Client(), server.URL, connect.WithHTTPGet())
	_, err = public.GetThemeContract(t.Context(), connect.NewRequest(&browserv1.GetThemeContractRequest{}))
	require.NoError(t, err)
}

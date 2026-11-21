package api

import (
	browser "github.com/r11234567/komari-proto/gen/go/komari/browser/v1/browserv1connect"
	credential "github.com/r11234567/komari-proto/gen/go/komari/credential/v1/credentialv1connect"
	enrollment "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1/enrollmentv1connect"
)

// privateSiteBootstrapProcedure lists the exact Connect RPC procedures that an
// unauthenticated browser or enrolling/refreshing agent must be able to invoke
// even when private_site is enabled.
func privateSiteBootstrapProcedure(procedure string) bool {
	switch procedure {
	case browser.BrowserServiceGetPublicInfoProcedure,
		browser.BrowserServiceGetThemeContractProcedure,
		browser.BrowserServiceGetSessionProcedure,
		credential.PasskeyLoginServiceGetPasskeyStatusProcedure,
		credential.PasskeyLoginServiceBeginPasskeyLoginProcedure,
		credential.PasskeyLoginServiceCompletePasskeyLoginProcedure,
		credential.PasswordLoginServiceBeginPasswordLoginProcedure,
		credential.PasswordLoginServiceBeginLoginVerificationProcedure,
		credential.PasswordLoginServiceCompletePasswordLoginProcedure,
		credential.PasswordLoginServiceCancelPasswordLoginProcedure,
		enrollment.EnrollmentServiceBeginEnrollmentProcedure,
		enrollment.EnrollmentServicePollEnrollmentProcedure,
		enrollment.EnrollmentServiceGetTrustBundleProcedure,
		enrollment.EnrollmentServiceRefreshCredentialsProcedure:
		return true
	default:
		return false
	}
}

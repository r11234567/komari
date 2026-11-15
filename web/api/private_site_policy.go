package api

import (
	browser "github.com/r11234567/komari-proto/gen/go/komari/browser/v1/browserv1connect"
	credential "github.com/r11234567/komari-proto/gen/go/komari/credential/v1/credentialv1connect"
	enrollment "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1/enrollmentv1connect"
)

// Private-site visibility is separate from authentication. These exact
// procedures must reach their own password/preauth, device-code or signed
// refresh validators without an existing access token. This is deliberately
// not a service-prefix exemption: revocation, approval and trusted-credential
// operations still require their normal interactive administrator identity.
func privateSiteBootstrapProcedure(procedure string) bool {
	switch procedure {
	case browser.BrowserServiceGetPublicInfoProcedure,
		browser.BrowserServiceGetThemeContractProcedure,
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

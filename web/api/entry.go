package api

import (
	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/web/security"
)

// ConfigureEntryBoundary is the shared production entry stack across main,
// public, and panel listeners as well as transport integration tests:
// RequestMetadata -> CORS -> APIAuth -> Identity -> RateLimit -> PrivateSite.
func ConfigureEntryBoundary(r *gin.Engine, cors *security.CorsController, budget *security.RateLimitController, pluginRoute ...func(string, string) bool) error {
	return ConfigureEntryBoundaryForOrigin(r, "", cors, budget, pluginRoute...)
}

// ConfigureEntryBoundaryForOrigin binds the shared entry stack with an
// explicit listener canonical origin (e.g. EffectivePublicOrigin or EffectivePanelOrigin).
func ConfigureEntryBoundaryForOrigin(r *gin.Engine, canonicalOrigin string, cors *security.CorsController, budget *security.RateLimitController, pluginRoute ...func(string, string) bool) error {
	if canonicalOrigin != "" {
		if err := security.ConfigureRequestMetadataForOrigin(r, canonicalOrigin); err != nil {
			return err
		}
	} else {
		if err := security.ConfigureRequestMetadata(r); err != nil {
			return err
		}
	}
	r.Use(cors.Middleware(), APIAuthenticationMiddleware(pluginRoute...), IdentityMiddleware(), budget.Middleware(), PrivateSiteMiddleware())
	return nil
}

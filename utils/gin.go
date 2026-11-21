package utils

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/requestmeta"
	"github.com/komari-monitor/komari/utils/requestscheme"
)

// https://github.com/labstack/echo/blob/98ca08e7dd64075b858e758d6693bf9799340756/context.go#L275-L294
func GetScheme(c *gin.Context) string {
	if requestscheme.IsHTTPS(c.Request) {
		return "https"
	}
	return "http"
}

func PublicPanelURL(ctx context.Context) (string, error) {
	configured, err := config.GetAs[string](config.ScriptDomainKey, "")
	if err != nil {
		return "", err
	}
	return requestmeta.PublicURL(ctx, configured)
}

func GetCallbackURL(c *gin.Context) (string, error) {
	base, err := PublicPanelURL(c.Request.Context())
	if err != nil {
		return "", err
	}
	return base + "/api/oauth_callback", nil
}

package github

import (
	"net/http"

	"github.com/komari-monitor/komari/web/oauth/factory"
)

func init() {
	factory.RegisterOidcProvider(func() factory.IOidcProvider {
		return &Github{}
	})
}

type Github struct {
	Addition
	stateStore *factory.StateStore
	httpClient *http.Client
	tokenURL   string
	userURL    string
}

type Addition struct {
	ClientId     string `json:"client_id" required:"true"`
	ClientSecret string `json:"client_secret" required:"true"`
}

type GitHubUser struct {
	ID    int    `json:"id"`
	Login string `json:"login"`
	Email string `json:"email"`
}

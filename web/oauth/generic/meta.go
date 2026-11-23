package generic

import (
	"net/http"

	"github.com/komari-monitor/komari/web/oauth/factory"
)

func init() {
	factory.RegisterOidcProvider(func() factory.IOidcProvider {
		return &Generic{}
	})
}

type Generic struct {
	Addition
	stateStore *factory.StateStore
	httpClient *http.Client
}

type Addition struct {
	ClientId     string `json:"client_id" required:"true"`
	ClientSecret string `json:"client_secret" required:"true"`
	AuthURL      string `json:"auth_url" required:"true"`
	TokenURL     string `json:"token_url" required:"true"`
	UserInfoURL  string `json:"user_info_url" required:"true"`
	Scope        string `json:"scope"`
	UserIDField  string `json:"user_id_field" required:"true"`
}

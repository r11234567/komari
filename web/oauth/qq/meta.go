package qq

import (
	"net/http"

	"github.com/komari-monitor/komari/web/oauth/factory"
)

func init() {
	factory.RegisterOidcProvider(func() factory.IOidcProvider {
		return &QQ{}
	})
}

type QQ struct {
	Addition
	stateStore *factory.StateStore
	httpClient *http.Client
}

type Addition struct {
	AggregationURL string `json:"aggregation_url" required:"true" default:"https://login.qjqq.cn"` // 聚合登录地址
	AppId          string `json:"app_id" required:"true"`
	AppKey         string `json:"app_key" required:"true"`
	LoginType      string `json:"login_type" required:"true"` // 登录方式，如qq, google等
}

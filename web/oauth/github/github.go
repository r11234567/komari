package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/utils"
	"github.com/komari-monitor/komari/web/oauth/factory"
)

func init() {

}

func (g *Github) GetName() string {
	return "github"
}
func (g *Github) GetConfiguration() factory.Configuration {
	return &g.Addition
}

func (g *Github) GetAuthorizationURL(_ string) (string, string) {
	if g.stateStore == nil {
		return "", ""
	}
	state := utils.GenerateRandomString(16)
	if !g.stateStore.Issue(state) {
		return "", ""
	}

	// 构建GitHub OAuth授权URL
	authURL := fmt.Sprintf(
		"https://github.com/login/oauth/authorize?client_id=%s&state=%s&scope=user:email",
		url.QueryEscape(g.Addition.ClientId),
		url.QueryEscape(state),
	)
	return authURL, state
}
func (g *Github) OnCallback(ctx *gin.Context, state string, query map[string]string, _ string) (factory.OidcCallback, error) {
	code := query["code"]

	// 验证并原子消费 state 防止 CSRF 与重放攻击
	if g.stateStore == nil {
		return factory.OidcCallback{}, fmt.Errorf("state cache not initialized")
	}
	if !g.stateStore.Consume(state) {
		return factory.OidcCallback{}, fmt.Errorf("invalid state")
	}

	if code == "" {
		return factory.OidcCallback{}, fmt.Errorf("no code provided")
	}

	// 获取访问令牌
	tokenURL := g.tokenURL
	if tokenURL == "" {
		tokenURL = "https://github.com/login/oauth/access_token"
	}
	data := url.Values{
		"client_id":     {g.Addition.ClientId},
		"client_secret": {g.Addition.ClientSecret},
		"code":          {code},
	}

	req, err := http.NewRequest(http.MethodPost, tokenURL, nil)
	if err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to build token request")
	}
	req.URL.RawQuery = data.Encode()
	req.Header.Set("Accept", "application/json")

	status, tokenBody, err := factory.DoBoundedRequest(g.httpClient, req, factory.MaxResponseBodyBytes)
	if err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to get access token: %s", utils.DataMasking(err.Error(), []string{g.Addition.ClientSecret, g.Addition.ClientId}))
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return factory.OidcCallback{}, fmt.Errorf("failed to get access token: HTTP %d", status)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
	}

	if err := json.Unmarshal(tokenBody, &tokenResp); err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to parse access token response: %s", utils.DataMasking(err.Error(), []string{g.Addition.ClientSecret, g.Addition.ClientId}))
	}
	if tokenResp.AccessToken == "" {
		return factory.OidcCallback{}, fmt.Errorf("empty access token returned")
	}

	// 获取用户信息
	userURL := g.userURL
	if userURL == "" {
		userURL = "https://api.github.com/user"
	}
	userReq, err := http.NewRequest(http.MethodGet, userURL, nil)
	if err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to build user info request")
	}
	userReq.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
	userReq.Header.Set("Accept", "application/json")

	userStatus, userBody, err := factory.DoBoundedRequest(g.httpClient, userReq, factory.MaxResponseBodyBytes)
	if err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to get user info: %v", err)
	}
	if userStatus < http.StatusOK || userStatus >= http.StatusMultipleChoices {
		return factory.OidcCallback{}, fmt.Errorf("failed to get user info: HTTP %d", userStatus)
	}

	var githubUser GitHubUser
	if err := json.Unmarshal(userBody, &githubUser); err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to parse user info response: %v", err)
	}
	if githubUser.ID == 0 {
		return factory.OidcCallback{}, fmt.Errorf("empty user id returned")
	}

	return factory.OidcCallback{UserId: fmt.Sprintf("%d", githubUser.ID)}, nil
}
func (g *Github) Init() error {
	g.stateStore = factory.NewStateStore(0, 0)
	g.httpClient = factory.NewHTTPClient()
	return nil
}
func (g *Github) Destroy() error {
	if g.stateStore != nil {
		g.stateStore.Reset()
		g.stateStore = nil
	}
	return nil
}

var _ factory.IOidcProvider = (*Github)(nil)

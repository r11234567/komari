package generic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/utils"
	"github.com/komari-monitor/komari/web/oauth/factory"
)

func (g *Generic) GetName() string {
	return "generic"
}
func (g *Generic) GetConfiguration() factory.Configuration {
	return &g.Addition
}

func (g *Generic) GetAuthorizationURL(redirectURI string) (string, string) {
	if g.stateStore == nil {
		return "", ""
	}
	state := utils.GenerateRandomString(16)
	if !g.stateStore.Issue(state) {
		return "", ""
	}

	// 构建通用 OAuth 授权 URL
	authURL := fmt.Sprintf(
		"%s?client_id=%s&state=%s&scope=%s&redirect_uri=%s&response_type=code",
		g.Addition.AuthURL,
		url.QueryEscape(g.Addition.ClientId),
		url.QueryEscape(state),
		url.QueryEscape(g.Addition.Scope),
		url.QueryEscape(redirectURI),
	)
	return authURL, state
}
func (g *Generic) OnCallback(ctx *gin.Context, state string, query map[string]string, callbackURI string) (factory.OidcCallback, error) {
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
	data := url.Values{
		"client_id":     {g.Addition.ClientId},
		"client_secret": {g.Addition.ClientSecret},
		"code":          {code},
		"redirect_uri":  {callbackURI},
		"grant_type":    {"authorization_code"},
	}

	req, err := http.NewRequest(http.MethodPost, g.Addition.TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to build token request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	status, tokenBody, err := factory.DoBoundedRequest(g.httpClient, req, factory.MaxResponseBodyBytes)
	if err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to get access token: %s", utils.DataMasking(err.Error(), []string{g.Addition.ClientSecret, g.Addition.ClientId}))
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return factory.OidcCallback{}, fmt.Errorf("failed to get access token: HTTP %d", status)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}

	if err := json.Unmarshal(tokenBody, &tokenResp); err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to parse access token response: %s", utils.DataMasking(err.Error(), []string{g.Addition.ClientSecret, g.Addition.ClientId}))
	}
	if tokenResp.AccessToken == "" {
		return factory.OidcCallback{}, fmt.Errorf("empty access token returned")
	}

	// 获取用户信息
	userReq, err := http.NewRequest(http.MethodGet, g.Addition.UserInfoURL, nil)
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

	var user map[string]interface{}
	if err := json.Unmarshal(userBody, &user); err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to parse user info response: %v", err)
	}

	userId, ok := user[g.Addition.UserIDField]
	if !ok || userId == nil || fmt.Sprintf("%v", userId) == "" {
		return factory.OidcCallback{}, fmt.Errorf("user id field '%s' not found in user info response", g.Addition.UserIDField)
	}

	return factory.OidcCallback{UserId: fmt.Sprintf("%v", userId)}, nil
}
func (g *Generic) Init() error {
	g.stateStore = factory.NewStateStore(0, 0)
	g.httpClient = factory.NewHTTPClient()
	return nil
}
func (g *Generic) Destroy() error {
	if g.stateStore != nil {
		g.stateStore.Reset()
		g.stateStore = nil
	}
	return nil
}

var _ factory.IOidcProvider = (*Generic)(nil)

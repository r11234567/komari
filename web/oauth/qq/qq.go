package qq

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/utils"
	"github.com/komari-monitor/komari/web/oauth/factory"
)

func (q *QQ) GetName() string {
	return "qq"
}

func (q *QQ) GetConfiguration() factory.Configuration {
	return &q.Addition
}

func (q *QQ) GetAuthorizationURL(redirectURI string) (string, string) {
	if q.stateStore == nil {
		return "", ""
	}
	state := utils.GenerateRandomString(16)

	callbackTarget := redirectURI
	if parsed, err := url.Parse(redirectURI); err == nil {
		query := parsed.Query()
		query.Set("state", state)
		parsed.RawQuery = query.Encode()
		callbackTarget = parsed.String()
	}

	// 构建请求QQ聚合登录平台的URL
	requestURL := fmt.Sprintf(
		"%s/connect.php?act=login&appid=%s&appkey=%s&type=%s&redirect_uri=%s",
		q.Addition.AggregationURL,
		url.QueryEscape(q.Addition.AppId),
		url.QueryEscape(q.Addition.AppKey),
		url.QueryEscape(q.Addition.LoginType),
		url.QueryEscape(callbackTarget),
	)

	req, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		return "", ""
	}
	status, body, err := factory.DoBoundedRequest(q.httpClient, req, factory.MaxResponseBodyBytes)
	if err != nil || status < http.StatusOK || status >= http.StatusMultipleChoices {
		return "", ""
	}

	// 解析响应JSON
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		URL  string `json:"url"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return "", ""
	}

	// 检查响应状态
	if result.Code != 0 || result.URL == "" {
		return "", ""
	}

	if !q.stateStore.Issue(state) {
		return "", ""
	}
	return result.URL, state
}

// consumeState atomically verifies and removes a pending OAuth state token so
// concurrent callbacks cannot replay the same state.
func (q *QQ) consumeState(state string) bool {
	return q.stateStore != nil && q.stateStore.Consume(state)
}

// OnCallback 处理QQ OAuth回调
// 例如：http://localhost:25774/api/oauth_callback?type=qq&code=XXXXXXXXXXXXXXXX
// 然后我们使用code参数向聚合登录平台请求用户信息
func (q *QQ) OnCallback(ctx *gin.Context, state string, query map[string]string, callbackURI string) (factory.OidcCallback, error) {
	// 根据文档，回调地址会附带type和code参数
	code := query["code"]
	loginType := query["type"]

	// 如果回调中没有type参数，则使用配置中的LoginType
	if loginType == "" {
		loginType = q.Addition.LoginType
	}

	// 验证state防止CSRF攻击，并原子消费防并发重放
	if q.stateStore == nil {
		return factory.OidcCallback{}, fmt.Errorf("state cache not initialized")
	}
	if !q.consumeState(state) {
		return factory.OidcCallback{}, fmt.Errorf("invalid state")
	}

	// 检查是否提供了Authorization Code
	if code == "" {
		return factory.OidcCallback{}, fmt.Errorf("no authorization code provided")
	}

	// 通过Authorization Code获取用户信息
	// 根据文档，请求URL应为: {AggregationURL}/connect.php?act=callback&appid={appid}&appkey={appkey}&type={登录方式}&code={code}
	callbackURL := fmt.Sprintf(
		"%s/connect.php?act=callback&appid=%s&appkey=%s&type=%s&code=%s",
		q.Addition.AggregationURL,
		url.QueryEscape(q.Addition.AppId),
		url.QueryEscape(q.Addition.AppKey),
		url.QueryEscape(loginType),
		url.QueryEscape(code),
	)

	req, err := http.NewRequest(http.MethodGet, callbackURL, nil)
	if err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to build callback request")
	}
	status, body, err := factory.DoBoundedRequest(q.httpClient, req, factory.MaxResponseBodyBytes)
	if err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to get user info: %v", err)
	}

	// 检查HTTP响应状态
	if status != http.StatusOK {
		return factory.OidcCallback{}, fmt.Errorf("HTTP request failed with status code: %d", status)
	}

	// 解析响应
	var result struct {
		Code        int    `json:"code"`
		Msg         string `json:"msg"`
		Type        string `json:"type"`
		SocialUid   string `json:"social_uid"`
		AccessToken string `json:"access_token"`
		FaceImg     string `json:"faceimg"`
		Nickname    string `json:"nickname"`
		Gender      string `json:"gender"`
		Location    string `json:"location"`
		IP          string `json:"ip"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to parse callback response: %v", err)
	}

	// 检查返回状态码
	if result.Code != 0 {
		return factory.OidcCallback{}, fmt.Errorf("QQ login callback failed with code %d", result.Code)
	}

	// 检查是否返回了用户唯一标识
	if result.SocialUid == "" {
		return factory.OidcCallback{}, fmt.Errorf("empty social_uid returned")
	}

	// 返回用户唯一标识
	return factory.OidcCallback{UserId: result.SocialUid}, nil
}

func (q *QQ) Init() error {
	q.stateStore = factory.NewStateStore(0, 0)
	q.httpClient = factory.NewHTTPClient()
	return nil
}

func (q *QQ) Destroy() error {
	if q.stateStore != nil {
		q.stateStore.Reset()
		q.stateStore = nil
	}
	return nil
}

var _ factory.IOidcProvider = (*QQ)(nil)

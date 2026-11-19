package qq

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/utils"
	"github.com/komari-monitor/komari/web/oauth/factory"
	"github.com/patrickmn/go-cache"
)

func (q *QQ) GetName() string {
	return "qq"
}

func (q *QQ) GetConfiguration() factory.Configuration {
	return &q.Addition
}

var qqHTTPClient = &http.Client{Timeout: 15 * time.Second}

const maxQQResponseBytes = 1 << 20

func (q *QQ) GetAuthorizationURL(redirectURI string) (string, string) {
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

	// 向聚合登录平台发送请求
	resp, err := qqHTTPClient.Get(requestURL)
	if err != nil {
		// 如果请求失败，返回错误信息
		return "", state
	}
	defer resp.Body.Close()

	// 读取响应内容（限制大小防止内存耗尽）
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQQResponseBytes))
	if err != nil {
		return "", state
	}

	// 解析响应JSON
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		URL  string `json:"url"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return "", state
	}

	// 检查响应状态
	if result.Code != 0 {
		return "", state
	}

	q.stateMu.Lock()
	if q.stateCache != nil {
		q.stateCache.Set(state, true, cache.DefaultExpiration)
	}
	q.stateMu.Unlock()
	return result.URL, state
}

// consumeState atomically verifies and removes a pending OAuth state token so
// concurrent callbacks cannot replay the same state.
func (q *QQ) consumeState(state string) bool {
	if state == "" {
		return false
	}
	q.stateMu.Lock()
	defer q.stateMu.Unlock()
	if q.stateCache == nil {
		return false
	}
	if _, ok := q.stateCache.Get(state); !ok {
		return false
	}
	q.stateCache.Delete(state)
	return true
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
	if q.stateCache == nil {
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

	resp, err := qqHTTPClient.Get(callbackURL)
	if err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to get user info: %v", err)
	}
	defer resp.Body.Close()

	// 检查HTTP响应状态
	if resp.StatusCode != http.StatusOK {
		return factory.OidcCallback{}, fmt.Errorf("HTTP request failed with status code: %d", resp.StatusCode)
	}

	// 读取响应（限制大小）
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQQResponseBytes))
	if err != nil {
		return factory.OidcCallback{}, fmt.Errorf("failed to read response: %v", err)
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
		return factory.OidcCallback{}, fmt.Errorf("failed to parse callback response: %v, response body: %s", err, string(body))
	}

	// 检查返回状态码
	if result.Code != 0 {
		return factory.OidcCallback{}, fmt.Errorf("QQ login callback failed with code %d: %s", result.Code, result.Msg)
	}

	// 检查是否返回了用户唯一标识
	if result.SocialUid == "" {
		return factory.OidcCallback{}, fmt.Errorf("empty social_uid returned, full response: %s", string(body))
	}

	// 返回用户唯一标识
	return factory.OidcCallback{UserId: result.SocialUid}, nil
}

func (q *QQ) Init() error {
	q.stateCache = cache.New(time.Minute*5, time.Minute*10)
	return nil
}

func (q *QQ) Destroy() error {
	q.stateCache.Flush()
	return nil
}

var _ factory.IOidcProvider = (*QQ)(nil)

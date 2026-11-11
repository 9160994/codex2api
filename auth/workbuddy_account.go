package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// UpstreamWorkBuddy 是腾讯 WorkBuddy（CodeBuddy CLI 同源）订阅账号的 upstream_type。
//
// 该渠道上游只提供 OpenAI Chat Completions 兼容接口（SSE），因此网关侧只对
// /v1/chat/completions 和 /v1/models 开放；Responses / Anthropic Messages 的
// 协议翻译不在本期范围（见 proxy/workbuddy_upstream.go 的显式拒绝）。
//
// 账号凭据存放在 accounts.credentials：
//
//	upstream_type = "workbuddy"
//	api_key       = accessToken（会过期，由刷新任务/惰性刷新替换）
//	models        = 该账号可用模型白名单
//	base_url      = 可选，缺省按 realm 取 WorkBuddyBaseURLCN / WorkBuddyBaseURLGlobal
//	workbuddy_uid / workbuddy_refresh_token / workbuddy_expires_at / workbuddy_realm ...
const UpstreamWorkBuddy = "workbuddy"

const (
	WorkBuddyRealmCN     = "cn"
	WorkBuddyRealmGlobal = "global"

	WorkBuddyBaseURLCN     = "https://copilot.tencent.com"
	WorkBuddyBaseURLGlobal = "https://www.workbuddy.ai"

	WorkBuddyChatPath    = "/v2/chat/completions"
	WorkBuddyRefreshPath = "/v2/plugin/auth/token/refresh"
	WorkBuddyModelsPath  = "/console/enterprises/personal/models"

	WorkBuddyOriginCN     = "https://www.codebuddy.cn"
	WorkBuddyOriginGlobal = "https://www.workbuddy.ai"

	// WorkBuddyDefaultUserAgent 是官方 CLI / 插件在用的出站 UA。上游按 UA 做客户端
	// 归因，空值会让请求落到未知客户端的限流桶。
	WorkBuddyDefaultUserAgent = "CLI/2.63.2 CodeBuddy/2.63.2"

	WorkBuddyUIDCredentialKey          = "workbuddy_uid"
	WorkBuddyRefreshTokenCredentialKey = "workbuddy_refresh_token"
	WorkBuddyExpiresAtCredentialKey    = "workbuddy_expires_at"
	WorkBuddyRealmCredentialKey        = "workbuddy_realm"
	WorkBuddyNicknameCredentialKey     = "workbuddy_nickname"
	WorkBuddyEnterpriseCredentialKey   = "workbuddy_enterprise_id"
	WorkBuddyDomainCredentialKey       = "workbuddy_domain"
)

// WorkBuddyToken 是一次 token 刷新的结果。
type WorkBuddyToken struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// isWorkBuddyLocked 判断账号是否为 WorkBuddy 渠道。调用方必须已持有 a.mu。
// 判定只用 upstream_type，不要求凭据齐全：凭据缺失的账号仍要能在后台列表里
// 显示为「待补登录」，而不是被当成 Codex 账号混进调度。
func (a *Account) isWorkBuddyLocked() bool {
	if a == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(a.UpstreamType), UpstreamWorkBuddy)
}

// IsWorkBuddy 判断账号是否为 WorkBuddy 渠道账号。
func (a *Account) IsWorkBuddy() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isWorkBuddyLocked()
}

// WorkBuddyUID 返回上游账号 uid（上游 X-User-Id 头与设备指纹派生的依据）。
func (a *Account) WorkBuddyUID() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.AccountID)
}

// WorkBuddyDomain 返回账号所在域（cn / global），未知值按 cn 处理。
func (a *Account) WorkBuddyDomain() string {
	if a == nil {
		return WorkBuddyRealmCN
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return NormalizeWorkBuddyRealm(a.WorkBuddyRealm)
}

// WorkBuddyAccessToken 返回当前 access token（存于 APIKey 字段）。
func (a *Account) WorkBuddyAccessToken() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.APIKey)
}

// WorkBuddyBaseURL 返回该账号的上游 base：账号显式配置优先，否则按 realm 取默认。
func (a *Account) WorkBuddyBaseURL() string {
	if a == nil {
		return WorkBuddyBaseURLCN
	}
	a.mu.RLock()
	base := strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
	realm := a.WorkBuddyRealm
	a.mu.RUnlock()
	if base != "" {
		return base
	}
	if strings.EqualFold(strings.TrimSpace(realm), WorkBuddyRealmGlobal) {
		return WorkBuddyBaseURLGlobal
	}
	return WorkBuddyBaseURLCN
}

// WorkBuddyOrigin 返回该账号的 Origin / Referer base。
func (a *Account) WorkBuddyOrigin() string {
	if a != nil && a.WorkBuddyDomain() == WorkBuddyRealmGlobal {
		return WorkBuddyOriginGlobal
	}
	return WorkBuddyOriginCN
}

// WorkBuddyExpiresAt 返回 access token 的过期时刻（零值表示未知）。
func (a *Account) WorkBuddyExpiresAt() time.Time {
	if a == nil {
		return time.Time{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ExpiresAt
}

// WorkBuddyModels 返回该账号的模型白名单副本。
func (a *Account) WorkBuddyModels() []string {
	if a == nil {
		return []string{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return cloneStringSlice(a.Models)
}

// SetWorkBuddyToken 用刷新结果覆盖内存中的 access/refresh token 与过期时刻。
func (a *Account) SetWorkBuddyToken(accessToken, refreshToken string, expiresAt time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if token := strings.TrimSpace(accessToken); token != "" {
		a.APIKey = token
	}
	if token := strings.TrimSpace(refreshToken); token != "" {
		a.RefreshToken = token
	}
	if !expiresAt.IsZero() {
		a.ExpiresAt = expiresAt
	}
}

// SetWorkBuddyModels 覆盖内存中的模型白名单（后台编辑后即时生效）。
func (a *Account) SetWorkBuddyModels(models []string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Models = cloneStringSlice(models)
}

// WorkBuddyStableID 按 uid 派生稳定的设备标识（X-Machine-ID / X-Session-ID）。
// 与 wb2api 口径一致：同一账号跨重启固定、账号之间互异，避免多号被上游按
// 「设备指纹漂移」关联风控。
func WorkBuddyStableID(uid, purpose string) string {
	if strings.TrimSpace(uid) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18])
}

// NormalizeWorkBuddyRealm 归一 realm 取值，未知值按 cn。
func NormalizeWorkBuddyRealm(realm string) string {
	if strings.EqualFold(strings.TrimSpace(realm), WorkBuddyRealmGlobal) {
		return WorkBuddyRealmGlobal
	}
	return WorkBuddyRealmCN
}

// RefreshWorkBuddyToken 用 refresh token 换取新的 access token。
//
// 上游端点：POST {base}/v2/plugin/auth/token/refresh，鉴权头是 X-Refresh-Token
// （access token 只用于 chat / 模型目录，绝不带到这里之外）。
func RefreshWorkBuddyToken(ctx context.Context, account *Account, proxyURL string) (WorkBuddyToken, error) {
	if account == nil {
		return WorkBuddyToken{}, fmt.Errorf("account is nil")
	}
	refreshToken := strings.TrimSpace(account.RefreshToken)
	if refreshToken == "" {
		return WorkBuddyToken{}, fmt.Errorf("workbuddy 账号缺少 refresh token")
	}
	base := account.WorkBuddyBaseURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+WorkBuddyRefreshPath, nil)
	if err != nil {
		return WorkBuddyToken{}, err
	}
	ApplyWorkBuddyCommonHeaders(req, account)
	req.Header.Set("X-Refresh-Token", refreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")

	client, err := NewWorkBuddyHTTPClient(proxyURL, 30*time.Second)
	if err != nil {
		return WorkBuddyToken{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return WorkBuddyToken{}, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return WorkBuddyToken{}, readErr
	}
	if resp.StatusCode != http.StatusOK {
		return WorkBuddyToken{}, fmt.Errorf("workbuddy token 刷新失败: http %d: %s", resp.StatusCode, truncateForError(body))
	}
	var payload struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return WorkBuddyToken{}, fmt.Errorf("workbuddy token 刷新响应解析失败: %w", err)
	}
	if payload.Code != 0 {
		return WorkBuddyToken{}, fmt.Errorf("workbuddy token 刷新被拒绝: code=%d msg=%s", payload.Code, payload.Msg)
	}
	var data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if len(payload.Data) > 0 {
		if err := json.Unmarshal(payload.Data, &data); err != nil {
			return WorkBuddyToken{}, fmt.Errorf("workbuddy token 刷新响应解析失败: %w", err)
		}
	}
	if strings.TrimSpace(data.AccessToken) == "" {
		return WorkBuddyToken{}, fmt.Errorf("workbuddy token 刷新响应缺少 accessToken")
	}
	expiresIn := data.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	result := WorkBuddyToken{
		AccessToken:  strings.TrimSpace(data.AccessToken),
		RefreshToken: strings.TrimSpace(data.RefreshToken),
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second),
	}
	if result.RefreshToken == "" {
		result.RefreshToken = refreshToken
	}
	return result, nil
}

// ApplyWorkBuddyCommonHeaders 注入所有 WorkBuddy 出站共享的头族。
func ApplyWorkBuddyCommonHeaders(req *http.Request, account *Account) {
	if req == nil {
		return
	}
	uid := ""
	realm := WorkBuddyRealmCN
	if account != nil {
		uid = account.WorkBuddyUID()
		realm = account.WorkBuddyDomain()
	}
	origin := WorkBuddyOriginCN
	language := "zh-CN"
	if realm == WorkBuddyRealmGlobal {
		origin = WorkBuddyOriginGlobal
		language = "en-US"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", WorkBuddyUserAgent(account))
	req.Header.Set("X-CodeBuddy-Request", "1")
	// 上游按 X-Product 做「通道校验」：缺失/不符会 400 code=11128
	// 「Illegal API invocation from an unapproved channel / 请求被拦截，请重新发送」。
	// 官方 CLI 上报的渠道就是 SaaS。
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("Accept-Language", language)
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
		if machine := WorkBuddyStableID(uid, "machine"); machine != "" {
			req.Header.Set("X-Machine-ID", machine)
		}
		if session := WorkBuddyStableID(uid, "session"); session != "" {
			req.Header.Set("X-Session-ID", session)
		}
	}
}

// WorkBuddyUserAgent 返回账号的出站 UA：账号自定义头优先，其次内置 CLI UA。
func WorkBuddyUserAgent(account *Account) string {
	if account == nil {
		return WorkBuddyDefaultUserAgent
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	for key, value := range account.CustomHeaders {
		if strings.EqualFold(strings.TrimSpace(key), "User-Agent") && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return WorkBuddyDefaultUserAgent
}

// NewWorkBuddyHTTPClient 构造 WorkBuddy 上游客户端（支持账号代理出口）。
func NewWorkBuddyHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	if err := ConfigureTransportProxy(transport, proxyURL, nil); err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

func truncateForError(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		return text[:300] + "..."
	}
	return text
}

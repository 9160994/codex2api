package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
)

// WorkBuddy 渠道管理接口。
//
// 账号凭据与所有渠道一样落在 accounts.credentials，platform / upstream_type 均为
// "workbuddy"。access token 存 api_key（会过期），refresh token 存
// workbuddy_refresh_token，uid 存 workbuddy_uid；支持从 wb2api 的
// auths/workbuddy-<uid>.json 直接批量导入。

type workBuddyAccountItem struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	UID       string   `json:"uid"`
	Nickname  string   `json:"nickname"`
	Realm     string   `json:"realm"`
	Status    string   `json:"status"`
	Enabled   bool     `json:"enabled"`
	Models    []string `json:"models"`
	ExpiresAt string   `json:"expires_at"`
	LastError string   `json:"last_error"`
	ProxyURL  string   `json:"proxy_url"`
}

type workBuddyAccountRequest struct {
	Name         string   `json:"name"`
	UID          string   `json:"uid"`
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	ExpiresAt    string   `json:"expires_at"`
	Realm        string   `json:"realm"`
	Nickname     string   `json:"nickname"`
	EnterpriseID string   `json:"enterprise_id"`
	Models       []string `json:"models"`
	ProxyURL     string   `json:"proxy_url"`
}

type workBuddyImportRequest struct {
	Items []workBuddyAccountRequest `json:"items"`
	Text  string                    `json:"text"`
}

// ListWorkBuddyAccounts 返回 workbuddy 渠道账号列表。
func (h *Handler) ListWorkBuddyAccounts(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	rows, err := h.db.ListActiveByChannel(ctx, database.UpstreamChannelWorkBuddy)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	items := make([]workBuddyAccountItem, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		item := workBuddyAccountItem{
			ID:        row.ID,
			Name:      row.Name,
			UID:       row.GetCredential(auth.WorkBuddyUIDCredentialKey),
			Nickname:  row.GetCredential(auth.WorkBuddyNicknameCredentialKey),
			Realm:     auth.NormalizeWorkBuddyRealm(row.GetCredential(auth.WorkBuddyRealmCredentialKey)),
			Status:    strings.ToLower(strings.TrimSpace(row.Status)),
			Enabled:   row.Enabled,
			Models:    row.GetCredentialStringSlice("models"),
			ExpiresAt: row.GetCredential(auth.WorkBuddyExpiresAtCredentialKey),
			LastError: row.ErrorMessage,
			ProxyURL:  row.ProxyURL,
		}
		if item.Models == nil {
			item.Models = []string{}
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"accounts": items})
}

// AddWorkBuddyAccount 新增（或按 uid 覆盖）一个 WorkBuddy 账号。
func (h *Handler) AddWorkBuddyAccount(c *gin.Context) {
	var req workBuddyAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	account, err := h.persistWorkBuddyAccount(c.Request.Context(), req)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	security.SecurityAuditLog("WORKBUDDY_ACCOUNT_ADDED", fmt.Sprintf("account_id=%d uid=%s ip=%s", account.ID(), req.UID, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{"message": "成功添加 WorkBuddy 账号", "id": account.ID})
}

// ImportWorkBuddyAccounts 批量导入：支持 {items:[...]}、wb2api auths 单文件或数组。
func (h *Handler) ImportWorkBuddyAccounts(c *gin.Context) {
	var req workBuddyImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	items := req.Items
	if len(items) == 0 && strings.TrimSpace(req.Text) != "" {
		parsed, err := parseWorkBuddyImportText(req.Text)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		items = parsed
	}
	if len(items) == 0 {
		writeError(c, http.StatusBadRequest, "没有可导入的账号")
		return
	}
	imported := make([]int64, 0, len(items))
	failed := make([]string, 0)
	for index, item := range items {
		account, err := h.persistWorkBuddyAccount(c.Request.Context(), item)
		if err != nil {
			failed = append(failed, fmt.Sprintf("#%d %s: %v", index+1, item.UID, err))
			continue
		}
		imported = append(imported, account.ID())
	}
	c.JSON(http.StatusOK, gin.H{
		"message":  fmt.Sprintf("导入完成：成功 %d，失败 %d", len(imported), len(failed)),
		"imported": len(imported),
		"failed":   len(failed),
		"errors":   failed,
	})
}

// DeleteWorkBuddyAccounts 软删除指定账号。
func (h *Handler) DeleteWorkBuddyAccounts(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要删除的账号 id")
		return
	}
	deleted := make([]int64, 0, len(req.IDs))
	for _, id := range req.IDs {
		if id <= 0 {
			continue
		}
		row, err := h.db.GetAccountByID(c.Request.Context(), id)
		if err != nil || row == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamWorkBuddy) {
			continue
		}
		if err := h.db.SoftDeleteAccount(c.Request.Context(), id); err != nil {
			continue
		}
		h.store.RemoveAccount(id)
		deleted = append(deleted, id)
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("已删除 %d 个账号", len(deleted)), "deleted": deleted})
}

// UpdateWorkBuddyAccount 更新名称 / 代理 / 凭据 / 模型。
func (h *Handler) UpdateWorkBuddyAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "账号 id 无效")
		return
	}
	var req struct {
		Name         *string   `json:"name"`
		Models       *[]string `json:"models"`
		ProxyURL     *string   `json:"proxy_url"`
		AccessToken  *string   `json:"access_token"`
		RefreshToken *string   `json:"refresh_token"`
		ExpiresAt    *string   `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	updates := map[string]interface{}{}
	if req.Name != nil {
		updates["name"] = security.SanitizeInput(strings.TrimSpace(*req.Name))
	}
	if req.Models != nil {
		updates["models"] = normalizeWorkBuddyModels(*req.Models)
	}
	if req.ProxyURL != nil {
		proxyURL := security.SanitizeInput(strings.TrimSpace(*req.ProxyURL))
		if err := security.ValidateProxyURL(proxyURL); err != nil {
			writeError(c, http.StatusBadRequest, "代理URL无效")
			return
		}
		updates["proxy_url"] = proxyURL
	}
	if req.AccessToken != nil {
		updates["api_key"] = strings.TrimSpace(*req.AccessToken)
	}
	if req.RefreshToken != nil {
		updates[auth.WorkBuddyRefreshTokenCredentialKey] = strings.TrimSpace(*req.RefreshToken)
	}
	if req.ExpiresAt != nil {
		updates[auth.WorkBuddyExpiresAtCredentialKey] = strings.TrimSpace(*req.ExpiresAt)
	}
	if len(updates) == 0 {
		writeError(c, http.StatusBadRequest, "没有需要更新的字段")
		return
	}
	if err := h.db.UpdateCredentials(c.Request.Context(), id, updates); err != nil {
		writeInternalError(c, err)
		return
	}
	h.applyWorkBuddyRuntimeUpdates(id, updates)
	c.JSON(http.StatusOK, gin.H{"message": "已更新"})
}

// TestWorkBuddyAccount 用账号真实凭据打一次最小 chat 请求，验证凭据与出口是否可用。
func (h *Handler) TestWorkBuddyAccount(c *gin.Context) {
	account, ok := h.workBuddyAccountFromPath(c)
	if !ok {
		return
	}
	models := account.WorkBuddyModels()
	model := ""
	for _, candidate := range models {
		if strings.TrimSpace(candidate) != "" {
			model = strings.TrimSpace(candidate)
			break
		}
	}
	if model == "" {
		model = strings.TrimSpace(c.Query("model"))
	}
	if model == "" {
		writeError(c, http.StatusBadRequest, "该账号还没有模型目录，请先同步模型或手工填写一个模型")
		return
	}
	body := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"ping"}],"stream":false,"max_tokens":8}`, model))
	started := time.Now()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	resp, err := proxy.ExecuteWorkBuddyChatRequest(ctx, account, body, "", nil)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		h.markWorkBuddyFailure(account, err.Error())
		c.JSON(http.StatusOK, gin.H{"ok": false, "latency_ms": latency, "message": err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := readWorkBuddyBody(resp.Body, 4096)
		message := fmt.Sprintf("上游返回 %d：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
		if proxy.IsWorkBuddySessionDeadError(raw) {
			message += "（refresh token 已失效，需要在浏览器重新登录授权后重新导入凭据）"
		}
		h.markWorkBuddyFailure(account, message)
		c.JSON(http.StatusOK, gin.H{"ok": false, "latency_ms": latency, "message": message})
		return
	}
	raw := readWorkBuddyBody(resp.Body, 1<<20)
	c.JSON(http.StatusOK, gin.H{
		"ok":         true,
		"latency_ms": latency,
		"message":    "连通正常",
		"model":      model,
		"preview":    strings.TrimSpace(string(raw))[:minInt(len(strings.TrimSpace(string(raw))), 200)],
	})
}

// RefreshWorkBuddyAccessToken 强制用 refresh token 换一次新 access token。
func (h *Handler) RefreshWorkBuddyAccessToken(c *gin.Context) {
	account, ok := h.workBuddyAccountFromPath(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	token, err := auth.RefreshWorkBuddyToken(ctx, account, account.ProxyURL)
	if err != nil {
		message := err.Error()
		if proxy.IsWorkBuddySessionDeadError([]byte(message)) {
			message += "（需要重新登录授权）"
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": message})
		return
	}
	account.SetWorkBuddyToken(token.AccessToken, token.RefreshToken, token.ExpiresAt)
	updates := map[string]interface{}{
		"api_key":                               token.AccessToken,
		auth.WorkBuddyRefreshTokenCredentialKey: token.RefreshToken,
		auth.WorkBuddyExpiresAtCredentialKey:    token.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if err := h.db.UpdateCredentials(ctx, account.ID(), updates); err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "expires_at": token.ExpiresAt.UTC().Format(time.RFC3339)})
}

// FetchWorkBuddyModels 拉取账号的上游模型目录并写回账号凭据。
func (h *Handler) FetchWorkBuddyModels(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	_ = c.ShouldBindJSON(&req)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	targets := make([]*auth.Account, 0, len(req.IDs))
	if len(req.IDs) == 0 {
		for _, account := range h.store.Accounts() {
			if account != nil && account.IsWorkBuddy() {
				targets = append(targets, account)
			}
		}
	} else {
		for _, id := range req.IDs {
			if account := h.store.FindByID(id); account != nil && account.IsWorkBuddy() {
				targets = append(targets, account)
			}
		}
	}
	if len(targets) == 0 {
		writeError(c, http.StatusBadRequest, "没有可同步的 WorkBuddy 账号")
		return
	}
	merged := make([]string, 0, 16)
	seen := map[string]struct{}{}
	failures := make([]string, 0)
	for _, account := range targets {
		models, err := proxy.FetchWorkBuddyModelIDs(ctx, account, account.ProxyURL)
		if err != nil {
			failures = append(failures, fmt.Sprintf("账号 %d: %v", account.ID(), err))
			continue
		}
		if err := h.db.UpdateCredentials(ctx, account.ID(), map[string]interface{}{"models": models}); err != nil {
			failures = append(failures, fmt.Sprintf("账号 %d 落库失败: %v", account.ID(), err))
			continue
		}
		for _, model := range models {
			key := strings.ToLower(strings.TrimSpace(model))
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, model)
		}
	}
	c.JSON(http.StatusOK, gin.H{"models": merged, "errors": failures})
}

func (h *Handler) workBuddyAccountFromPath(c *gin.Context) (*auth.Account, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "账号 id 无效")
		return nil, false
	}
	account := h.store.FindByID(id)
	if account == nil || !account.IsWorkBuddy() {
		writeError(c, http.StatusNotFound, "WorkBuddy 账号不存在")
		return nil, false
	}
	return account, true
}

func (h *Handler) markWorkBuddyFailure(account *auth.Account, message string) {
	account.SetWorkBuddyToken("", "", time.Time{})
}

func (h *Handler) applyWorkBuddyRuntimeUpdates(id int64, updates map[string]interface{}) {
	account := h.store.FindByID(id)
	if account == nil || !account.IsWorkBuddy() {
		return
	}
	if models, ok := updates["models"].([]string); ok {
		account.SetWorkBuddyModels(models)
	}
	if token, ok := updates["api_key"].(string); ok && strings.TrimSpace(token) != "" {
		expires := account.WorkBuddyExpiresAt()
		account.SetWorkBuddyToken(token, "", expires)
	}
}

// persistWorkBuddyAccount 校验并写入一个账号（按 uid 去重覆盖）。
func (h *Handler) persistWorkBuddyAccount(ctx context.Context, req workBuddyAccountRequest) (*auth.Account, error) {
	uid := strings.TrimSpace(req.UID)
	accessToken := strings.TrimSpace(req.AccessToken)
	refreshToken := strings.TrimSpace(req.RefreshToken)
	if uid == "" {
		return nil, fmt.Errorf("uid 不能为空")
	}
	if accessToken == "" && refreshToken == "" {
		return nil, fmt.Errorf("access_token 与 refresh_token 至少需要一个")
	}
	name := security.SanitizeInput(strings.TrimSpace(req.Name))
	if name == "" {
		name = "workbuddy-" + trimUID(uid)
	}
	proxyURL := security.SanitizeInput(strings.TrimSpace(req.ProxyURL))
	if err := security.ValidateProxyURL(proxyURL); err != nil {
		return nil, fmt.Errorf("代理URL无效")
	}
	realm := auth.NormalizeWorkBuddyRealm(req.Realm)
	baseURL := auth.WorkBuddyBaseURLCN
	if realm == auth.WorkBuddyRealmGlobal {
		baseURL = auth.WorkBuddyBaseURLGlobal
	}
	expiresAt := strings.TrimSpace(req.ExpiresAt)
	if expiresAt == "" {
		expiresAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	}
	credentials := map[string]interface{}{
		"upstream_type":                         auth.UpstreamWorkBuddy,
		"base_url":                              baseURL,
		"api_key":                               accessToken,
		"models":                                normalizeWorkBuddyModels(req.Models),
		auth.WorkBuddyUIDCredentialKey:          uid,
		auth.WorkBuddyRefreshTokenCredentialKey: refreshToken,
		auth.WorkBuddyExpiresAtCredentialKey:    expiresAt,
		auth.WorkBuddyRealmCredentialKey:        realm,
		auth.WorkBuddyNicknameCredentialKey:     strings.TrimSpace(req.Nickname),
		auth.WorkBuddyEnterpriseCredentialKey:   strings.TrimSpace(req.EnterpriseID),
		"plan_type":                             "workbuddy",
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	existing, err := h.db.ListActiveByChannel(ctx, database.UpstreamChannelWorkBuddy)
	if err == nil {
		for _, row := range existing {
			if row != nil && strings.EqualFold(strings.TrimSpace(row.GetCredential(auth.WorkBuddyUIDCredentialKey)), uid) {
				if err := h.db.UpdateCredentials(ctx, row.ID, credentials); err != nil {
					return nil, err
				}
				account := h.store.FindByID(row.ID)
				if account == nil {
					return &auth.Account{DBID: row.ID}, nil
				}
				account.SetWorkBuddyToken(accessToken, refreshToken, parseWorkBuddyExpiresAt(expiresAt))
				account.SetWorkBuddyModels(normalizeWorkBuddyModels(req.Models))
				return account, nil
			}
		}
	}

	id, err := h.db.InsertAccountWithUpstream(ctx, name, database.UpstreamChannelWorkBuddy, "oauth", credentials, proxyURL)
	if err != nil {
		return nil, err
	}
	h.db.InsertAccountEventAsync(id, "added", "manual_workbuddy")
	account := &auth.Account{
		DBID:           id,
		UpstreamType:   auth.UpstreamWorkBuddy,
		BaseURL:        baseURL,
		APIKey:         accessToken,
		RefreshToken:   refreshToken,
		Models:         normalizeWorkBuddyModels(req.Models),
		AccountID:      uid,
		Email:          strings.TrimSpace(req.Nickname),
		ProxyURL:       proxyURL,
		WorkBuddyRealm: realm,
		PlanType:       "workbuddy",
		HealthTier:     auth.HealthTierHealthy,
		ExpiresAt:      parseWorkBuddyExpiresAt(expiresAt),
	}
	h.store.AddAccount(account)
	return account, nil
}

// parseWorkBuddyImportText 解析 wb2api 的 auths/*.json：单文件、数组或 {items:[...]}。
func parseWorkBuddyImportText(text string) ([]workBuddyAccountRequest, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, fmt.Errorf("导入内容为空")
	}
	if strings.HasPrefix(trimmed, "[") {
		var raws []map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &raws); err != nil {
			return nil, fmt.Errorf("导入 JSON 解析失败: %w", err)
		}
		items := make([]workBuddyAccountRequest, 0, len(raws))
		for _, raw := range raws {
			items = append(items, workBuddyRequestFromRaw(raw))
		}
		return items, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, fmt.Errorf("导入 JSON 解析失败: %w", err)
	}
	if itemsRaw, ok := raw["items"]; ok {
		var raws []map[string]json.RawMessage
		if err := json.Unmarshal(itemsRaw, &raws); err != nil {
			return nil, fmt.Errorf("导入 JSON 解析失败: %w", err)
		}
		items := make([]workBuddyAccountRequest, 0, len(raws))
		for _, item := range raws {
			items = append(items, workBuddyRequestFromRaw(item))
		}
		return items, nil
	}
	return []workBuddyAccountRequest{workBuddyRequestFromRaw(raw)}, nil
}

// workBuddyRequestFromRaw 兼容两种形态：
//  1. wb2api 文件：{account:{uid,nickname,enterpriseId}, auth:{accessToken,refreshToken,expiresAt,realm}}
//  2. 扁平对象：{uid,access_token,refresh_token,...}
func workBuddyRequestFromRaw(raw map[string]json.RawMessage) workBuddyAccountRequest {
	var item workBuddyAccountRequest
	var account struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		EnterpriseID string `json:"enterpriseId"`
	}
	var token struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		Realm        string `json:"realm"`
	}
	if node, ok := raw["account"]; ok {
		_ = json.Unmarshal(node, &account)
	}
	if node, ok := raw["auth"]; ok {
		_ = json.Unmarshal(node, &token)
	}
	item.UID = account.UID
	item.Nickname = account.Nickname
	item.EnterpriseID = account.EnterpriseID
	item.AccessToken = token.AccessToken
	item.RefreshToken = token.RefreshToken
	item.Realm = token.Realm
	if token.ExpiresAt > 0 {
		item.ExpiresAt = time.Unix(token.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	// 扁平字段覆盖（前端表单直传）
	var flat workBuddyAccountRequest
	if err := json.Unmarshal(mustMarshal(raw), &flat); err == nil {
		if flat.UID != "" {
			item.UID = flat.UID
		}
		if flat.Nickname != "" {
			item.Nickname = flat.Nickname
		}
		if flat.AccessToken != "" {
			item.AccessToken = flat.AccessToken
		}
		if flat.RefreshToken != "" {
			item.RefreshToken = flat.RefreshToken
		}
		if flat.Realm != "" {
			item.Realm = flat.Realm
		}
		if flat.ExpiresAt != "" {
			item.ExpiresAt = flat.ExpiresAt
		}
		if len(flat.Models) > 0 {
			item.Models = flat.Models
		}
		if flat.ProxyURL != "" {
			item.ProxyURL = flat.ProxyURL
		}
		if flat.Name != "" {
			item.Name = flat.Name
		}
	}
	return item
}

func mustMarshal(raw map[string]json.RawMessage) []byte {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

func normalizeWorkBuddyModels(models []string) []string {
	result := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, model)
	}
	return result
}

func parseWorkBuddyExpiresAt(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts
	}
	if unix, err := strconv.ParseInt(raw, 10, 64); err == nil && unix > 0 {
		return time.Unix(unix, 0)
	}
	return time.Time{}
}

func readWorkBuddyBody(body io.Reader, limit int64) []byte {
	raw, err := io.ReadAll(io.LimitReader(body, limit))
	if err != nil {
		return nil
	}
	return raw
}

func trimUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}

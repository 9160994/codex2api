package proxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// workBuddyChatCompletions 是 workbuddy 渠道专用入口。
//
// 为什么不复用通用 ChatCompletions 主链：那条链的响应侧是「Codex Responses →
// OpenAI Chat」的翻译器（TranslateStreamChunk 只认 response.* 事件），而 WorkBuddy
// 上游本身就是 OpenAI Chat Completions 形态。硬塞进去的结果是每个 delta 都被丢弃，
// 表现为 200 但正文空、或 upstream_stream_break。这里直接做 chat→chat 转发。
func (h *Handler) workBuddyChatCompletions(c *gin.Context) {
	started := time.Now()
	rawBody, err := readRawRequestBody(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Failed to read request body", "type": "invalid_request_error"}})
		return
	}
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/chat/completions", gjson.GetBytes(rawBody, "model").String()) {
		return
	}
	model := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "model 不能为空", "type": "invalid_request_error"}})
		return
	}
	isStream := gjson.GetBytes(rawBody, "stream").Bool()

	account := h.nextWorkBuddyAccount(model)
	if account == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
			"message": "WorkBuddy 渠道暂无可用账号（全部冷却/未配置该模型），请检查账号状态或重新授权",
			"type":    "server_error",
			"code":    ErrorCodeNoAvailableAccount,
		}})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Minute)
	defer cancel()

	h.ensureWorkBuddyToken(ctx, account, account.ProxyURL)

	// 上游风控的 11128（"Illegal API invocation from an unapproved channel"，
	// displayMsg 明确写着「请求被拦截，请重新发送」）是可重试的瞬时拦截，
	// 同一账号同一模型下一次往往就过。这里内部重试，避免把偶发拦截直接甩给客户端。
	var resp *http.Response
	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// 逐级降噪：1 原样，2 去掉温度/top_p/penalties 等装饰参数，3 再去掉 thinking。
		level := 0
		switch attempt {
		case 2:
			level = 2
		case 3:
			level = 3
		}
		resp, err = ExecuteWorkBuddyChatRequest(ctx, account, rawBody, account.ProxyURL, c.Request.Header, level)
		if err != nil || resp.StatusCode < http.StatusBadRequest {
			if attempt > 1 {
				log.Printf("[WorkBuddy] 第 %d 次尝试（level=%d）成功", attempt, level)
			}
			break
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		if !workBuddyRetryableBlock(body) || attempt == maxAttempts {
			log.Printf("[WorkBuddy] upstream %d account=%d attempt=%d body=%s", resp.StatusCode, account.ID(), attempt, truncateBodyForLog(body))
			h.applyWorkBuddyCooldown(account, resp.StatusCode)
			message := WorkBuddyUpstreamErrorMessage(body) + workBuddyBlockHint(body)
			c.JSON(resp.StatusCode, gin.H{"error": gin.H{"message": message, "type": ErrorTypeUpstreamError, "code": "upstream_" + strconv.Itoa(resp.StatusCode)}})
			return
		}
		log.Printf("[WorkBuddy] 上游风控拦截(11128)，%dms 后降噪重试 (attempt %d/%d)", 300*attempt, attempt, maxAttempts)
		select {
		case <-time.After(time.Duration(300*attempt) * time.Millisecond):
		case <-ctx.Done():
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": gin.H{"message": "上游超时", "type": ErrorTypeUpstreamError}})
			return
		}
	}
	if err != nil {
		log.Printf("[WorkBuddy] 请求失败 account=%d: %v", account.ID(), err)
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": err.Error(), "type": ErrorTypeUpstreamError, "code": "workbuddy_upstream_error"}})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		message := WorkBuddyUpstreamErrorMessage(body) + workBuddyBlockHint(body)
		log.Printf("[WorkBuddy] upstream %d account=%d body=%s", resp.StatusCode, account.ID(), truncateBodyForLog(body))
		h.applyWorkBuddyCooldown(account, resp.StatusCode)
		c.JSON(resp.StatusCode, gin.H{"error": gin.H{"message": message, "type": ErrorTypeUpstreamError, "code": "upstream_" + strconv.Itoa(resp.StatusCode)}})
		return
	}

	if !isStream {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if readErr != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": readErr.Error(), "type": ErrorTypeUpstreamError}})
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", body)
		prompt, completion, total, reasoning := workBuddyUsageFromJSON(body)
		h.logWorkBuddyUsage(c, account, model, http.StatusOK, started, prompt, completion, total, reasoning)
		log.Printf("[WorkBuddy] 非流式完成 account=%d model=%s %dms bytes=%d", account.ID(), model, time.Since(started).Milliseconds(), len(body))
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	flusher, _ := c.Writer.(http.Flusher)
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	usageLine := []byte(nil)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, writeErr := c.Writer.Write(line); writeErr != nil {
				return
			}
			if bytes.Contains(line, []byte("\"usage\":")) && !bytes.Contains(line, []byte("\"usage\":null")) {
				usageLine = append(usageLine[:0], line...)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}
	if len(usageLine) > 0 {
		prompt, completion, total, reasoning := workBuddyUsageFromJSON(usageLine)
		h.logWorkBuddyUsage(c, account, model, http.StatusOK, started, prompt, completion, total, reasoning)
	}
	log.Printf("[WorkBuddy] 流式完成 account=%d model=%s %dms", account.ID(), model, time.Since(started).Milliseconds())
}

// nextWorkBuddyAccount 选一个能服务该模型的 WorkBuddy 账号：
// 未冷却、凭据可用、模型在白名单内（白名单为空表示全部放行）。
func (h *Handler) nextWorkBuddyAccount(model string) *auth.Account {
	if h == nil || h.store == nil {
		return nil
	}
	now := time.Now()
	for _, account := range h.store.Accounts() {
		if account == nil || !account.IsWorkBuddy() {
			continue
		}
		switch account.RuntimeStatus() {
		case "unauthorized", "banned", "error":
			continue
		}
		if until := account.WorkBuddyCooldownUntil(); !until.IsZero() && now.Before(until) {
			continue
		}
		if !account.SupportsWorkBuddyModel(model) {
			continue
		}
		if account.WorkBuddyAccessToken() == "" && strings.TrimSpace(account.RefreshToken) == "" {
			continue
		}
		return account
	}
	return nil
}

// applyWorkBuddyCooldown 上游错误 → 账号冷却（只处理明确的账号级状态）。
func (h *Handler) applyWorkBuddyCooldown(account *auth.Account, status int) {
	if account == nil {
		return
	}
	if status == http.StatusUnauthorized || status == http.StatusTooManyRequests {
		account.MarkWorkBuddyCooldown(time.Now().Add(10*time.Minute), "workbuddy_upstream_"+strconv.Itoa(status))
	}
}

// workBuddyUsageFromJSON 从响应体/最后一帧里取用量。
func workBuddyUsageFromJSON(body []byte) (prompt, completion, total, reasoning int) {
	prompt = int(gjson.GetBytes(body, "usage.prompt_tokens").Int())
	completion = int(gjson.GetBytes(body, "usage.completion_tokens").Int())
	total = int(gjson.GetBytes(body, "usage.total_tokens").Int())
	reasoning = int(gjson.GetBytes(body, "usage.completion_tokens_details.reasoning_tokens").Int())
	return
}

// logWorkBuddyUsage 写一条用量日志（与管理台其它渠道同表，便于统计/计费）。
func (h *Handler) logWorkBuddyUsage(c *gin.Context, account *auth.Account, model string, status int, started time.Time, prompt, completion, total, reasoning int) {
	if h == nil || h.db == nil || account == nil {
		return
	}
	h.logUsage(&database.UsageLogInput{
		AccountID:        account.ID(),
		Channel:          database.UpstreamChannelWorkBuddy,
		Endpoint:         "/v1/chat/completions",
		Model:            model,
		EffectiveModel:   model,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
		InputTokens:      prompt,
		OutputTokens:     completion,
		ReasoningTokens:  reasoning,
		StatusCode:       status,
		DurationMs:       int(time.Since(started).Milliseconds()),
		ClientUserAgent:  c.Request.UserAgent(),
		ClientIP:         c.ClientIP(),
	})
}

// workBuddyRetryableBlock 判断上游错误是否可以立刻重试。
// 11128 是上游风控的瞬时拦截（displayMsg: 请求被拦截，请重新发送），重发通常即可通过。
func workBuddyRetryableBlock(body []byte) bool {
	return gjson.GetBytes(body, "code").Int() == 11128
}

// workBuddyBlockHint 给风控类拦截补一句可执行提示，避免用户误判成账号/额度问题。
func workBuddyBlockHint(body []byte) string {
	if gjson.GetBytes(body, "code").Int() != 11128 {
		return ""
	}
	return "（上游风控瞬时拦截：网关已自动重发 3 次仍被拦，稍后再试或降低并发即可，与账号额度无关）"
}

package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// WorkBuddy 每日签到。
//
// 上游端点（实测 CN/global 同形，CN 带 /v2 前缀）：
//
//	POST {base}/v2/billing/meter/daily-checkin   body: {}
//
// 响应信封 {code,msg,data}：
//   - code=0            签到成功
//   - code=10001/14001  「今天已签到，请明天再来」（HTTP 400，幂等拒绝，不算失败）
//
// 鉴权与 chat 一致：Bearer access token + X-User-Id / X-Domain / X-CodeBuddy-Request。
// 签到属低频维护调用，瞬时 5xx 做有界重试，业务拒绝不重试。

const (
	// WorkBuddyCheckinPathCN 是 CN 域的签到路径。
	WorkBuddyCheckinPathCN = "/v2/billing/meter/daily-checkin"
	// WorkBuddyCheckinPathGlobal 是 global 域首选路径（国际版无 /v2 前缀）。
	WorkBuddyCheckinPathGlobal = "/billing/meter/daily-checkin"
	// WorkBuddyCheckinPathGlobalV2 是 global 域的兜底路径。
	WorkBuddyCheckinPathGlobalV2 = "/v2/billing/meter/daily-checkin"
)

// ErrWorkBuddyAlreadyCheckedIn 表示今天已经签到过（上游幂等拒绝）。
var ErrWorkBuddyAlreadyCheckedIn = errors.New("workbuddy: 今天已签到")

// workBuddyAlreadyCheckinMarkers 用于识别「已签到」类业务拒绝。
// 上游实测 code 有 10001/14001 两种，文案含「已签到」；这里按 code 与文案双判，
// 避免上游换码后把幂等拒绝误报成失败。
var workBuddyAlreadyCheckinMarkers = []string{"已签到", "already", "already checked"}

// WorkBuddyCheckinResult 是一次签到的结果。
type WorkBuddyCheckinResult struct {
	// Checked 为 true 表示本次调用真的完成了签到（而非今天已签过）。
	Checked bool
	// Already 为 true 表示今天已经签到过（幂等）。
	Already bool
	// Code / Msg 是上游原始业务码与文案，便于排障。
	Code int
	Msg  string
}

// WorkBuddyCheckinPaths 按 realm 返回签到路径候选序列。
//
// global 先试无 /v2 前缀，404 时回退到有前缀（上游新旧路径分叉）；CN 只有带前缀的
// 一条。该差异化与 billing/meter 族其他端点保持一致。
func WorkBuddyCheckinPaths(realm string) []string {
	if NormalizeWorkBuddyRealm(realm) == WorkBuddyRealmGlobal {
		return []string{WorkBuddyCheckinPathGlobal, WorkBuddyCheckinPathGlobalV2}
	}
	return []string{WorkBuddyCheckinPathCN}
}

// IsWorkBuddyAlreadyCheckin 判断上游错误是否为「今天已签到」。
func IsWorkBuddyAlreadyCheckin(msg string) bool {
	lower := strings.ToLower(msg)
	for _, marker := range workBuddyAlreadyCheckinMarkers {
		if strings.Contains(msg, marker) || strings.Contains(lower, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

// DailyCheckinWorkBuddy 对单个账号执行一次签到。
//
// 调用方需保证 account.RefreshToken 可用（token 由上层负责刷新后落在 account 上）。
// 返回值：
//   - result.Already 为 true 时 err 为 nil（幂等成功）
//   - 其他失败返回 err
func DailyCheckinWorkBuddy(ctx context.Context, account *Account, proxyURL string) (WorkBuddyCheckinResult, error) {
	if account == nil {
		return WorkBuddyCheckinResult{}, fmt.Errorf("workbuddy: account is nil")
	}
	accessToken := strings.TrimSpace(account.WorkBuddyAccessToken())
	if accessToken == "" {
		return WorkBuddyCheckinResult{}, fmt.Errorf("workbuddy: 账号缺少 access token，请重新登录授权")
	}
	base := account.WorkBuddyBaseURL()
	if strings.TrimSpace(base) == "" {
		return WorkBuddyCheckinResult{}, fmt.Errorf("workbuddy: 账号缺少 base url")
	}
	paths := WorkBuddyCheckinPaths(account.WorkBuddyRealm)
	client, err := NewWorkBuddyHTTPClient(proxyURL, 30*time.Second)
	if err != nil {
		return WorkBuddyCheckinResult{}, err
	}

	var lastErr error
	for i, path := range paths {
		result, status, err := workBuddyCheckinOnce(ctx, client, account, base+path, accessToken)
		if err == nil {
			return result, nil
		}
		lastErr = err
		// 仅 404 换路径（路径不存在才值得回退）；其他错误直接返回。
		if status != http.StatusNotFound || i == len(paths)-1 {
			return WorkBuddyCheckinResult{}, err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("workbuddy: 签到失败")
	}
	return WorkBuddyCheckinResult{}, lastErr
}

// workBuddyCheckinOnce 发一次签到请求，返回结果、HTTP 状态码与错误。
func workBuddyCheckinOnce(ctx context.Context, client *http.Client, account *Account, url, accessToken string) (WorkBuddyCheckinResult, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return WorkBuddyCheckinResult{}, 0, err
	}
	ApplyWorkBuddyCommonHeaders(req, account)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return WorkBuddyCheckinResult{}, 0, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return WorkBuddyCheckinResult{}, resp.StatusCode, readErr
	}

	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	parseErr := json.Unmarshal(body, &payload)
	// 上游对「已签到」返回 HTTP 400 + 业务码，所以先看业务码再看 HTTP 状态。
	if parseErr == nil {
		if payload.Code == 0 && resp.StatusCode == http.StatusOK {
			return WorkBuddyCheckinResult{Checked: true, Code: payload.Code, Msg: payload.Msg}, resp.StatusCode, nil
		}
		// 已签到判定要放在状态码判断之前：它同样是 HTTP 400+code!=0，但不是故障。
		if IsWorkBuddyAlreadyCheckin(payload.Msg) {
			return WorkBuddyCheckinResult{Already: true, Code: payload.Code, Msg: payload.Msg}, resp.StatusCode, nil
		}
		// HTTP 5xx 优先按传输层故障处理：上游在 500 时也会回 code=10000 的业务信封，
		// 若当成业务拒绝就丢掉了可重试语义（见 TestWorkBuddyCheckinWithRetryRecovers）。
		if resp.StatusCode >= http.StatusInternalServerError {
			return WorkBuddyCheckinResult{}, resp.StatusCode, fmt.Errorf(
				"workbuddy 签到失败: http %d: %s", resp.StatusCode, truncateForError(body))
		}
		if payload.Code != 0 {
			return WorkBuddyCheckinResult{}, resp.StatusCode, fmt.Errorf(
				"workbuddy 签到被拒绝: code=%d msg=%s", payload.Code, truncateForError(body))
		}
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return WorkBuddyCheckinResult{}, resp.StatusCode, fmt.Errorf(
			"workbuddy 签到失败: http %d: %s", resp.StatusCode, truncateForError(body))
	}
	if parseErr != nil {
		return WorkBuddyCheckinResult{}, resp.StatusCode, fmt.Errorf(
			"workbuddy 签到响应解析失败: %w (body: %s)", parseErr, truncateForError(body))
	}
	return WorkBuddyCheckinResult{}, resp.StatusCode, fmt.Errorf(
		"workbuddy 签到响应异常: http %d: %s", resp.StatusCode, truncateForError(body))
}

// WorkBuddyCheckinWithRetry 对签到做瞬时错误有界重试（最多 2 次补打，间隔 2s/4s）。
//
// 只重试网络层错误与 5xx：业务拒绝（已签到/参数错/401）重试没有意义。
func WorkBuddyCheckinWithRetry(ctx context.Context, account *Account, proxyURL string) (WorkBuddyCheckinResult, error) {
	result, err := DailyCheckinWorkBuddy(ctx, account, proxyURL)
	if err == nil || !isTransientWorkBuddyCheckinErr(err) {
		return result, err
	}
	for i := 1; i <= 2; i++ {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(time.Duration(i) * 2 * time.Second):
		}
		result, err = DailyCheckinWorkBuddy(ctx, account, proxyURL)
		if err == nil || !isTransientWorkBuddyCheckinErr(err) {
			return result, err
		}
	}
	return result, err
}

// isTransientWorkBuddyCheckinErr 判断错误是否值得重试：网络传输错误或上游 5xx。
func isTransientWorkBuddyCheckinErr(err error) bool {
	if err == nil {
		return false
	}
	var urlErr interface{ Timeout() bool }
	if errors.As(err, &urlErr) {
		return true
	}
	msg := err.Error()
	// 上游 5xx 与网络层错误都带这些特征；业务码错误不含 "http 5"。
	for _, marker := range []string{"http 500", "http 502", "http 503", "http 504", "connection refused", "connection reset", "timeout", "EOF"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

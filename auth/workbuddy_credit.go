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

// WorkBuddy 积分（credits）查询。
//
// 上游端点（与签到同域）：
//
//	POST {base}/v2/billing/meter/get-user-resource   body: {}
//
// 响应 data.Response.Data:
//
//	{ TotalCount, TotalDosage, Accounts: [ { PackageName, CapacityRemain,
//	  CapacitySize, CycleCapacityRemain, CycleCapacitySize, ... } ] }
//
// 口径（实测比对确认）：
//   - 总积分 = TotalDosage，且等于各包 CapacitySize 之和
//   - 剩余积分 = 各包 CapacityRemain 之和
//
// 注意：CycleCapacityRemain 与 CapacityRemain 语义不同（周期重置点 vs 真实可用余额），
// 实测同一账号会出现 3507 vs 3752 的差异，因此剩余额度取 CapacityRemain 而不是周期字段。

const (
	// WorkBuddyCreditPathCN 是 CN 域的积分查询路径。
	WorkBuddyCreditPathCN = "/v2/billing/meter/get-user-resource"
	// WorkBuddyCreditPathGlobal 是 global 域首选路径（国际版无 /v2 前缀）。
	WorkBuddyCreditPathGlobal = "/billing/meter/get-user-resource"
	// WorkBuddyCreditPathGlobalV2 是 global 域的兜底路径。
	WorkBuddyCreditPathGlobalV2 = "/v2/billing/meter/get-user-resource"
)

// WorkBuddyCredit 是一个账号的积分快照。
type WorkBuddyCredit struct {
	// Remain 是当前剩余积分（各包 CapacityRemain 之和）。
	Remain int64
	// Total 是总积分（上游 TotalDosage）。
	Total int64
	// Packages 是该账号的积分包数量，便于排障区分「没有包」与「包已用尽」。
	Packages int
}

// WorkBuddyCreditPaths 按 realm 返回积分查询路径候选序列。
func WorkBuddyCreditPaths(realm string) []string {
	if NormalizeWorkBuddyRealm(realm) == WorkBuddyRealmGlobal {
		return []string{WorkBuddyCreditPathGlobal, WorkBuddyCreditPathGlobalV2}
	}
	return []string{WorkBuddyCreditPathCN}
}

// FetchWorkBuddyCredit 查询单个账号的积分余额。
func FetchWorkBuddyCredit(ctx context.Context, account *Account, proxyURL string) (WorkBuddyCredit, error) {
	if account == nil {
		return WorkBuddyCredit{}, fmt.Errorf("workbuddy: account is nil")
	}
	accessToken := strings.TrimSpace(account.WorkBuddyAccessToken())
	if accessToken == "" {
		return WorkBuddyCredit{}, fmt.Errorf("workbuddy: 账号缺少 access token")
	}
	base := account.WorkBuddyBaseURL()
	if strings.TrimSpace(base) == "" {
		return WorkBuddyCredit{}, fmt.Errorf("workbuddy: 账号缺少 base url")
	}
	client, err := NewWorkBuddyHTTPClient(proxyURL, 30*time.Second)
	if err != nil {
		return WorkBuddyCredit{}, err
	}
	paths := WorkBuddyCreditPaths(account.WorkBuddyRealm)

	var lastErr error
	for i, path := range paths {
		credit, status, err := workBuddyCreditOnce(ctx, client, account, base+path, accessToken)
		if err == nil {
			return credit, nil
		}
		lastErr = err
		if status != http.StatusNotFound || i == len(paths)-1 {
			return WorkBuddyCredit{}, err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("workbuddy: 积分查询失败")
	}
	return WorkBuddyCredit{}, lastErr
}

// workBuddyCreditOnce 发一次积分查询请求。
func workBuddyCreditOnce(ctx context.Context, client *http.Client, account *Account, url, accessToken string) (WorkBuddyCredit, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return WorkBuddyCredit{}, 0, err
	}
	ApplyWorkBuddyCommonHeaders(req, account)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return WorkBuddyCredit{}, 0, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		return WorkBuddyCredit{}, resp.StatusCode, readErr
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return WorkBuddyCredit{}, resp.StatusCode, fmt.Errorf(
			"workbuddy 积分查询失败: http %d: %s", resp.StatusCode, truncateForError(body))
	}

	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Response struct {
				Data struct {
					TotalDosage int64 `json:"TotalDosage"`
					Accounts    []struct {
						CapacityRemain int64 `json:"CapacityRemain"`
						CapacitySize   int64 `json:"CapacitySize"`
					} `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return WorkBuddyCredit{}, resp.StatusCode, fmt.Errorf(
			"workbuddy 积分响应解析失败: %w (body: %s)", err, truncateForError(body))
	}
	if payload.Code != 0 {
		if IsWorkBuddyAlreadyCheckin(payload.Msg) {
			// 不可能命中，仅防御：积分端点不该返回已签到语义。
			return WorkBuddyCredit{}, resp.StatusCode, fmt.Errorf("workbuddy 积分查询异常: code=%d msg=%s", payload.Code, payload.Msg)
		}
		return WorkBuddyCredit{}, resp.StatusCode, fmt.Errorf(
			"workbuddy 积分查询被拒绝: code=%d msg=%s", payload.Code, payload.Msg)
	}

	inner := payload.Data.Response.Data
	credit := WorkBuddyCredit{
		Total:    inner.TotalDosage,
		Packages: len(inner.Accounts),
	}
	var remain int64
	for _, pkg := range inner.Accounts {
		remain += pkg.CapacityRemain
	}
	credit.Remain = remain
	// Total 缺失时用各包 CapacitySize 之和兜底：上游个别版本不下发 TotalDosage。
	if credit.Total <= 0 {
		var total int64
		for _, pkg := range inner.Accounts {
			total += pkg.CapacitySize
		}
		credit.Total = total
	}
	return credit, resp.StatusCode, nil
}

// ErrWorkBuddyCreditUnsupported 表示该账号不支持积分查询。
var ErrWorkBuddyCreditUnsupported = errors.New("workbuddy: 账号不支持积分查询")

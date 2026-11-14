package admin

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// WorkBuddy 每日签到。
//
// 与 WorkBuddy 用完即弃的 chat 调用不同，签到是「每天一次、错点就丢一天额度」的
// 定时任务：它的价值在于稳定执行，所以这里既提供手动触发接口（账号页「签到」/
// 「全部签到」按钮），也提供后台每日定时器（默认本地时间 10:00）。
//
// 定时器语义：每小时醒一次，判断「今天的目标时刻是否已过且今天还没跑过」。
// 用「已过 + 未跑」而不是精确 sleep 到点，是为了容忍进程重启/休眠——重启后如果
// 当天还没签到，当轮就会补上，不会因为错过整点而整天不签。

const (
	// workBuddyCheckinHour 是每日自动签到的目标小时（本地时区）。
	workBuddyCheckinHour = 10

	// workBuddyCheckinTick 是定时器的检查间隔。取小时级：签到点精度到小时即可，
	// 更密的轮询只会增加无意义的唤醒。
	workBuddyCheckinTick = time.Hour

	// workBuddyCheckinConcurrency 限制并发，避免大号池同时打上游。
	workBuddyCheckinConcurrency = 4

	// workBuddyCheckinTimeout 是单账号签到的总时长上限。
	workBuddyCheckinTimeout = 60 * time.Second
)

var workBuddyCheckinSchedulerOnce sync.Once

// workBuddyCheckinState 记录自动签到的运行态（仅本进程内存）。
//
// 不落库的理由：签到本身是幂等的（重复签到上游返回「已签到」），持久化状态带来的
// 收益只是省掉一次幂等调用，却要为一个可有可无的优化引入表结构迁移。进程重启后
// 重新判断一次反而更安全——启动即补签。
type workBuddyCheckinState struct {
	mu sync.Mutex
	// lastAutoDay 是最近一次自动签到执行的日期（本地时区 yyyy-mm-dd）。
	lastAutoDay string
	// lastResult 是最近一次自动签到的汇总，供状态查询接口回显。
	lastResult WorkBuddyCheckinSummary
	lastRunAt  time.Time
}

// WorkBuddyCheckinSummary 是一次批量签到的汇总。
type WorkBuddyCheckinSummary struct {
	// Total 是本次尝试的账号数。
	Total int `json:"total"`
	// Checked 是本次真正签到成功的账号数。
	Checked int `json:"checked"`
	// Already 是今天已经签到过（幂等）的账号数。
	Already int `json:"already"`
	// Failed 是签到失败的账号数。
	Failed int `json:"failed"`
	// StartedAt / FinishedAt 是本次批量执行的起止时刻。
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// Trigger 取值 auto / manual，标明本次由定时器还是用户触发。
	Trigger string `json:"trigger"`
	// Errors 是失败账号的可读原因（最多保留若干条，避免响应体过大）。
	Errors []string `json:"errors,omitempty"`
}

// workBuddyCheckinItemResult 是批量签到里单账号的结果，用于接口逐行回显。
type workBuddyCheckinItemResult struct {
	AccountID int64  `json:"account_id"`
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
	Status    string `json:"status"` // checked / already / failed
	Message   string `json:"message,omitempty"`
}

// StartWorkBuddyDailyCheckin 启动 WorkBuddy 每日自动签到定时器。
//
// 与其它 Start* 任务一样由 main 在装配完成后调用；ctx 结束即退出。
func (h *Handler) StartWorkBuddyDailyCheckin(ctx context.Context) {
	if h == nil || h.store == nil {
		return
	}
	workBuddyCheckinSchedulerOnce.Do(func() {
		go func() {
			// 启动后先等一会：避开进程刚起来时的账号加载与 token 刷新高峰。
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Minute):
			}
			h.runWorkBuddyCheckinIfDue(ctx, "auto")
			ticker := time.NewTicker(workBuddyCheckinTick)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					h.runWorkBuddyCheckinIfDue(ctx, "auto")
				}
			}
		}()
	})
}

// runWorkBuddyCheckinIfDue 在「今天的目标时刻已过且今天没跑过」时执行一轮自动签到。
func (h *Handler) runWorkBuddyCheckinIfDue(ctx context.Context, trigger string) {
	now := time.Now()
	day := now.Format("2006-01-02")
	if now.Hour() < workBuddyCheckinHour {
		return
	}

	state := h.workBuddyCheckinStateRef()
	state.mu.Lock()
	if state.lastAutoDay == day {
		state.mu.Unlock()
		return
	}
	// 先占位再执行：即使执行中被取消/panic，也不会在同一小时内重复触发。
	state.lastAutoDay = day
	state.mu.Unlock()

	summary, err := h.runWorkBuddyCheckinAll(ctx, trigger)
	if err != nil {
		log.Printf("[WorkBuddy] 自动签到失败: %v", err)
		return
	}
	state.mu.Lock()
	state.lastResult = summary
	state.lastRunAt = time.Now()
	state.mu.Unlock()
	log.Printf("[WorkBuddy] 自动签到完成 total=%d checked=%d already=%d failed=%d",
		summary.Total, summary.Checked, summary.Already, summary.Failed)
}

func (h *Handler) workBuddyCheckinStateRef() *workBuddyCheckinState {
	h.workBuddyCheckinMu.Lock()
	defer h.workBuddyCheckinMu.Unlock()
	if h.workBuddyCheckinSt == nil {
		h.workBuddyCheckinSt = &workBuddyCheckinState{}
	}
	return h.workBuddyCheckinSt
}

// workBuddyCheckinTargets 挑出可签到的账号：WorkBuddy 渠道、未删除、有 refresh token。
func (h *Handler) workBuddyCheckinTargets() []*auth.Account {
	if h == nil || h.store == nil {
		return nil
	}
	accounts := h.store.Accounts()
	out := make([]*auth.Account, 0, len(accounts))
	for _, account := range accounts {
		if !workBuddyCheckinEligible(account) {
			continue
		}
		out = append(out, account)
	}
	return out
}

// workBuddyCheckinEligible 判断账号是否可参与签到。
func workBuddyCheckinEligible(account *auth.Account) bool {
	if account == nil || account.DBID <= 0 {
		return false
	}
	if !account.IsWorkBuddy() {
		return false
	}
	if account.WorkBuddyUID() == "" {
		return false
	}
	// 有 access token 或 refresh token 之一即可：执行前会按需刷新。
	return account.WorkBuddyAccessToken() != "" || strings.TrimSpace(account.RefreshToken) != ""
}

// runWorkBuddyCheckinAll 对所有可签到账号执行一轮签到。
func (h *Handler) runWorkBuddyCheckinAll(ctx context.Context, trigger string) (WorkBuddyCheckinSummary, error) {
	targets := h.workBuddyCheckinTargets()
	summary := WorkBuddyCheckinSummary{
		Total:     len(targets),
		StartedAt: time.Now(),
		Trigger:   trigger,
	}
	if len(targets) == 0 {
		summary.FinishedAt = time.Now()
		return summary, nil
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		sem  = make(chan struct{}, workBuddyCheckinConcurrency)
		errs []string
	)
	for _, account := range targets {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(acc *auth.Account) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			result := h.checkinOneWorkBuddyAccount(ctx, acc)
			mu.Lock()
			defer mu.Unlock()
			switch result.Status {
			case "checked":
				summary.Checked++
			case "already":
				summary.Already++
			default:
				summary.Failed++
				if len(errs) < 10 {
					errs = append(errs, fmt.Sprintf("account=%d %s", acc.DBID, result.Message))
				}
			}
		}(account)
	}
	wg.Wait()
	summary.FinishedAt = time.Now()
	summary.Errors = errs
	return summary, nil
}

// checkinOneWorkBuddyAccount 对单账号签到，必要时先刷新 token。
func (h *Handler) checkinOneWorkBuddyAccount(ctx context.Context, account *auth.Account) workBuddyCheckinItemResult {
	item := workBuddyCheckinItemResult{
		AccountID: account.DBID,
		UID:       account.WorkBuddyUID(),
		// auth.Account 上没有昵称读取器（昵称是 database 行对象从凭据 JSON 里取出的），
		// 这里用账号显示名兜底；前端按 uid 关联列表行，昵称仅用于提示可读性。
		Nickname: strings.TrimSpace(account.Email),
	}
	reqCtx, cancel := context.WithTimeout(ctx, workBuddyCheckinTimeout)
	defer cancel()

	// token 临近过期或为空时先刷新：签到不依赖 chat 路径，不会有人替它刷。
	h.ensureWorkBuddyTokenForCheckin(reqCtx, account)

	result, err := auth.WorkBuddyCheckinWithRetry(reqCtx, account, account.ProxyURL)
	if err != nil {
		item.Status = "failed"
		item.Message = err.Error()
		return item
	}
	if result.Already {
		item.Status = "already"
		item.Message = "今天已签到"
		return item
	}
	item.Status = "checked"
	item.Message = result.Msg
	return item
}

// ensureWorkBuddyTokenForCheckin 在 token 缺失或即将过期时刷新一次。
//
// 刷新失败不阻断签到：access token 可能仍然有效，让上游判定比本地猜测更准。
func (h *Handler) ensureWorkBuddyTokenForCheckin(ctx context.Context, account *auth.Account) {
	if account == nil {
		return
	}
	hasToken := strings.TrimSpace(account.WorkBuddyAccessToken()) != ""
	expiresAt := account.WorkBuddyExpiresAt()
	needsRefresh := !hasToken || (!expiresAt.IsZero() && time.Now().Add(5*time.Minute).After(expiresAt))
	if !needsRefresh {
		return
	}
	if strings.TrimSpace(account.RefreshToken) == "" {
		return
	}
	token, err := auth.RefreshWorkBuddyToken(ctx, account, account.ProxyURL)
	if err != nil {
		log.Printf("[WorkBuddy] 签到前刷新 token 失败 account=%d: %v", account.DBID, err)
		return
	}
	account.SetWorkBuddyToken(token.AccessToken, token.RefreshToken, token.ExpiresAt)
	updates := map[string]interface{}{
		"api_key":                               token.AccessToken,
		auth.WorkBuddyRefreshTokenCredentialKey: token.RefreshToken,
		auth.WorkBuddyExpiresAtCredentialKey:    token.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if err := h.db.UpdateCredentials(ctx, account.ID(), updates); err != nil {
		log.Printf("[WorkBuddy] 签到前 token 落库失败 account=%d: %v", account.DBID, err)
	}
}

// CheckinWorkBuddyAccounts 手动签到接口：POST /accounts/workbuddy/checkin
//
// 请求体可选 {"ids": [1,2,3]}；省略或为空表示全部可签到账号。
// 返回逐账号结果与汇总，前端据此逐行回显。
func (h *Handler) CheckinWorkBuddyAccounts(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	// 请求体可空：读失败按「全部」处理，不报错。
	_ = c.ShouldBindJSON(&req)

	targets := h.workBuddyCheckinTargets()
	if len(req.IDs) > 0 {
		want := make(map[int64]struct{}, len(req.IDs))
		for _, id := range req.IDs {
			if id > 0 {
				want[id] = struct{}{}
			}
		}
		filtered := make([]*auth.Account, 0, len(want))
		for _, account := range targets {
			if _, ok := want[account.DBID]; ok {
				filtered = append(filtered, account)
			}
		}
		targets = filtered
	}
	if len(targets) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"summary": WorkBuddyCheckinSummary{Total: 0, Trigger: "manual", StartedAt: time.Now(), FinishedAt: time.Now()},
			"items":   []workBuddyCheckinItemResult{},
		})
		return
	}

	// 手动签到允许更长的整体时限：账号多时并发 4 也要几分钟。
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Minute)
	defer cancel()

	summary := WorkBuddyCheckinSummary{
		Total:     len(targets),
		StartedAt: time.Now(),
		Trigger:   "manual",
	}
	items := make([]workBuddyCheckinItemResult, 0, len(targets))
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		sem = make(chan struct{}, workBuddyCheckinConcurrency)
	)
	for _, account := range targets {
		wg.Add(1)
		go func(acc *auth.Account) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			item := h.checkinOneWorkBuddyAccount(ctx, acc)
			mu.Lock()
			defer mu.Unlock()
			items = append(items, item)
			switch item.Status {
			case "checked":
				summary.Checked++
			case "already":
				summary.Already++
			default:
				summary.Failed++
			}
		}(account)
	}
	wg.Wait()
	summary.FinishedAt = time.Now()

	security.SecurityAuditLog("WORKBUDDY_CHECKIN", fmt.Sprintf(
		"total=%d checked=%d already=%d failed=%d ip=%s",
		summary.Total, summary.Checked, summary.Already, summary.Failed, c.ClientIP()))

	c.JSON(http.StatusOK, gin.H{"summary": summary, "items": items})
}

// GetWorkBuddyCheckinStatus 返回最近一次自动签到的运行态（供前端展示）。
func (h *Handler) GetWorkBuddyCheckinStatus(c *gin.Context) {
	state := h.workBuddyCheckinStateRef()
	state.mu.Lock()
	defer state.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"hour":        workBuddyCheckinHour,
		"last_auto_day": state.lastAutoDay,
		"last_run_at": state.lastRunAt,
		"last_result": state.lastResult,
	})
}

// parseIDsQuery 解析逗号分隔的 id 列表（供可能的 GET 变体复用）。
func parseIDsQuery(raw string) []int64 {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]int64, 0, len(parts))
	for _, part := range parts {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}

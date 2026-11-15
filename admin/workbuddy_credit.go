package admin

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
)

// WorkBuddy 账号列表的「积分」与「签到状态」两列的数据来源。
//
// 积分：实时查上游 get-user-resource（见 auth/workbuddy_credit.go）。列表接口
// 一次要覆盖整页账号，逐个串行打上游会让页面明显变慢，因此这里做并发受限的
// 批量拉取，并带短 TTL 缓存：翻页/刷新时重复请求同一账号不再重复打上游。
//
// 签到状态：签到时把日期写进账号凭据 workbuddy_checkin_date（yyyy-mm-dd），
// 列表直接比对今天即可。不额外建表——签到本身幂等，一个日期字符串足够表达
// 「今天签没签」，且跟着账号走，删账号自动清理。

const (
	// workBuddyCreditConcurrency 限制批量积分查询的并发，避免大号池同时打上游。
	workBuddyCreditConcurrency = 4
	// workBuddyCreditTimeout 是单个账号积分查询的超时。
	workBuddyCreditTimeout = 15 * time.Second
	// workBuddyCreditCacheTTL 是积分缓存有效期：列表高频刷新时避免重复打上游，
	// 又保证用户点刷新能较快看到新值。
	workBuddyCreditCacheTTL = 30 * time.Second
	// workBuddyCheckinDateCredentialKey 记录最近一次成功签到的日期（本地时区）。
	workBuddyCheckinDateCredentialKey = "workbuddy_checkin_date"
)

// workBuddyCreditEntry 是积分缓存条目。
type workBuddyCreditEntry struct {
	credit auth.WorkBuddyCredit
	err    error
	at     time.Time
}

// workBuddyCreditCache 是账号 id → 积分快照的进程内缓存。
var (
	workBuddyCreditCacheMu sync.Mutex
	workBuddyCreditCache   = map[int64]workBuddyCreditEntry{}
)

// workBuddyTodayKey 返回本地时区的今日键（yyyy-mm-dd），与签到写入口径一致。
func workBuddyTodayKey() string {
	return time.Now().Format("2006-01-02")
}

// workBuddyCheckinStateFor 判断账号今天是否已签到。
// 返回 ("已签到"/"未签到", true)；凭据缺失时返回 ("", false) 表示未知。
func workBuddyCheckinStateFor(date string) (string, bool) {
	date = strings.TrimSpace(date)
	if date == "" {
		return "未签到", true
	}
	if date == workBuddyTodayKey() {
		return "已签到", true
	}
	return "未签到", true
}

// recordWorkBuddyCheckinDate 把签到日期写入账号凭据。
func (h *Handler) recordWorkBuddyCheckinDate(ctx context.Context, account *auth.Account) {
	if h == nil || h.db == nil || account == nil || account.DBID <= 0 {
		return
	}
	today := workBuddyTodayKey()
	updates := map[string]interface{}{workBuddyCheckinDateCredentialKey: today}
	if err := h.db.UpdateCredentials(ctx, account.DBID, updates); err != nil {
		log.Printf("[WorkBuddy] 记录签到日期失败 account=%d: %v", account.DBID, err)
	}
}

// creditCacheGet 读缓存，命中且未过期返回。
func creditCacheGet(id int64) (auth.WorkBuddyCredit, error, bool) {
	workBuddyCreditCacheMu.Lock()
	defer workBuddyCreditCacheMu.Unlock()
	entry, ok := workBuddyCreditCache[id]
	if !ok {
		return auth.WorkBuddyCredit{}, nil, false
	}
	if time.Since(entry.at) > workBuddyCreditCacheTTL {
		delete(workBuddyCreditCache, id)
		return auth.WorkBuddyCredit{}, nil, false
	}
	return entry.credit, entry.err, true
}

// creditCachePut 写缓存。失败结果同样缓存，避免坏号每次刷新都打上游。
func creditCachePut(id int64, credit auth.WorkBuddyCredit, err error) {
	workBuddyCreditCacheMu.Lock()
	defer workBuddyCreditCacheMu.Unlock()
	// 简单防膨胀：超过 2000 个账号整体清空（进程生命周期内账号数远小于此）。
	if len(workBuddyCreditCache) > 2000 {
		workBuddyCreditCache = map[int64]workBuddyCreditEntry{}
	}
	workBuddyCreditCache[id] = workBuddyCreditEntry{credit: credit, err: err, at: time.Now()}
}

// invalidateWorkBuddyCreditCache 清掉某账号的积分缓存（签到时额度会变）。
func invalidateWorkBuddyCreditCache(id int64) {
	workBuddyCreditCacheMu.Lock()
	delete(workBuddyCreditCache, id)
	workBuddyCreditCacheMu.Unlock()
}

// fetchWorkBuddyCredits 并发批量拉取一组账号的积分。
// forced 为 true 时忽略缓存、强制回源上游（页面刷新用）。
// 返回 account id → 积分；失败的账号不出现在结果里（前端显示为「—」）。
func (h *Handler) fetchWorkBuddyCredits(ctx context.Context, accounts []*auth.Account, forced bool) map[int64]auth.WorkBuddyCredit {
	out := make(map[int64]auth.WorkBuddyCredit, len(accounts))
	if len(accounts) == 0 {
		return out
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, workBuddyCreditConcurrency)
	)
	for _, account := range accounts {
		if account == nil || account.DBID <= 0 {
			continue
		}
		// 先看缓存（强制刷新时跳过）。
		if !forced {
			if credit, err, ok := creditCacheGet(account.DBID); ok {
				if err == nil {
					mu.Lock()
					out[account.DBID] = credit
					mu.Unlock()
				}
				continue
			}
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

			reqCtx, cancel := context.WithTimeout(ctx, workBuddyCreditTimeout)
			defer cancel()
			// token 过期时先刷新，否则积分查询会 401。
			h.ensureWorkBuddyTokenForCheckin(reqCtx, acc)
			credit, err := auth.FetchWorkBuddyCredit(reqCtx, acc, acc.ProxyURL)
			creditCachePut(acc.DBID, credit, err)
			if err != nil {
				return
			}
			mu.Lock()
			out[acc.DBID] = credit
			mu.Unlock()
		}(account)
	}
	wg.Wait()
	return out
}

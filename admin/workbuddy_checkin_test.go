package admin

import (
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
)

// newEligibleWorkBuddyAccount 构造一个可通过资格判断的 WorkBuddy 账号。
func newEligibleWorkBuddyAccount() *auth.Account {
	return &auth.Account{
		DBID:         7,
		UpstreamType: auth.UpstreamWorkBuddy,
		AccountID:    "uid-7",
		APIKey:       "at-7",
	}
}

// 管理员停用（accounts.enabled=false → DispatchPaused=1）的账号不得参与签到。
//
// 这条是回归护栏：早期实现只判断渠道/UID/token，漏了停用标志，
// 表现为「停用后每天仍被自动签到」，与停用语义冲突。
func TestWorkBuddyCheckinEligibleSkipsDisabled(t *testing.T) {
	account := newEligibleWorkBuddyAccount()
	if !workBuddyCheckinEligible(account) {
		t.Fatal("baseline account should be eligible")
	}

	atomic.StoreInt32(&account.DispatchPaused, 1)
	if workBuddyCheckinEligible(account) {
		t.Fatal("disabled account (DispatchPaused=1) must not be eligible for checkin")
	}

	atomic.StoreInt32(&account.DispatchPaused, 0)
	if !workBuddyCheckinEligible(account) {
		t.Fatal("re-enabled account should be eligible again")
	}
}

// 非 WorkBuddy 渠道、缺 UID、缺凭据的账号都不可签到。
func TestWorkBuddyCheckinEligibleRejectsIncomplete(t *testing.T) {
	codex := &auth.Account{DBID: 1, UpstreamType: "codex", AccessToken: "at"}
	if workBuddyCheckinEligible(codex) {
		t.Fatal("non-workbuddy account must not be eligible")
	}

	noUID := newEligibleWorkBuddyAccount()
	noUID.AccountID = ""
	if workBuddyCheckinEligible(noUID) {
		t.Fatal("account without uid must not be eligible")
	}

	noCreds := newEligibleWorkBuddyAccount()
	noCreds.APIKey = ""
	noCreds.RefreshToken = ""
	if workBuddyCheckinEligible(noCreds) {
		t.Fatal("account without any credential must not be eligible")
	}

	if workBuddyCheckinEligible(nil) {
		t.Fatal("nil account must not be eligible")
	}
}

// 只有 refresh token 的账号仍应可签到（执行前会刷新出 access token）。
func TestWorkBuddyCheckinEligibleAllowsRefreshTokenOnly(t *testing.T) {
	account := newEligibleWorkBuddyAccount()
	account.APIKey = ""
	account.RefreshToken = "rt-7"
	if !workBuddyCheckinEligible(account) {
		t.Fatal("refresh-token-only account should be eligible")
	}
}

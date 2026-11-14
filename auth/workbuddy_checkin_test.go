package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newCheckinTestAccount 构造一个最小可用的 WorkBuddy 账号。
func newCheckinTestAccount(baseURL string) *Account {
	acc := &Account{
		DBID:         1,
		UpstreamType: UpstreamWorkBuddy,
		BaseURL:      baseURL,
		APIKey:       "at-test",
		RefreshToken: "rt-test",
		AccountID:    "uid-test",
	}
	return acc
}

// 签到成功：上游 code=0 + HTTP 200。
func TestDailyCheckinWorkBuddySuccess(t *testing.T) {
	var gotPath, gotAuth, gotUID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer server.Close()

	account := newCheckinTestAccount(server.URL)
	result, err := DailyCheckinWorkBuddy(context.Background(), account, "")
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !result.Checked {
		t.Fatalf("expected Checked=true, got %+v", result)
	}
	if result.Already {
		t.Fatalf("expected Already=false, got %+v", result)
	}
	if gotPath != WorkBuddyCheckinPathCN {
		t.Fatalf("path = %q, want %q", gotPath, WorkBuddyCheckinPathCN)
	}
	if gotAuth != "Bearer at-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotUID != "uid-test" {
		t.Fatalf("X-User-Id = %q", gotUID)
	}
}

// 今天已签到：上游 HTTP 400 + code=10001，必须判成幂等成功而不是失败。
func TestDailyCheckinWorkBuddyAlready(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":10001,"msg":"今天已签到，请明天再来"}`))
	}))
	defer server.Close()

	account := newCheckinTestAccount(server.URL)
	result, err := DailyCheckinWorkBuddy(context.Background(), account, "")
	if err != nil {
		t.Fatalf("already checkin must not be an error, got: %v", err)
	}
	if !result.Already {
		t.Fatalf("expected Already=true, got %+v", result)
	}
	if result.Checked {
		t.Fatalf("expected Checked=false, got %+v", result)
	}
	if result.Code != 10001 {
		t.Fatalf("Code = %d, want 10001", result.Code)
	}
}

// 业务错误（非已签到）：必须返回错误。
func TestDailyCheckinWorkBuddyBusinessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":11101,"msg":"参数错误"}`))
	}))
	defer server.Close()

	account := newCheckinTestAccount(server.URL)
	if _, err := DailyCheckinWorkBuddy(context.Background(), account, ""); err == nil {
		t.Fatal("expected error for business failure")
	}
}

// 缺少 access token 时在本地拒绝，不打上游。
func TestDailyCheckinWorkBuddyRequiresToken(t *testing.T) {
	account := newCheckinTestAccount("http://127.0.0.1:1")
	account.APIKey = ""
	if _, err := DailyCheckinWorkBuddy(context.Background(), account, ""); err == nil {
		t.Fatal("expected error for missing access token")
	}
}

// global 域先试无 /v2 前缀，404 时回退到带前缀的路径。
func TestDailyCheckinWorkBuddyGlobalFallsBackOn404(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == WorkBuddyCheckinPathGlobal {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer server.Close()

	account := newCheckinTestAccount(server.URL)
	account.BaseURL = server.URL
	account.WorkBuddyRealm = WorkBuddyRealmGlobal
	result, err := DailyCheckinWorkBuddy(context.Background(), account, "")
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !result.Checked {
		t.Fatalf("expected Checked=true, got %+v", result)
	}
	if len(paths) != 2 {
		t.Fatalf("expected 2 attempts (404 then fallback), got %v", paths)
	}
	if paths[0] != WorkBuddyCheckinPathGlobal || paths[1] != WorkBuddyCheckinPathGlobalV2 {
		t.Fatalf("unexpected path order: %v", paths)
	}
}

// CN 域只有一条路径，404 不重试。
func TestDailyCheckinWorkBuddyCNNoFallback(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
	}))
	defer server.Close()

	account := newCheckinTestAccount(server.URL)
	if _, err := DailyCheckinWorkBuddy(context.Background(), account, ""); err == nil {
		t.Fatal("expected error for 404 on CN")
	}
	if calls != 1 {
		t.Fatalf("CN must not fallback, calls=%d want 1", calls)
	}
}

// 路径表：global 两条、CN 一条。
func TestWorkBuddyCheckinPaths(t *testing.T) {
	if got := WorkBuddyCheckinPaths(WorkBuddyRealmGlobal); len(got) != 2 {
		t.Fatalf("global paths = %v, want 2", got)
	}
	if got := WorkBuddyCheckinPaths(WorkBuddyRealmCN); len(got) != 1 {
		t.Fatalf("cn paths = %v, want 1", got)
	}
	if got := WorkBuddyCheckinPaths(""); len(got) != 1 {
		t.Fatalf("empty realm should default to cn, got %v", got)
	}
}

// 已签到识别：按 code 与文案双判，大小写不敏感。
func TestIsWorkBuddyAlreadyCheckin(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"今天已签到，请明天再来", true},
		{"今日已签到", true},
		{"Already checked in today", true},
		{"already checked", true},
		{"参数错误", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsWorkBuddyAlreadyCheckin(tc.msg); got != tc.want {
			t.Errorf("IsWorkBuddyAlreadyCheckin(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// 瞬时错误判定：5xx 与网络错误重试，业务错误不重试。
func TestIsTransientWorkBuddyCheckinErr(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&checkinTestErr{"workbuddy 签到失败: http 500: boom"}, true},
		{&checkinTestErr{"workbuddy 签到失败: http 502: boom"}, true},
		{&checkinTestErr{"connection refused"}, true},
		{&checkinTestErr{"timeout"}, true},
		{&checkinTestErr{"workbuddy 签到被拒绝: code=11101 msg=参数错误"}, false},
		{&checkinTestErr{"workbuddy 签到失败: http 401: unauthorized"}, false},
	}
	for _, tc := range cases {
		if got := isTransientWorkBuddyCheckinErr(tc.err); got != tc.want {
			t.Errorf("isTransientWorkBuddyCheckinErr(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

type checkinTestErr struct{ msg string }

func (e *checkinTestErr) Error() string { return e.msg }

// 重试：首次 500、第二次 200 应整体成功。
func TestWorkBuddyCheckinWithRetryRecovers(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":10000,"msg":"API request failed with status code: 500"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	account := newCheckinTestAccount(server.URL)
	result, err := WorkBuddyCheckinWithRetry(ctx, account, "")
	if err != nil {
		t.Fatalf("retry should recover, got: %v", err)
	}
	if !result.Checked {
		t.Fatalf("expected Checked=true, got %+v", result)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}

// 重试不作用于业务错误：已签到只打一次。
func TestWorkBuddyCheckinWithRetrySkipsBusinessError(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":10001,"msg":"今天已签到"}`))
	}))
	defer server.Close()

	account := newCheckinTestAccount(server.URL)
	result, err := WorkBuddyCheckinWithRetry(context.Background(), account, "")
	if err != nil {
		t.Fatalf("already checkin must not fail: %v", err)
	}
	if !result.Already {
		t.Fatalf("expected Already=true, got %+v", result)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1 (no retry for business rejection)", calls)
	}
}

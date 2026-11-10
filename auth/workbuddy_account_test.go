package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRefreshWorkBuddyTokenSuccess(t *testing.T) {
	var gotPath, gotRefresh, gotSource string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRefresh = r.Header.Get("X-Refresh-Token")
		gotSource = r.Header.Get("X-Auth-Refresh-Source")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}}`)
	}))
	defer server.Close()

	account := &Account{UpstreamType: UpstreamWorkBuddy, BaseURL: server.URL, APIKey: "old-at", RefreshToken: "old-rt", AccountID: "uid-1"}
	token, err := RefreshWorkBuddyToken(context.Background(), account, "")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if gotPath != "/v2/plugin/auth/token/refresh" {
		t.Fatalf("refresh path = %q", gotPath)
	}
	if gotRefresh != "old-rt" || gotSource != "plugin" {
		t.Fatalf("refresh headers = %q / %q", gotRefresh, gotSource)
	}
	if token.AccessToken != "new-at" || token.RefreshToken != "new-rt" {
		t.Fatalf("token = %+v", token)
	}
	if time.Until(token.ExpiresAt) < 50*time.Minute {
		t.Fatalf("expiresAt not advanced: %v", token.ExpiresAt)
	}
}

func TestRefreshWorkBuddyTokenSessionDead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":12153,"msg":"12153:refresh token failed:400 Bad Request: invalid_grant: Offline user session not found"}`)
	}))
	defer server.Close()

	account := &Account{UpstreamType: UpstreamWorkBuddy, BaseURL: server.URL, RefreshToken: "dead-rt", AccountID: "uid-1"}
	_, err := RefreshWorkBuddyToken(context.Background(), account, "")
	if err == nil {
		t.Fatal("expected error for dead refresh token")
	}
	if !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "12153") {
		t.Fatalf("error should carry upstream status/code, got %v", err)
	}
}

func TestWorkBuddyRealmAndBaseURL(t *testing.T) {
	cn := &Account{UpstreamType: UpstreamWorkBuddy}
	if cn.WorkBuddyDomain() != WorkBuddyRealmCN || cn.WorkBuddyBaseURL() != WorkBuddyBaseURLCN {
		t.Fatalf("cn defaults wrong: %s / %s", cn.WorkBuddyDomain(), cn.WorkBuddyBaseURL())
	}
	global := &Account{UpstreamType: UpstreamWorkBuddy, WorkBuddyRealm: WorkBuddyRealmGlobal}
	if global.WorkBuddyDomain() != WorkBuddyRealmGlobal || global.WorkBuddyBaseURL() != WorkBuddyBaseURLGlobal {
		t.Fatalf("global defaults wrong: %s / %s", global.WorkBuddyDomain(), global.WorkBuddyBaseURL())
	}
	if global.WorkBuddyOrigin() != WorkBuddyOriginGlobal {
		t.Fatalf("global origin = %s", global.WorkBuddyOrigin())
	}
	// 显式 base 优先
	custom := &Account{UpstreamType: UpstreamWorkBuddy, BaseURL: "https://example.test/"}
	if custom.WorkBuddyBaseURL() != "https://example.test" {
		t.Fatalf("custom base = %s", custom.WorkBuddyBaseURL())
	}
}

func TestWorkBuddyStableIDStability(t *testing.T) {
	a := WorkBuddyStableID("uid-1", "machine")
	b := WorkBuddyStableID("uid-1", "machine")
	c := WorkBuddyStableID("uid-1", "session")
	if a == "" || a != b {
		t.Fatal("machine id must be stable per uid")
	}
	if a == c {
		t.Fatal("machine and session ids must differ")
	}
	if WorkBuddyStableID("", "machine") != "" {
		t.Fatal("empty uid must not produce an id")
	}
}

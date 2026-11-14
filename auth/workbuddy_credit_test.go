package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newCreditTestAccount(baseURL string) *Account {
	return &Account{
		DBID:         1,
		UpstreamType: UpstreamWorkBuddy,
		BaseURL:      baseURL,
		APIKey:       "at-test",
		RefreshToken: "rt-test",
		AccountID:    "uid-test",
	}
}

// creditResponse 构造标准 get-user-resource 响应。
func creditResponse(total int64, packages ...int64) string {
	// packages 依次为 [remain1, size1, remain2, size2, ...]
	body := `{"code":0,"msg":"OK","data":{"Response":{"Data":{"TotalDosage":` + itoa(total) + `,"Accounts":[`
	for i := 0; i+1 < len(packages); i += 2 {
		if i > 0 {
			body += ","
		}
		body += `{"CapacityRemain":` + itoa(packages[i]) + `,"CapacitySize":` + itoa(packages[i+1]) + `}`
	}
	body += `]}}}}`
	return body
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// 正常取到积分：剩余为各包 CapacityRemain 之和，总额取 TotalDosage。
func TestFetchWorkBuddyCreditSuccess(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(creditResponse(3752, 1500, 1500, 100, 100, 252, 252)))
	}))
	defer server.Close()

	account := newCreditTestAccount(server.URL)
	credit, err := FetchWorkBuddyCredit(context.Background(), account, "")
	if err != nil {
		t.Fatalf("fetch credit: %v", err)
	}
	if credit.Remain != 1852 {
		t.Fatalf("Remain = %d, want 1852 (1500+100+252)", credit.Remain)
	}
	if credit.Total != 3752 {
		t.Fatalf("Total = %d, want 3752", credit.Total)
	}
	if credit.Packages != 3 {
		t.Fatalf("Packages = %d, want 3", credit.Packages)
	}
	if gotPath != WorkBuddyCreditPathCN {
		t.Fatalf("path = %q, want %q", gotPath, WorkBuddyCreditPathCN)
	}
	if gotAuth != "Bearer at-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

// 包为空时剩余为 0，但总额仍取上游值。
func TestFetchWorkBuddyCreditEmptyPackages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(creditResponse(0)))
	}))
	defer server.Close()

	account := newCreditTestAccount(server.URL)
	credit, err := FetchWorkBuddyCredit(context.Background(), account, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if credit.Remain != 0 || credit.Total != 0 || credit.Packages != 0 {
		t.Fatalf("unexpected credit: %+v", credit)
	}
}

// TotalDosage 缺失时用各包 CapacitySize 之和兜底。
func TestFetchWorkBuddyCreditTotalFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"CapacityRemain":60,"CapacitySize":100},
			{"CapacityRemain":40,"CapacitySize":200}]}}}}`))
	}))
	defer server.Close()

	account := newCreditTestAccount(server.URL)
	credit, err := FetchWorkBuddyCredit(context.Background(), account, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if credit.Total != 300 {
		t.Fatalf("Total = %d, want 300 (100+200 fallback)", credit.Total)
	}
	if credit.Remain != 100 {
		t.Fatalf("Remain = %d, want 100", credit.Remain)
	}
}

// 缺 access token 本地拒绝。
func TestFetchWorkBuddyCreditRequiresToken(t *testing.T) {
	account := newCreditTestAccount("http://127.0.0.1:1")
	account.APIKey = ""
	if _, err := FetchWorkBuddyCredit(context.Background(), account, ""); err == nil {
		t.Fatal("expected error for missing access token")
	}
}

// 业务错误必须返回 error。
func TestFetchWorkBuddyCreditBusinessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":11101,"msg":"参数错误"}`))
	}))
	defer server.Close()

	account := newCreditTestAccount(server.URL)
	if _, err := FetchWorkBuddyCredit(context.Background(), account, ""); err == nil {
		t.Fatal("expected error for business failure")
	}
}

// HTTP 5xx 必须返回 error 且带状态码语义。
func TestFetchWorkBuddyCreditServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":10000,"msg":"internal"}`))
	}))
	defer server.Close()

	account := newCreditTestAccount(server.URL)
	if _, err := FetchWorkBuddyCredit(context.Background(), account, ""); err == nil {
		t.Fatal("expected error for 500")
	}
}

// global 域先走无 /v2 前缀，404 回退。
func TestFetchWorkBuddyCreditGlobalFallback(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == WorkBuddyCreditPathGlobal {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(creditResponse(500, 300, 500)))
	}))
	defer server.Close()

	account := newCreditTestAccount(server.URL)
	account.WorkBuddyRealm = WorkBuddyRealmGlobal
	credit, err := FetchWorkBuddyCredit(context.Background(), account, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if credit.Remain != 300 || credit.Total != 500 {
		t.Fatalf("unexpected credit: %+v", credit)
	}
	if len(paths) != 2 || paths[0] != WorkBuddyCreditPathGlobal || paths[1] != WorkBuddyCreditPathGlobalV2 {
		t.Fatalf("unexpected path order: %v", paths)
	}
}

// 路径表。
func TestWorkBuddyCreditPaths(t *testing.T) {
	if got := WorkBuddyCreditPaths(WorkBuddyRealmGlobal); len(got) != 2 {
		t.Fatalf("global paths = %v, want 2", got)
	}
	if got := WorkBuddyCreditPaths(WorkBuddyRealmCN); len(got) != 1 {
		t.Fatalf("cn paths = %v, want 1", got)
	}
}

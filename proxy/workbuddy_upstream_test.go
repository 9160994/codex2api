package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

func newWorkBuddyTestAccount(baseURL string) *auth.Account {
	return &auth.Account{
		DBID:           42,
		UpstreamType:   auth.UpstreamWorkBuddy,
		BaseURL:        baseURL,
		APIKey:         "at-test",
		RefreshToken:   "rt-test",
		AccountID:      "uid-123",
		WorkBuddyRealm: auth.WorkBuddyRealmCN,
	}
}

// WorkBuddy 上游契约：路径 /v2/chat/completions、Bearer、X-User-Id、官方 UA。
func TestExecuteWorkBuddyChatRequestSendsOfficialHeaders(t *testing.T) {
	var gotPath, gotAuth, gotUID, gotUA, gotCodeBuddy string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		gotUA = r.Header.Get("User-Agent")
		gotCodeBuddy = r.Header.Get("X-CodeBuddy-Request")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	account := newWorkBuddyTestAccount(server.URL)
	body := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	resp, err := ExecuteWorkBuddyChatRequest(context.Background(), account, body, "", nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if gotPath != "/v2/chat/completions" {
		t.Fatalf("upstream path = %q, want /v2/chat/completions", gotPath)
	}
	if gotAuth != "Bearer at-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotUID != "uid-123" {
		t.Fatalf("X-User-Id = %q", gotUID)
	}
	if gotUA == "" || !strings.Contains(gotUA, "CodeBuddy") {
		t.Fatalf("User-Agent = %q, want CodeBuddy CLI UA", gotUA)
	}
	if gotCodeBuddy != "1" {
		t.Fatalf("X-CodeBuddy-Request = %q, want 1", gotCodeBuddy)
	}
}

// 非流式客户端：上游强制流式，网关聚合成单个 chat.completion。
func TestExecuteWorkBuddyChatRequestAggregatesNonStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), `"stream":true`) {
			t.Errorf("upstream body should force stream=true, got %s", raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"cmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"可用\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"cmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"！\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	account := newWorkBuddyTestAccount(server.URL)
	body := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	resp, err := ExecuteWorkBuddyChatRequest(context.Background(), account, body, "", nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var payload struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("aggregated body is not JSON: %v (%s)", err, raw)
	}
	if payload.Object != "chat.completion" {
		t.Fatalf("object = %q", payload.Object)
	}
	if len(payload.Choices) != 1 || payload.Choices[0].Message.Content != "可用！" {
		t.Fatalf("aggregated content mismatch: %+v", payload.Choices)
	}
	if payload.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", payload.Choices[0].FinishReason)
	}
	if payload.Usage.TotalTokens != 5 {
		t.Fatalf("usage not preserved: %+v", payload.Usage)
	}
}

// 无 access token 时必须在网关侧拒绝，不发请求。
func TestExecuteWorkBuddyChatRequestRequiresAccessToken(t *testing.T) {
	account := newWorkBuddyTestAccount("http://127.0.0.1:1")
	account.APIKey = ""
	if _, err := ExecuteWorkBuddyChatRequest(context.Background(), account, []byte(`{"model":"m"}`), "", nil); err == nil {
		t.Fatal("expected error for missing access token")
	}
}

// 同一帧同时给出裸 choices 与 data 信封两种正文表达时，只能取其中一个，
// 否则聚合后的 content 会把正文重复拼接一遍。
func TestAggregateWorkBuddyStreamDoesNotDuplicateEnvelopeContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c1","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"可用","reasoning_content":"思考A"}}],"data":{"choices":[{"delta":{"content":"可用"}}],"content":"可用"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"！","reasoning":"思考B"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	account := newWorkBuddyTestAccount(server.URL)
	body := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	resp, err := ExecuteWorkBuddyChatRequest(context.Background(), account, body, "", nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var payload struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("aggregated body is not JSON: %v (%s)", err, raw)
	}
	if got := payload.Choices[0].Message.Content; got != "可用！" {
		t.Fatalf("content duplicated across envelope paths: %q, want %q", got, "可用！")
	}
	if got := payload.Choices[0].Message.ReasoningContent; got != "思考A思考B" {
		t.Fatalf("reasoning duplicated across alias paths: %q, want %q", got, "思考A思考B")
	}
}

// 分段数组形态的 content 仍然要整段拼上，不能因为「只取第一个命中」而丢片段。
func TestAggregateWorkBuddyStreamKeepsSegmentedContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c2","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"分段"},{"type":"text","text":"正文"}]}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	account := newWorkBuddyTestAccount(server.URL)
	body := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	resp, err := ExecuteWorkBuddyChatRequest(context.Background(), account, body, "", nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var payload struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("aggregated body is not JSON: %v (%s)", err, raw)
	}
	if got := payload.Choices[0].Message.Content; got != "分段正文" {
		t.Fatalf("segmented content lost: %q", got)
	}
}

// 模型目录解析要兼容字符串/对象两种条目与 data/models 两种容器。
func TestParseWorkBuddyModelCatalog(t *testing.T) {
	cases := []string{
		`{"code":0,"data":[{"id":"deepseek-v4-flash"},{"model":"glm-5.2"}]}`,
		`{"models":["deepseek-v4-flash","glm-5.2"]}`,
		`[{"id":"deepseek-v4-flash"},{"id":"glm-5.2"}]`,
	}
	for _, raw := range cases {
		models := parseWorkBuddyModelCatalog([]byte(raw))
		if len(models) != 2 || models[0] != "deepseek-v4-flash" || models[1] != "glm-5.2" {
			t.Fatalf("parse %s => %v", raw, models)
		}
	}
}

// 渠道过滤器：只收 WorkBuddy 账号；白名单模型需匹配（下游可带 cn: 前缀）。
func TestWorkBuddyChannelAccountFilter(t *testing.T) {
	account := newWorkBuddyTestAccount("http://x")
	account.Models = []string{"deepseek-v4-flash"}
	filter := workbuddyChannelAccountFilter("cn:deepseek-v4-flash")
	if !filter(account) {
		t.Fatal("expected workbuddy account with matching model to pass")
	}
	if workbuddyChannelAccountFilter("glm-5.2")(account) {
		t.Fatal("model outside whitelist must not pass")
	}
	codex := &auth.Account{DBID: 1, UpstreamType: "codex", AccessToken: "at"}
	if workbuddyChannelAccountFilter("")(codex) {
		t.Fatal("non-workbuddy account must not pass workbuddy filter")
	}
}

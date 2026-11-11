package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/codex2api/auth"
)

// WorkBuddy 上游适配器。
//
// 与 Codex / Grok / Antigravity 不同，WorkBuddy 上游只说 OpenAI Chat Completions：
//   - 请求：POST {base}/v2/chat/completions（SSE）
//   - 鉴权：Authorization: Bearer <accessToken> + X-User-Id / X-Domain / X-CodeBuddy-Request
//   - 刷新：POST {base}/v2/plugin/auth/token/refresh，头 X-Refresh-Token
//
// 因此渠道只对 /v1/chat/completions 与 /v1/models 开放；Responses / Anthropic
// Messages 走协议翻译不在本期范围，入口处显式拒绝而不是静默降级。
const (
	workBuddyChatPath = auth.WorkBuddyChatPath

	// workBuddyTokenRefreshSkew 到期前多久主动刷新，避免请求正卡在过期点上。
	workBuddyTokenRefreshSkew = 5 * time.Minute
	// workBuddyChatTimeout 是单次上游聊天的整体上限；SSE 长输出由调用方 context 控制。
	workBuddyChatTimeout = 0
	// workBuddyAggregateLimit 是非流式聚合时允许的最大 SSE 字节数。
	workBuddyAggregateLimit = 64 << 20
)

// ExecuteWorkBuddyChatRequest 向 WorkBuddy 上游发起一次 Chat Completions 请求。
//
// 客户端请求流式时原样透传 SSE；请求非流式时强制上游流式，并在网关侧聚合为
// 单个 chat.completion（上游不保证支持非流式，聚合口径与 wb2api 一致）。
func ExecuteWorkBuddyChatRequest(ctx context.Context, account *auth.Account, inboundBody []byte, proxyOverride string, headers http.Header, levels ...int) (*http.Response, error) {
	if account == nil {
		return nil, errors.New("workbuddy: account is nil")
	}
	if strings.TrimSpace(account.WorkBuddyAccessToken()) == "" {
		return nil, errors.New("workbuddy: 账号缺少 access token，请重新登录授权")
	}
	clientStream := gjson.GetBytes(inboundBody, "stream").Bool()
	log.Printf("[WorkBuddy] EXEC account=%d base=%s model=%s stream=%v", account.ID(), account.WorkBuddyBaseURL(), gjson.GetBytes(inboundBody, "model").String(), clientStream)
	level := 0
	if len(levels) > 0 {
		level = levels[0]
	}
	body := prepareWorkBuddyChatBody(inboundBody)
	body = workBuddySanitizeBody(body, level)

	proxyURL := proxyOverride
	if strings.TrimSpace(proxyURL) == "" {
		proxyURL = account.ProxyURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, account.WorkBuddyBaseURL()+workBuddyChatPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyWorkBuddyChatHeaders(req, account, headers)

	if workBuddyFrameDebugEnabled() {
		keys := make([]string, 0, len(req.Header))
		for key := range req.Header {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var sb strings.Builder
		for _, key := range keys {
			value := req.Header.Get(key)
			if strings.EqualFold(key, "Authorization") && len(value) > 12 {
				value = value[:12] + "…"
			}
			sb.WriteString(key + "=" + value + "; ")
		}
		log.Printf("[WorkBuddy] REQ-HEADERS %s", sb.String())
	}
	client, err := auth.NewWorkBuddyHTTPClient(proxyURL, workBuddyChatTimeout)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	// 上游 4xx/5xx 的业务体（{"code":11102,"msg":"该后端无此模型"} 这类）是唯一
	// 可诊断的信息，必须落到日志里，否则客户端只看到笼统的 upstream_400。
	if resp.StatusCode >= http.StatusBadRequest {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if readErr == nil {
			log.Printf("[WorkBuddy] upstream %d %s level=%d req=%s body=%s", resp.StatusCode, req.URL.Path, level, truncateBodyForLog(workBuddyHead(body, 1200)), truncateBodyForLog(raw))
		}
		resp.Body = io.NopCloser(bytes.NewReader(raw))
	}
	log.Printf("[WorkBuddy] resp status=%d ct=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	if workBuddyFrameDebugEnabled() && resp.Body != nil {
		resp.Body = &workBuddyFrameTap{reader: resp.Body, limit: 2000}
	}
	if clientStream {
		// 上游经常不发终止帧（data: [DONE]）就关流，下游中继会把「没有终止事件」
		// 判成 upstream_stream_break。这里在 EOF 处补一个标准终止帧兜底。
		resp.Body = newWorkBuddyDoneAppendingBody(resp.Body)
		return resp, nil
	}
	return aggregateWorkBuddyStream(resp)
}

// applyWorkBuddyChatHeaders 注入 chat 出站头族。
func applyWorkBuddyChatHeaders(req *http.Request, account *auth.Account, downstream http.Header) {
	auth.ApplyWorkBuddyCommonHeaders(req, account)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+account.WorkBuddyAccessToken())
	uid := account.WorkBuddyUID()
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if domain := strings.TrimSpace(downstreamDomain(account)); domain != "" {
		req.Header.Set("X-Domain", domain)
	}
	// 官方客户端每次 user send 都带一组会话/请求追踪头，上游的通道校验会看它们。
	// 网关没有真实会话语义，用一次请求内恒定的随机 id 顶上（同一请求所有头同值）。
	if reqID := workBuddyRequestID(); reqID != "" {
		req.Header.Set("X-Request-ID", reqID)
		req.Header.Set("X-Conversation-Message-ID", reqID)
		req.Header.Set("X-Conversation-Request-ID", reqID)
		req.Header.Set("X-Root-Request-ID", reqID)
		req.Header.Set("X-Trace-ID", reqID)
		req.Header.Set("X-B3-TraceId", reqID)
		req.Header.Set("X-B3-SpanId", reqID[:16])
		req.Header.Set("X-B3-Sampled", "1")
	}
	// 客户端 x-request-id 透传，便于反查；不伪造会话聚合头。
	if downstream != nil {
		for _, key := range []string{"X-Request-Id", "X-Conversation-Id"} {
			if value := strings.TrimSpace(downstream.Get(key)); value != "" {
				req.Header.Set(key, value)
			}
		}
	}
}

// downstreamDomain 返回账号的 X-Domain：与官方客户端一致，国内声明 codebuddy.cn，
// 国际声明 workbuddy.ai。缺失该头同样会进 11128 通道校验失败分支。
func downstreamDomain(account *auth.Account) string {
	if account != nil && account.WorkBuddyDomain() == auth.WorkBuddyRealmGlobal {
		return "www.workbuddy.ai"
	}
	return "www.codebuddy.cn"
}

// prepareWorkBuddyChatBody 让上游恒走 SSE（非流式客户端由网关聚合），并修掉上游
// 会直接 400 的几种 OpenAI 扩展写法：
//   - 上游拒绝非流式：stream 强制 true
//   - 上游只认 max_tokens，OpenAI 新别名 max_completion_tokens 会被忽略后回落默认
//     上限，长输出被截 → 翻译成 max_tokens
//   - 上游 tool_choice 只接受字符串，对象形态（{"type":"function",...}）会 400
//     code=11101 → 降级成 "auto"
func prepareWorkBuddyChatBody(inboundBody []byte) []byte {
	body := inboundBody
	if !gjson.GetBytes(body, "stream").Bool() {
		body, _ = sjson.SetBytes(body, "stream", true)
	}
	if !gjson.GetBytes(body, "stream_options.include_usage").Bool() {
		body, _ = sjson.SetBytes(body, "stream_options.include_usage", true)
	}
	if alias := gjson.GetBytes(body, "max_completion_tokens"); alias.Exists() && alias.Type == gjson.Number {
		if !gjson.GetBytes(body, "max_tokens").Exists() && alias.Int() > 0 {
			body, _ = sjson.SetBytes(body, "max_tokens", alias.Int())
		}
		body, _ = sjson.DeleteBytes(body, "max_completion_tokens")
	}
	if choice := gjson.GetBytes(body, "tool_choice"); choice.Exists() && choice.IsObject() {
		body, _ = sjson.SetBytes(body, "tool_choice", "auto")
	}
	// 上游只认 system/user/assistant/tool 四个角色，OpenAI 新客户端把 system 写成
	// developer 会被风控判成「非法调用」直接回 11128（实测确定性复现，重试无效）。
	body = normalizeWorkBuddyMessageRoles(body)
	// 上游只认 low/medium/high；客户端的 off/none/minimal、max/xhigh 这类档位会 400。
	body = applyWorkBuddyDeepSeekThinking(body)
	if effort := gjson.GetBytes(body, "reasoning_effort"); effort.Exists() && effort.Type == gjson.String {
		switch strings.ToLower(strings.TrimSpace(effort.String())) {
		case "none", "off", "minimal", "disabled":
			body, _ = sjson.SetBytes(body, "reasoning_effort", "low")
		case "max", "maximum", "xhigh", "x-high", "ultra":
			body, _ = sjson.SetBytes(body, "reasoning_effort", "high")
		}
	}
	return body
}

// normalizeWorkBuddyMessageRoles 把客户端的新角色名映射回上游认识的四个角色。
// 目前只处理 OpenAI 的 developer → system（推理模型客户端普遍这么发）。
func normalizeWorkBuddyMessageRoles(body []byte) []byte {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body
	}
	for i, msg := range msgs.Array() {
		role := strings.ToLower(strings.TrimSpace(msg.Get("role").String()))
		if role == "developer" {
			body, _ = sjson.SetBytes(body, fmt.Sprintf("messages.%d.role", i), "system")
		}
	}
	return body
}

// workBuddySanitizeBody 按级别剥离客户端带来的「非必要字段」。
//
// 背景：部分客户端会带上 temperature/top_p/penalties/response_format 之类的
// 装饰性参数，上游风控会直接回 11128（Illegal API invocation from an unapproved
// channel）。level 2 去掉这些装饰字段，level 3 再去掉 thinking/reasoning_effort
// （保留 tools——工具调用是语义的一部分，不能为了绕过风控而丢掉）。
func workBuddySanitizeBody(body []byte, level int) []byte {
	if level < 2 {
		return body
	}
	for _, field := range []string{
		"temperature", "top_p", "top_k", "presence_penalty", "frequency_penalty",
		"logprobs", "top_logprobs", "seed", "stop", "user", "metadata",
		"response_format", "parallel_tool_calls", "logit_bias",
	} {
		body, _ = sjson.DeleteBytes(body, field)
	}
	if level < 3 {
		return body
	}
	for _, field := range []string{"thinking", "reasoning_effort"} {
		body, _ = sjson.DeleteBytes(body, field)
	}
	return body
}

// aggregateWorkBuddyStream 把上游 SSE 聚合成单个 chat.completion。
//
// 保留首帧的 id/model/created，逐帧合并 choices[0].delta 的 content / reasoning_content /
// tool_calls，末帧 usage 原样带出；上游给出非 2xx 时直接把原始响应交回调用方。
func aggregateWorkBuddyStream(resp *http.Response) (*http.Response, error) {
	if resp == nil {
		return nil, errors.New("workbuddy: empty upstream response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, nil
	}
	defer resp.Body.Close()

	var (
		id         string
		model      string
		created    int64
		content    strings.Builder
		reason     strings.Builder
		tools      []map[string]any
		usage      json.RawMessage
		finish     string
		firstFrame []byte
		frameCount int
	)
	reader := bufio.NewReaderSize(io.LimitReader(resp.Body, workBuddyAggregateLimit), 32*1024)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
				if !bytes.Equal(payload, []byte("[DONE]")) && len(payload) > 0 && json.Valid(payload) {
					frameCount++
					if firstFrame == nil {
						firstFrame = append([]byte(nil), payload...)
					}
					// 上游可能把正文放在 delta.content（字符串或分段数组）、
					// delta.reasoning_content、或非流式帧的 message.content 里，
					// 也可能整体包在 data 信封中；逐种取到为止。
					for _, path := range []string{"choices.0.delta.content", "choices.0.message.content", "data.choices.0.delta.content", "data.content", "content"} {
						if text := workBuddyTextValue(gjson.GetBytes(payload, path)); text != "" {
							content.WriteString(text)
						}
					}
					for _, path := range []string{"choices.0.delta.reasoning_content", "choices.0.delta.reasoning", "data.choices.0.delta.reasoning_content"} {
						if text := workBuddyTextValue(gjson.GetBytes(payload, path)); text != "" {
							reason.WriteString(text)
						}
					}
					if id == "" {
						id = gjson.GetBytes(payload, "id").String()
					}
					if model == "" {
						model = gjson.GetBytes(payload, "model").String()
					}
					if created == 0 {
						created = gjson.GetBytes(payload, "created").Int()
					}
					if frameUsage := gjson.GetBytes(payload, "usage"); frameUsage.Exists() && frameUsage.Raw != "null" {
						usage = json.RawMessage(frameUsage.Raw)
					}
					delta := gjson.GetBytes(payload, "choices.0.delta")
					if calls := delta.Get("tool_calls"); calls.IsArray() {
						tools = mergeWorkBuddyToolCalls(tools, calls.Array())
					}
					if reason_ := gjson.GetBytes(payload, "choices.0.finish_reason").String(); reason_ != "" {
						finish = reason_
					}
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, readErr
		}
	}

	if content.Len() == 0 && reason.Len() == 0 && frameCount > 0 {
		log.Printf("[WorkBuddy] 聚合后正文为空（frames=%d）首帧=%s", frameCount, truncateBodyForLog(firstFrame))
	}
	message := map[string]any{"role": "assistant"}
	if content.Len() > 0 {
		message["content"] = content.String()
	} else {
		message["content"] = ""
	}
	if reason.Len() > 0 {
		message["reasoning_content"] = reason.String()
	}
	if len(tools) > 0 {
		message["tool_calls"] = tools
	}
	if finish == "" {
		if len(tools) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	choice := map[string]any{"index": 0, "message": message, "finish_reason": finish}
	completionID := strings.TrimSpace(id)
	if completionID == "" {
		completionID = "chatcmpl-workbuddy"
	}
	payload := map[string]any{
		"id":      completionID,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{choice},
	}
	if len(usage) > 0 {
		payload["usage"] = usage
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	result := &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          io.NopCloser(bytes.NewReader(encoded)),
		ContentLength: int64(len(encoded)),
		Request:       resp.Request,
	}
	result.Header.Set("Content-Type", "application/json")
	return result, nil
}

// mergeWorkBuddyToolCalls 按 index 合并流式 tool_calls 增量（arguments 拼接）。
func mergeWorkBuddyToolCalls(existing []map[string]any, deltas []gjson.Result) []map[string]any {
	for _, delta := range deltas {
		index := int(delta.Get("index").Int())
		if index < 0 {
			continue
		}
		for len(existing) <= index {
			existing = append(existing, map[string]any{"index": len(existing), "type": "function", "function": map[string]any{"name": "", "arguments": ""}})
		}
		entry := existing[index]
		if id := delta.Get("id").String(); id != "" {
			entry["id"] = id
		}
		if kind := delta.Get("type").String(); kind != "" {
			entry["type"] = kind
		}
		fn, _ := entry["function"].(map[string]any)
		if fn == nil {
			fn = map[string]any{"name": "", "arguments": ""}
			entry["function"] = fn
		}
		if name := delta.Get("function.name").String(); name != "" {
			fn["name"] = name
		}
		if fragment := delta.Get("function.arguments").String(); fragment != "" {
			fn["arguments"] = strings.TrimSpace(fmt.Sprint(fn["arguments"])) + fragment
		}
	}
	return existing
}

// FetchWorkBuddyModelIDs 拉取账号可用模型目录。
//
// 上游 /console/enterprises/personal/models 的响应结构随版本变动，这里按
// 「data/models 两种容器 + 字符串/对象两种条目」宽容解析，解析不出即返回错误，
// 由调用方决定是否保留手工配置的模型列表。
func FetchWorkBuddyModelIDs(ctx context.Context, account *auth.Account, proxyOverride string) ([]string, error) {
	if account == nil {
		return nil, errors.New("workbuddy: account is nil")
	}
	proxyURL := proxyOverride
	if strings.TrimSpace(proxyURL) == "" {
		proxyURL = account.ProxyURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, account.WorkBuddyBaseURL()+auth.WorkBuddyModelsPath, nil)
	if err != nil {
		return nil, err
	}
	auth.ApplyWorkBuddyCommonHeaders(req, account)
	if token := account.WorkBuddyAccessToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if uid := account.WorkBuddyUID(); uid != "" {
		req.Header.Set("X-User-Id", uid)
	}
	client, err := auth.NewWorkBuddyHTTPClient(proxyURL, 30*time.Second)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("workbuddy 模型目录拉取失败: http %d: %s", resp.StatusCode, truncateBodyForLog(body))
	}
	models := parseWorkBuddyModelCatalog(body)
	if len(models) == 0 {
		return nil, fmt.Errorf("workbuddy 模型目录为空或结构无法识别")
	}
	return models, nil
}

// parseWorkBuddyModelCatalog 宽容解析模型目录：先看 data，再看 models / 顶层数组。
func parseWorkBuddyModelCatalog(body []byte) []string {
	candidates := []string{"data", "models", "data.models", "data.list", "result"}
	for _, path := range candidates {
		if node := gjson.GetBytes(body, path); node.Exists() {
			if models := collectWorkBuddyModelIDs(node); len(models) > 0 {
				return models
			}
		}
	}
	if gjson.ParseBytes(body).IsArray() {
		return collectWorkBuddyModelIDs(gjson.ParseBytes(body))
	}
	return nil
}

func collectWorkBuddyModelIDs(node gjson.Result) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, 16)
	appendID := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		key := strings.ToLower(id)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		result = append(result, id)
	}
	if !node.IsArray() {
		return result
	}
	for _, item := range node.Array() {
		if item.Type == gjson.String {
			appendID(item.String())
			continue
		}
		for _, key := range []string{"id", "model", "model_id", "modelId", "name"} {
			if value := item.Get(key); value.Exists() && value.Type == gjson.String {
				appendID(value.String())
				break
			}
		}
	}
	return result
}

// workBuddyFrameDebugEnabled 判断是否开启原始帧转储（排查上游协议差异用）。
// 打开方式：容器环境变量 WORKBUDDY_DEBUG_FRAMES=1。
func workBuddyFrameDebugEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("WORKBUDDY_DEBUG_FRAMES")))
	return v == "1" || v == "true" || v == "yes"
}

// workBuddyFrameTap 抓取上游响应的前 limit 字节并打一行日志，其余数据原样透传。
// 只在 WORKBUDDY_DEBUG_FRAMES=1 时挂载，不改变正常路径行为。
type workBuddyFrameTap struct {
	reader   io.Reader
	buf      []byte
	firstLen int
	limit    int
	logged   bool
}

func (t *workBuddyFrameTap) Read(p []byte) (int, error) {
	n, err := t.reader.Read(p)
	if n > 0 {
		if room := 16384 - len(t.buf); room > 0 {
			take := n
			if take > room {
				take = room
			}
			t.buf = append(t.buf, p[:take]...)
		}
		if !t.logged {
			t.logged = true
			t.firstLen = len(t.buf)
			log.Printf("[WorkBuddy] WORKBUDDY-RAW-FIRST (%d bytes) %q", t.firstLen, workBuddyHead(t.buf, 900))
		}
	}
	if err != nil {
		t.dumpRemainder()
	}
	return n, err
}

func (t *workBuddyFrameTap) dumpRemainder() {
	if len(t.buf) > t.firstLen {
		log.Printf("[WorkBuddy] WORKBUDDY-RAW-REST (total=%d) %q", len(t.buf), workBuddyHead(t.buf[t.firstLen:], 900))
		t.firstLen = len(t.buf)
	}
}

func (t *workBuddyFrameTap) Close() error {
	t.dumpRemainder()
	if closer, ok := t.reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// workBuddyHead 取前 n 字节，供 %q 转义打印，便于看清事件分隔符。
func workBuddyHead(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// workBuddyDoneReader 在 EOF 处补一条 `data: [DONE]`（上游偶尔不发终止帧），
// 但如果上游自己已经发过 [DONE] 就不再补，避免出现两条终止帧。
type workBuddyDoneReader struct {
	reader io.Reader
	tail   [64]byte
	tailN  int
	eof    bool
	done   bool
}

func newWorkBuddyDoneAppendingBody(reader io.Reader) io.ReadCloser {
	return &workBuddyDoneReader{reader: reader}
}

func (r *workBuddyDoneReader) Read(p []byte) (int, error) {
	if r.eof {
		return 0, io.EOF
	}
	n, err := r.reader.Read(p)
	if n > 0 {
		r.rememberDone(p[:n])
		return n, nil
	}
	if err == io.EOF {
		r.eof = true
		if r.done {
			return 0, io.EOF
		}
		return copy(p, []byte("\n\ndata: [DONE]\n\n")), nil
	}
	return n, err
}

func (r *workBuddyDoneReader) rememberDone(chunk []byte) {
	if r.done {
		return
	}
	if len(chunk) >= len(r.tail) {
		copy(r.tail[:], chunk[len(chunk)-len(r.tail):])
		r.tailN = len(r.tail)
	} else {
		keep := r.tailN + len(chunk) - len(r.tail)
		if keep > 0 {
			copy(r.tail[:], r.tail[keep:r.tailN])
			r.tailN -= keep
		}
		copy(r.tail[r.tailN:], chunk)
		r.tailN += len(chunk)
	}
	if bytes.Contains(r.tail[:r.tailN], []byte("[DONE]")) {
		r.done = true
	}
}

func (r *workBuddyDoneReader) Close() error {
	if closer, ok := r.reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// applyWorkBuddyDeepSeekThinking 对齐官方客户端对 deepseek 系模型的出站要求：
// 必须显式 thinking:{type:"enabled"} + 一个 reasoning_effort 档位，否则上游按
// 「不思考」应答（实测表现为 delta 全空、无正文）。同时给 assistant 消息补
// reasoning_content 字段（官方 requiresReasoningContentOnAssistantMessages）。
func applyWorkBuddyDeepSeekThinking(body []byte) []byte {
	model := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "model").String()))
	if !strings.HasPrefix(model, "deepseek") {
		return body
	}
	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	if thinkingType == "" {
		body, _ = sjson.SetBytes(body, "thinking", map[string]any{"type": "enabled"})
		thinkingType = "enabled"
	}
	if thinkingType != "enabled" {
		return body
	}
	if !gjson.GetBytes(body, "reasoning_effort").Exists() {
		body, _ = sjson.SetBytes(body, "reasoning_effort", "high")
	}
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body
	}
	for i, msg := range msgs.Array() {
		if !strings.EqualFold(strings.TrimSpace(msg.Get("role").String()), "assistant") {
			continue
		}
		if msg.Get("reasoning_content").Type != gjson.String {
			body, _ = sjson.SetBytes(body, fmt.Sprintf("messages.%d.reasoning_content", i), "")
		}
	}
	return body
}

// workBuddyTextValue 从 gjson 节点取纯文本：字符串直接返回；分段数组
// （[{"type":"text","text":"..."}]）拼接 text 字段。
func workBuddyTextValue(node gjson.Result) string {
	if !node.Exists() {
		return ""
	}
	if node.Type == gjson.String {
		return node.String()
	}
	if node.IsArray() {
		var sb strings.Builder
		for _, part := range node.Array() {
			if part.Type == gjson.String {
				sb.WriteString(part.String())
				continue
			}
			if text := part.Get("text"); text.Type == gjson.String {
				sb.WriteString(text.String())
			}
		}
		return sb.String()
	}
	return ""
}

// workBuddyRequestID 生成 32 位十六进制请求标识（与官方客户端 X-Request-ID 同形）。
func workBuddyRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

func truncateBodyForLog(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		return text[:300] + "..."
	}
	return text
}

// IsWorkBuddySessionDeadError 识别 refresh token 彻底失效（需要用户重新登录）。
func IsWorkBuddySessionDeadError(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "offline user session not found") || strings.Contains(lower, "12153")
}

// WorkBuddyUpstreamErrorMessage 从上游错误体里抠出可读原因：
// {"code":11102,"msg":"该后端无此模型"} → `上游 11102: 该后端无此模型`。
// 解析不出时回退到原文（截断）。
func WorkBuddyUpstreamErrorMessage(body []byte) string {
	code := gjson.GetBytes(body, "code").Int()
	msg := strings.TrimSpace(firstGJSONString(body, "msg", "message", "error.message", "error"))
	if msg != "" {
		if code != 0 {
			return fmt.Sprintf("上游 %d: %s", code, msg)
		}
		return "上游: " + msg
	}
	if code == 11128 {
		// 上游把「模型不属于该账号/该通道」也归到 11128（unapproved channel），
		// 客户端只看到「请求被拦截」会误判成账号问题，这里补一句可执行的提示。
		return "上游 11128: Illegal API invocation from an unapproved channel（请求被拦截）——" +
			"最常见原因是模型名不在该账号目录内，请改用 /v1/models 返回的模型（如 deepseek-v4-pro）"
	}
	if code != 0 {
		return fmt.Sprintf("上游返回错误码 %d", code)
	}
	return truncateBodyForLog(body)
}

// ensureWorkBuddyToken 在 access token 即将过期（或缺失）时用 refresh token 换新，
// 并把新凭据写回数据库与内存。返回是否真的发生了刷新。
//
// 刷新失败不返回错误给调用方：请求继续打上游，由 401 走既有冷却/换号；
// 失败原因写日志，refresh token 失效（12153）时提示需要重新登录。
func (h *Handler) ensureWorkBuddyToken(ctx context.Context, account *auth.Account, proxyURL string) bool {
	if h == nil || account == nil || !account.IsWorkBuddy() {
		return false
	}
	accessToken := account.WorkBuddyAccessToken()
	expiresAt := account.WorkBuddyExpiresAt()
	if accessToken != "" && (expiresAt.IsZero() || time.Until(expiresAt) > workBuddyTokenRefreshSkew) {
		return false
	}
	if strings.TrimSpace(account.RefreshToken) == "" {
		log.Printf("[WorkBuddy] 账号 %d 缺少 refresh token，无法刷新 access token", account.ID())
		return false
	}
	token, err := auth.RefreshWorkBuddyToken(ctx, account, proxyURL)
	if err != nil {
		log.Printf("[WorkBuddy] 账号 %d token 刷新失败: %v", account.ID(), err)
		return false
	}
	account.SetWorkBuddyToken(token.AccessToken, token.RefreshToken, token.ExpiresAt)
	if h.db != nil {
		updates := map[string]interface{}{
			"api_key":                               token.AccessToken,
			auth.WorkBuddyRefreshTokenCredentialKey: token.RefreshToken,
			auth.WorkBuddyExpiresAtCredentialKey:    token.ExpiresAt.UTC().Format(time.RFC3339),
		}
		persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.db.UpdateCredentials(persistCtx, account.ID(), updates); err != nil {
			log.Printf("[WorkBuddy] 账号 %d 刷新后落库失败: %v", account.ID(), err)
		}
	}
	log.Printf("[WorkBuddy] 账号 %d access token 已刷新，有效期至 %s", account.ID(), token.ExpiresAt.Format(time.RFC3339))
	return true
}

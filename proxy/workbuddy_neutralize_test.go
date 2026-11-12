package proxy

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNeutralizeWorkBuddyBlockedSignature(t *testing.T) {
	body := []byte(`{
	  "model": "deepseek-v4.1-flash",
	  "messages": [
	    {"role": "system", "content": "You are an AI agent powered by DeepSeek Harness.\n\nRelative paths resolve from the workspace root."},
	    {"role": "user", "content": "我用的是 DeepSeek Harness 客户端"},
	    {"role": "tool", "content": "tool output mentions deepseek harness and Workspace Root"}
	  ],
	  "tools": [
	    {"type": "function", "function": {"name": "read", "description": "由 DeepSeek Harness 提供的读文件工具"}}
	  ]
	}`)

	got := string(neutralizeWorkBuddyBlockedSignature(body))

	// 1) system 消息与工具描述（客户端生成的部分）必须清洗。
	system := gjson.Get(got, "messages.0.content").String()
	if strings.Contains(system, "DeepSeek Harness") || strings.Contains(system, "workspace root") {
		t.Fatalf("system 消息仍残留指纹:\n%s", system)
	}
	if !strings.Contains(system, "the CLI") || !strings.Contains(system, "project directory") {
		t.Fatalf("system 消息未按预期改写:\n%s", system)
	}
	desc := gjson.Get(got, "tools.0.function.description").String()
	if strings.Contains(desc, "DeepSeek Harness") || !strings.Contains(desc, "the CLI") {
		t.Fatalf("工具描述未按预期改写: %s", desc)
	}

	// 2) user / tool 正文原样保留（用户可能在对话里正当地提到这个名字）。
	if user := gjson.Get(got, "messages.1.content").String(); user != "我用的是 DeepSeek Harness 客户端" {
		t.Fatalf("user 消息被误改: %s", user)
	}
	if tool := gjson.Get(got, "messages.2.content").String(); !strings.Contains(tool, "deepseek harness") {
		t.Fatalf("tool 消息被误改: %s", tool)
	}

	// 3) 模型名保持原样。
	if gjson.Get(got, "model").String() != "deepseek-v4.1-flash" {
		t.Fatalf("模型名被误改: %s", gjson.Get(got, "model").String())
	}
}

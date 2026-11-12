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

	for _, banned := range []string{"DeepSeek Harness", "deepseek harness", "workspace root", "Workspace Root"} {
		if strings.Contains(got, banned) {
			t.Fatalf("仍然残留指纹 %q:\n%s", banned, got)
		}
	}
	if strings.Count(got, "the CLI") < 3 {
		t.Fatalf("应至少替换 3 处身份标识，实际:\n%s", got)
	}
	if !strings.Contains(got, "project directory") {
		t.Fatalf("workspace root 未替换:\n%s", got)
	}
	// 模型名与正常正文必须保持原样。
	if gjson.Get(got, "model").String() != "deepseek-v4.1-flash" {
		t.Fatalf("模型名被误改: %s", gjson.Get(got, "model").String())
	}
	if !strings.Contains(got, "我用的是 the CLI 客户端") {
		t.Fatalf("user 消息未按预期改写:\n%s", got)
	}
}

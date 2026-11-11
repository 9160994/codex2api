package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tidwall/gjson"
)

// workBuddyShiftMemo 记住「这套 system 提示词会被上游判 11128、必须降级成 user 消息」
// 的结论，键是 system 文本的哈希。命中后第一次尝试直接用 level=5，省掉前面几次
// 必然失败的重试（每次失败 ≈ 0.4s 外加一遍上游风控计数）。
var (
	workBuddyShiftMemo    sync.Map // key -> struct{}
	workBuddyShiftMemoLen int32
)

const workBuddyShiftMemoLimit = 512

// workBuddySystemMemoKey 取所有 system/developer 消息内容的哈希；没有 system 消息返回空串。
func workBuddySystemMemoKey(body []byte) string {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return ""
	}
	var sb strings.Builder
	for _, msg := range msgs.Array() {
		role := strings.ToLower(strings.TrimSpace(msg.Get("role").String()))
		if role != "system" && role != "developer" {
			continue
		}
		content := msg.Get("content")
		if content.Type == gjson.String {
			sb.WriteString(content.String())
		}
		if sb.Len() > 8192 {
			break
		}
	}
	if sb.Len() == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:12])
}

func workBuddyShiftMemoHit(key string) bool {
	if key == "" {
		return false
	}
	_, ok := workBuddyShiftMemo.Load(key)
	return ok
}

func workBuddyShiftMemoRemember(key string) {
	if key == "" {
		return
	}
	if _, loaded := workBuddyShiftMemo.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	if atomic.AddInt32(&workBuddyShiftMemoLen, 1) > workBuddyShiftMemoLimit {
		// 简单清理：超限时整体丢弃，重建成本很低（每个会话一次）
		workBuddyShiftMemo.Range(func(k, _ any) bool {
			workBuddyShiftMemo.Delete(k)
			return true
		})
		atomic.StoreInt32(&workBuddyShiftMemoLen, 0)
	}
}

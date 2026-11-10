# WorkBuddy 渠道

把腾讯 **WorkBuddy / CodeBuddy 订阅账号**接入 codex2api，作为第五个上游渠道
（`codex` / `claude` / `antigravity` / `grok` / `workbuddy`）。

## 1. 能力边界（先看这一段）

| 网关端点 | 支持 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | ✅ | 流式透传；非流式由网关把上游 SSE 聚合为 `chat.completion` |
| `GET /v1/models` | ✅ | 返回账号配置/同步到的模型目录；空白名单时不返回任何 workbuddy 模型 |
| `POST /v1/responses` | ❌ 400 | 上游没有 Responses 语义，返回 `workbuddy_chat_only` |
| `POST /v1/messages`（Anthropic） | ❌ 400 | 同上 |
| Images / Videos / Live | ❌ | 未实现 |

WorkBuddy 账号**只能被绑定了 workbuddy 渠道的 API Key 调度**：不限定渠道的 Key
会把它们排除在候选之外，避免 Codex 请求误落到 chat-only 上游。

## 2. 上游契约（实测口径）

| 项 | 值 |
|---|---|
| Chat | `POST {base}/v2/chat/completions`（SSE） |
| 刷新 | `POST {base}/v2/plugin/auth/token/refresh`，请求头 `X-Refresh-Token`、`X-Auth-Refresh-Source: plugin` |
| 模型目录 | `GET {base}/console/enterprises/personal/models` |
| base（cn） | `https://copilot.tencent.com` |
| base（global） | `https://www.workbuddy.ai` |
| 鉴权头 | `Authorization: Bearer <accessToken>`、`X-User-Id: <uid>` |
| 客户端头 | `User-Agent: CLI/2.63.2 CodeBuddy/2.63.2`、`X-CodeBuddy-Request: 1`、`Origin/Referer` 按域 |
| 设备头 | `X-Machine-ID` / `X-Session-ID`：按 uid SHA-256 派生，跨重启稳定、账号间互异 |

- 刷新响应：`{"code":0,"data":{"accessToken","refreshToken","expiresIn"}}`；`expiresIn` 缺省按 3600s。
- 业务错误码：`12153`（`Offline user session not found`）= refresh token 彻底失效，只能重新登录授权。
- 网关在 access token 距过期 < 5 分钟时惰性刷新一次，并把新 token 写回
  `accounts.credentials`（`api_key` / `workbuddy_refresh_token` / `workbuddy_expires_at`）。

## 3. 账号凭据结构

`accounts` 行：`platform = 'workbuddy'`、`type = 'oauth'`，`credentials` 为：

```json
{
  "upstream_type": "workbuddy",
  "base_url": "https://copilot.tencent.com",
  "api_key": "<accessToken>",
  "models": ["deepseek-v4-flash"],
  "workbuddy_uid": "<uid>",
  "workbuddy_refresh_token": "<refreshToken>",
  "workbuddy_expires_at": "2026-11-15T14:00:00Z",
  "workbuddy_realm": "cn",
  "workbuddy_nickname": "冬",
  "workbuddy_enterprise_id": "",
  "plan_type": "workbuddy"
}
```

## 4. 管理接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/admin/accounts/workbuddy` | 账号列表（uid / realm / 状态 / 模型 / 过期时间） |
| POST | `/api/admin/accounts/workbuddy` | 新增（按 uid 覆盖） |
| POST | `/api/admin/accounts/workbuddy/import` | 批量导入，接受 `{items:[...]}` 或 `{text:"<wb2api auth JSON>"}` |
| POST | `/api/admin/accounts/workbuddy/delete` | `{ids:[...]}` 软删除 |
| POST | `/api/admin/accounts/workbuddy/models` | 拉取上游模型目录并写回账号 |
| PATCH | `/api/admin/accounts/:id/workbuddy` | 名称 / 代理 / token / 过期时间 |
| POST | `/api/admin/accounts/:id/workbuddy/test` | 用真实凭据打一次最小 chat（诊断用） |
| POST | `/api/admin/accounts/:id/workbuddy/refresh` | 强制刷新 access token |

导入兼容 wb2api 的 `auths/workbuddy-<uid>.json`：

```json
{
  "account": {"uid": "...", "nickname": "...", "enterpriseId": ""},
  "auth": {"accessToken": "...", "refreshToken": "...", "expiresAt": 1794751200, "domain": "www.codebuddy.cn", "realm": "cn"}
}
```

启停、模型编辑复用通用接口：`POST /api/admin/accounts/:id/enable`、`PATCH /api/admin/accounts/:id/models`。

## 5. 典型报错

| 现象 | 含义 | 处置 |
|---|---|---|
| `401 workbuddy_credentials_expired` | 池内账号全在 unauthorized/cooldown | 重新登录授权，导入新 token |
| `400 workbuddy_chat_only` | 用了 `/v1/responses` 或 `/v1/messages` | 改用 `/v1/chat/completions` |
| `503 no_available_account` | 没有绑定 workbuddy 渠道的可用账号 | 检查 Key 的 `upstream_channel` 与账号状态 |
| 上游 `401 Authorization Required`（APISIX 页面） | access token 被上游拒绝 | 先试 `refresh`，`12153` 则必须重新登录 |

## 6. 风险

上游是腾讯 CodeBuddy CLI / 插件使用的**非公开接口**，任何 `/v1` 形态的第三方网关
都属于带外 API 使用，可能导致账号被风控或封禁；接口随时可能变更。仅在自有账号、
自担风险的前提下使用。

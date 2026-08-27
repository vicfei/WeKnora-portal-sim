# WeKnora-portal-sim（v2 路线 · 门户替身）

**角色定位**：在 v2 路线里扮演"未来 Java 门户"中与 WeKnora 相关的最小职责——SSO 登录入口 + bridge 换票 + 302 回跳前端（决策 022 的 `#bridge_result` 契约）。**自身零权限逻辑**：不做任何过滤/裁决，一切由 WeKnora 完成（与 B1 路线的 WeKnora-portal-proxy 形成架构对照：智能在平台 vs 智能在代理）。

```
浏览器 ──登录──▶ portal-sim(:8082) ──bridge(无tenant_id, platform key)──▶ WeKnora(:8080)
   ◀──302 #bridge_result=...──┘                    （个人空间+grants 全在 WeKnora 落地）
浏览器 ──▶ WeKnora 前端(:5173，真实 JWT 直调)
浏览器 ──/api/* /auth/*──▶ portal-sim 透明反代 ──▶ WeKnora（无过滤直通）
```

## 端点

| 路径 | 作用 |
|---|---|
| `GET /` | SSO 模拟登录页（员工源=portal_proxy.employees 只读，与 B1 同一批测试账号） |
| `POST /sso/authorize` | 校验工号密码 → POST /identity/bridge（无 tenant_id）→ 302 `{FRONTEND_URL}/#bridge_result=base64url({token,refresh_token})` |
| `GET /admin` | v2 管理台页面（经本服务反代调 WeKnora /api/v1/admin/*，管理员 JWT） |
| `/api/*`、`/auth/*` | 透明反向代理到 WeKnora（零过滤——权限全在 WeKnora 侧） |
| `GET /healthz` | 健康检查 |

## 配置（env，见 .env.example）

- `SIM_ADDR`（默认 :8082）、`WEKNORA_BASE_URL`、`WEKNORA_FRONTEND_URL`（回跳目标）
- `WEKNORA_PLATFORM_KEY`（bridge 用，服务器侧持有，不下发浏览器）
- `PORTAL_DB_DSN`（只读 portal_proxy.employees）

## 纪律

1. 本服务**永远不出现权限判断代码**——出现即说明 v2 架构被破坏；
2. platform key 只在服务端 env，不进任何响应/页面；
3. 测试账号与 B1 共用（REVIEW-*，密码 review123456）。

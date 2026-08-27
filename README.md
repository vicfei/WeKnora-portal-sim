# WeKnora-portal-sim（v2 路线 · 门户替身）

**角色定位**（2026-08-27 升级为"B1 皮 + v2 芯"）：承载与 B1 路线**同款的门户三页**（登录/知识库/问答），但接线全走 v2——真实 JWT（bridge 换票）、权限裁决全部在 WeKnora（grants 引擎 + 逐请求 RBAC）、`/api/*` 透明反代零过滤。与 WeKnora-portal-proxy 构成同 UI 不同架构的 A/B 对照，另保留 `/sso/native` 进入 WeKnora 原生前端的对照路径。

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
| `POST /sso/authorize` | 校验 → 无 tenant_id bridge → 建 cookie 会话（存真实 JWT）→ 302 `/kb` |
| `GET /kb`、`GET /kb/{id}`、`GET /chat` | **B1 同款门户三页（v2 接线）**：分组列表/详情检索上传/流式问答（含模型选择器） |
| `GET /sso/native?uum_user_id=` | 对照演示：bridge → `#bridge_result` → WeKnora 原生前端 |
| `GET /admin` | v2 管理台（反代调 /api/v1/admin/*，管理员 JWT） |
| `/api/*` | 透明反代（注入会话 Authorization、SSE 即时 Flush；**零过滤**） |
| `GET /healthz` | 健康检查 |

## 配置（env，见 .env.example）

- `SIM_ADDR`（默认 :8082）、`WEKNORA_BASE_URL`、`WEKNORA_FRONTEND_URL`（回跳目标）
- `WEKNORA_PLATFORM_KEY`（bridge 用，服务器侧持有，不下发浏览器）
- `PORTAL_DB_DSN`（只读 portal_proxy.employees）

## 纪律

1. 本服务**永远不出现权限判断代码**——出现即说明 v2 架构被破坏；
2. platform key 只在服务端 env，不进任何响应/页面；
3. 测试账号与 B1 共用（REVIEW-*，密码 review123456）。

# WeKnora-portal-sim（v2 路线 · 门户替身）

**角色定位**（2026-08-27 升级为"B1 皮 + v2 芯"）：承载与 B1 路线**同款的门户三页**（登录/知识库/问答），但接线全走 v2——真实 JWT（bridge 换票）、权限裁决全部在 WeKnora（grants 引擎 + 逐请求 RBAC）、`/api/*` 透明反代零过滤。与 WeKnora-portal-proxy 构成同 UI 不同架构的 A/B 对照，另保留 `/sso/native` 进入 WeKnora 原生前端的对照路径。

**UI**（2026-08-27 按原型设计重构）：品牌「智研助手 Research Copilot」，light tech 设计体系（电蓝/青/紫三色渐变 tokens，Sora + Noto Sans SC + JetBrains Mono；字体走 Google Fonts CDN，内网加载失败自动回退系统字体）。

```
浏览器 ──登录──▶ portal-sim(:8082) ──bridge(无tenant_id, platform key)──▶ WeKnora(:8080)
   ◀──302──┘                        （个人空间+grants 全在 WeKnora 落地）
浏览器 ──▶ WeKnora 前端(:5173，真实 JWT 直调，对照路径)
浏览器 ──/api/* /auth/*──▶ portal-sim 透明反代 ──▶ WeKnora（无过滤直通）
```

## 端点

| 路径 | 作用 |
|---|---|
| `GET /` | 登录页（原型双栏设计；员工源=`idp_sim.employees` 只读测试替身） |
| `POST /sso/authorize` | 校验 → 无 tenant_id bridge → 建 cookie 会话（存真实 JWT）→ 302 `/kb` |
| `GET /kb` | 知识库门户：hero 统计（真实聚合）+ 分组 tabs（个人/团队/公司公共/共享给我）+ 搜索 |
| `GET /kb/{id}` | 知识库详情：混合检索 + 文档表格（分页/解析状态自动刷新/预览/下载/标签过滤）+ 上传（进度/类型校验/**上传后自动弹打标签**） |
| `GET /chat` | 智能问答：会话历史（加载/置顶/删除）+ 模型选择 + KB 多选 popover + SSE 流式 + 引用来源右栏 + 停止生成 |
| `GET /sso/native?uum_user_id=` | 对照演示：bridge → `#bridge_result` → WeKnora 原生前端 |
| `GET /admin` | v2 管理台（反代调 /api/v1/admin/*，管理员 JWT；授权行编辑/按空间过滤） |
| `/api/*` | 透明反代（注入会话 Authorization、SSE 即时 Flush；**零过滤**） |
| `GET /healthz` | 健康检查 |

## 会话生命周期

- cookie 会话**落盘持久化**（`SIM_SESSION_FILE`，0600，原子写），sim 重启后用户无感续用
- bridge token（24h）临期 5 分钟内首次请求**透明重 bridge**（bridge 幂等，WeKnora 每次换票重新裁决空间与角色）；失败则沿用旧 token 由平台 401
- 会话绝对寿命 30 天（cookie 同步），过期重新登录

## 配置（env，见 .env.example）

- `SIM_ADDR`（默认 :8082）、`WEKNORA_BASE_URL`、`WEKNORA_FRONTEND_URL`（回跳目标）
- `WEKNORA_PLATFORM_KEY`（bridge 用，服务器侧持有，不下发浏览器）
- `PORTAL_DB_DSN`（只读 `idp_sim.employees` —— 公司统一身份源的测试替身，见下节）
- `SIM_SESSION_FILE`（会话落盘路径；留空 = 纯内存，重启即失）

## 测试数据

**员工账号源 = `fixtures/idp_sim.sql`**（本仓库自带的测试夹具，架构定位：模拟生产环境的 UM/KIP/SSO 身份体系；portal-sim 对其只读做登录校验，不参与任何授权判断）：

```bash
# 初始化（幂等，可重复执行）
sudo docker exec -i WeKnora-postgres-dev psql -U postgres < fixtures/idp_sim.sql
```

| 工号 | 密码 | 授权画像（在 WeKnora 侧，与本表无关） |
|---|---|---|
| REVIEW-U0001 | review123456 | 个人 owner + 团队 contributor + 公共库 admin（主力演示） |
| REVIEW-U0002 | review123456 | 团队 viewer + 公共库经组织共享（受限对照） |
| REVIEW-A0001 / REVIEW-U0003 | review123456 | 无授权（冷启动 / 预授权演示对象） |

管理台账号 `review-v2-admin@test.local`（密码同上）是 WeKnora 系统管理员，经 `/admin` 页走 WeKnora `/auth/login`，不在员工表。

> 历史说明：账号数据从 B1 时代的 `portal_proxy.employees` 原样迁出（bcrypt 哈希未变，密码不变）；`portal_proxy` 库属已冻结的 B1 实物，保留不动。表内 `is_admin` 列为 B1 遗留字段，C 路线完全忽略。

## 纪律

1. 本服务**永远不出现权限判断代码**——出现即说明 v2 架构被破坏；分组/角色只是呈现服务端字段
2. platform key 与 JWT 只在服务端，不进任何响应/页面/日志
3. 测试账号与 B1 共用（REVIEW-U0001 / REVIEW-U0002 等，密码 review123456，见「测试数据」节）

## 已知边界（设计稿 ↔ 后端现状）

- **KB 范围请求的成员视角**：WeKnora 按 token 激活空间解析 KB 权限，跨空间 membership 需 `X-Tenant-ID` 切换才生效（auth.go 原生机制，切换合法性由平台校验 membership）。详情页对**有 my_role（membership）**的 KB 统一带此头；组织共享只读用户不带，走共享解析——两条路径的读/写边界均实测正确
- 管理台**审计页**：设计稿 §3.1-9 的 `GET /api/v1/admin/audit`（系统级）在 WeKnora 侧未实现；现有 `/tenants/{id}/audit-log` 对系统管理员不可跨租户访问（实测 403）——待后端补端点后接入
- 授权编辑**无法改回永久**：`PUT /admin/grants/{id}` 的 `valid_until` 为 `*time.Time`，JSON null 视为"未提供"
- KB 卡片**chunk_count**：portal 列表/详情接口对文档型 KB 不填 chunk_count（仅 FAQ 型），卡片显示 knowledge_count 口径
- 「深度思考 / 联网搜索」pill 为置灰占位（WeKnora 原生能力，门户 POC 未接线）
- 登录页「忘记密码」等为置灰占位；UM 切换仅文案变化（POC 单一员工账号源）

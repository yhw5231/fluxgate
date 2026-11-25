# 整合重构说明：UniGate + Fluxgate → Fluxgate

本文件记录 `D:\NET\ai\unigate`（UniGate，原 cline2api-go）与 `D:\NET\ai\fluxgate`（Fluxgate）
两个 Go 网关项目的整合方案、功能归属与落地进度。整合后以 **fluxgate** 为主工程，仓库与
发布都落在 `https://github.com/yhw5231/fluxgate`。

## 1. 整合原则

1. **以 Fluxgate 的工程骨架为准**：`cmd/fluxgate` + `internal/*` 分层、SQLite 配置库
   （`hub.db` 的 sites/accounts/account_tokens/token_routes/route_channels 等表）、
   通用资源 CRUD 管理 API、内嵌管理台（`internal/console/static`）。UniGate 的单包
   平铺结构与 `gateway.json` 配置文件**不再保留**。
2. **吸收 UniGate 的独有能力**，按 Fluxgate 的分层重写为独立包：
   客户端指纹预设、用量统计、账号探测、内部渠道固定、IPv6 代理池、错误/请求日志保留策略等。
3. **数据面统一到一条 SQLite 文件**：Fluxgate 的 `gateway_*` 前缀表（breaker、请求日志、
   media jobs，以及后续的用量/探测/固定渠道）与上游配置表同库但互不冲突；UniGate 的
   `gateway.json` / `cooldowns.json` / `lease-assignments.json` 一律改为库表，消除
   「配置一半在 JSON、一半在 SQLite」的旧结构。
4. **前端按功能重新组织页面**：不再沿用任一项目的平铺标签页，按「监控 / 渠道 / 访问 /
   系统」四条主线布局（详见第 5 节）。

## 2. 架构映射（UniGate → Fluxgate）

| UniGate | Fluxgate | 说明 |
| --- | --- | --- |
| `Channel`（渠道） | `sites` + `upstreams` 复合资源 | 一个上游 = 站点 + 账号 + 若干 token + 由模型派生的 route/channel |
| `Key`（渠道 key） | `account_tokens` 行（线路） | 每个 key 一条线路，独立代理、独立冷却 |
| `GWKey`（通用密钥） | `downstream_api_keys` | 新增的限定路由/上游/排除项沿用 Fluxgate 既有实现 |
| `Pool`（代理池） | 新增 `proxy_pools` 资源 | key/线路只引用池，连接信息集中管理 |
| `gateway.json` | SQLite 表 | 配置写库，重启不丢、可事务化 |
| `cooldowns.json` | `gateway_breaker_states` | Fluxgate 已实现熔断/冷却持久化 |
| `usage.db` | 同库 `gateway_usage_events` | 用量与请求日志共用一条连接，减少多写者 |

## 3. 功能对照表

| 能力 | UniGate | Fluxgate（整合前） | 整合决定 |
| --- | --- | --- | --- |
| OpenAI 兼容代理（chat/responses/embeddings/images/videos） | 部分 | 完整（含 multipart、视频任务固定线路） | 保留 Fluxgate |
| 模型名归一化（渠道前缀/分组/变体后缀） | 有 | 有（`pattern` + `domain` 身份比较） | 保留 Fluxgate |
| 优先级 + 权重选路、key 模式（可用优先/加权随机） | 有 | 有 | 保留 Fluxgate |
| 熔断/冷却（按 key、按 key+模型、禁用） | 有 | 有（持久化） | 保留 Fluxgate |
| 请求日志（逐次尝试、上游原文） | 有（另一张表） | 有（单表 + 尝试轨迹） | 保留 Fluxgate，补充保留天数 |
| 错误日志独立保留 | 有 | 记录内含失败原因 | 合并为请求日志的失败视图 + 保留策略 |
| **客户端指纹预设（23 个真实客户端请求头）** | 有 | 无 | **已移植**：`internal/clientprofile` |
| **用量统计（token 记账、窗口聚合）** | 有 | 无 | 待移植：`gateway_usage_events` + `GET /management/usage` |
| **账号自动探测（加法题）** | 有 | 无 | 待移植：`internal/probe` + 调度器 |
| **内部渠道固定（model pin + 探测/验证）** | 有 | 无 | 待移植：上游/模型级固定 + 注入 `provider.only/order` |
| **IPv6 代理池（租约、换 IP、一号一 IP）** | 有 | 无 | 待移植：`internal/proxypool` + `proxy_pools` 资源 |
| 登录/会话 | token（localStorage） | HttpOnly Cookie 会话 + 防爆破 | 保留 Fluxgate |
| 管理 API 鉴权 | 管理员 token | 会话 Cookie 或 management token，写操作同源校验 | 保留 Fluxgate |
| 管理台 | 10 个平铺标签页 | 6 视图（概览/上游/路由/请求记录/客户端密钥/设置） | **重做为按功能分组的四区布局** |
| 部署 | Docker/Compose + 环境变量 | Docker/Compose + 环境变量（更严格的安全基线） | 保留 Fluxgate，补充 UniGate 的运维脚本要点 |

## 4. 分阶段落地

- [x] **阶段 1：客户端指纹预设**（`internal/clientprofile`）
  - 23 个预设 + 别名归一化 + 动态头（UUID/时间戳/随机/同值引用），只含非鉴权头。
  - `sites.client_profile` 列（旧库启动时就地补列），经上游复合资源读写。
  - 转发链路按「调用方头 → 预设 → 上游自定义头」顺序合并；上游自定义头留空可去掉预设头。
  - 获取模型探测同样带指纹；管理 API 返回预设目录（含证据与可信度）供管理台渲染。
- [x] **阶段 2：用量统计**（token 记账 + 概览/用量页）
  - 新增 `internal/usage`：跟随响应读取上游自报的 usage（JSON 体 / SSE 末帧 / Responses 与
    embeddings 形状），不缓冲、不延迟转发；超过 2 MiB 不扫描，记为「未知」而非 0。
  - 新增 `gateway_usage_events`（时间/模型/上游/密钥索引，按条数裁剪），
    `GET /management/usage`（窗口 today/24h/7d/30d/all + 模型/密钥/上游筛选）、
    `DELETE /management/usage`。
  - 运行策略新增 `gateway.usage.enabled` / `gateway.usage.keep`，环境变量
    `FLUXGATE_USAGE_LOG_ENABLED` / `FLUXGATE_USAGE_LOG_KEEP`。
  - 管理台新增「用量」页：窗口选择、四项汇总、按模型/上游/密钥三张分组表、保留范围说明。
- [x] **阶段 3：账号自动探测**（启动/冷却恢复/空闲三类触发）
  - 新增 `internal/probe`：随机加法题（三个五位数）+ 答案校验（chat / responses /
    data 信封 / 纯文本），按上游状态与回答分类为稳定 reason（ok / unreachable /
    invalid_proxy / auth_rejected / rate_limited / upstream_error / bad_status /
    wrong_answer）。
  - 触发调度：启动后全量核对、线路空闲超过间隔、线路从熔断中放出；探测走线路自己的
    密钥、客户端指纹、自定义头、代理与超时；**只记录事实，不改熔断状态**。
  - 新表 `gateway_probe_runs`（按条数裁剪），`GET /management/probes`、
    `POST /management/probes/run`（单条同步 / 全量后台）、`DELETE /management/probes`。
  - 运行策略新增 4 项 `gateway.probe.*`，环境变量 5 个；管理台新增「探测」页
    （调度说明、结果表、单条「再探测」、全量探测、清空记录）。
- [x] **阶段 6：管理台按功能重做**（四区侧栏 + 客户端指纹目录）
  - 顶部导航改为按功能分组的侧栏：监控（概览/用量/探测/请求记录）、渠道（上游/路由）、
    访问（客户端密钥/代理）、系统（设置）；顶栏显示「分组 / 页面」当前位置，窄屏时
    侧栏折成上方可换行的横排。
  - 「代理」从设置子页提升为独立页面（它属于「访问」，不是进程自身）；设置子页改为
    账号 / 运行策略 / 客户端指纹。
  - 新增「客户端指纹」设置页：23 个预设的名称、可信度、请求头数量、说明与证据逐行
    展开（数据来自网关的预设目录，不在前端写死）。
  - 空表隐藏表头、窄屏换行布局、内容列宽度与密集表格列距随新布局调整，并用 Chrome
    DevTools Protocol 对全部页面截图核对。
- [ ] **阶段 4：内部渠道固定**（探测 + 逐渠道验证 + 注入）
- [ ] **阶段 5：IPv6 代理池**（租约管理、换 IP、共享池）
- [ ] **阶段 7：文档与部署**（README 重写、Compose/环境变量对齐）

## 5. 管理台信息架构（按功能重做）

顶栏保留：品牌、版本、运行时长、健康状态、主题切换、立即刷新、退出登录。

侧栏按「谁在用、要解决什么问题」分成四组，每组下是可切换的页面：

| 分组 | 页面 | 职责 |
| --- | --- | --- |
| **监控** | 概览 | 健康、可路由模型数、线路状态汇总、熔断清单、用量概览 |
| | 用量统计 | token 记账的趋势与按模型/上游/密钥聚合（窗口：今日/24h/7d/30d/全部） |
| | 请求记录 | 逐请求结果、每次尝试、上游原文；只看失败、按模型筛选、分页 |
| **渠道** | 上游 | 上游一条配置：地址、优先级/权重、指纹、请求头、代理、密钥、模型 |
| | 路由 | 模型 → 上游 → 线路逐级展开，冷却倒计时与逐级解除 |
| | 内部渠道固定 | 按上游+模型固定上游内部 provider（探测/验证/排除） |
| | 探测 | 账号健康探测结果与调度状态（启动/冷却恢复/空闲） |
| **访问** | 客户端密钥 | 下游密钥、限定/排除模型/路由/上游、轮换与显示 |
| | 代理 | 代理档案（HTTP/SOCKS5）与 IPv6 代理池、租约一览 |
| **系统** | 设置 | 账号、运行策略（重试/熔断/日志/默认指纹）、探测策略、关于 |

设计约束（沿用 Fluxgate 的既有安全基线，重做不改）：

- 数据一律 `textContent`/DOM API 渲染，配置值不会被当作标记解析。
- 密钥只写不读：列表只显示掩码，客户端密钥的「显示」单独鉴权并记审计日志。
- 写操作同源校验；页面可在反代前缀下挂载（`X-Forwarded-Prefix`）。
- 每个可编辑表格都带字段级错误、危险操作二次确认、悬浮提示（不推挤页面）。
- 明暗两套配色、时间统一显示北京时间、10 秒自动刷新的页面只读且展开状态不被折叠。

## 6. 兼容与迁移

- **旧 Fluxgate 数据库**：`sites.client_profile` 等新增列在启动时由
  `EnsureUpstreamSchema` 就地补齐，旧行读作「无预设」，语义不变。
- **旧 UniGate 数据**：`gateway.json` 不自动导入。迁移按「站点 = 上游、key = 线路、
  通用密钥 = 下游密钥」人工/脚本映射（后续在 `cmd/fluxgate` 增加 `import` 子命令）。
- **行为差异**：UniGate 的 `priority` 越大越先已被 Fluxgate 采用；UniGate 的
  `cooldowns.json` 状态不迁移（重启后重新累积），避免把旧进程的瞬时状态当成事实。

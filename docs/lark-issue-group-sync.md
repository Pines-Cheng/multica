# Issue ↔ 飞书群双向协作

> 状态：P0、P1 已验收通过；P2 的 `/issue` 交给小组、运行进度卡片、群消息附件入站已实现，附件出站待定。

## 1. 目标

让一个 Multica issue 与一个飞书群（或群内话题）成为同一场协作的两个界面：

- 给 issue 开启飞书群后，由 **管家 Bot**（默认 = Squad Leader 的飞书 Bot）建群，拉入 Squad 成员（各 Agent 的 Bot + 人类成员 + 发起人）。
- issue timeline（comment、状态/指派变更、Agent 运行进度）实时出现在群里，Agent 的发言由它**自己的 Bot** 发出。
- 群里的任何消息都写回 issue comment；群里 @某 Agent ≡ 在 issue 里 `@Agent` 的 comment，走现有触发链路。
- 子 issue = 父群里的一个话题（thread）；孙 issue 在同一群里平铺为新话题。
- issue 结案后群保留、继续同步。

Multica 是唯一事实源和执行方；botmux 只作交互参考，不参与运行。

## 2. 可行性结论

| # | 能力 | 结论 | 依据 |
|---|---|---|---|
| F1 | Bot 用 API 建群 | ✅ | `POST /im/v1/chats`，权限 `im:chat` 或 `im:chat:create`；扫码创建（PersonalAgent）的默认模板**已包含** `im:chat:create`（[飞书：一键创建智能体应用·附录](https://open.feishu.cn/document/mcp_open_tools/integrating-agents-with-feishu/overview)、[创建群](https://open.feishu.cn/document/server-docs/group/chat/create)） |
| F2 | 建群时拉其他应用的 Bot | ✅，有上限 | `bot_id_list` 填 app_id，**单次 ≤5 个，群内 Bot 总数 ≤15**；更多需用"拉人入群"接口分批加入（需 `im:chat.members:write_only`）。botmux 生产代码按 5 个一批加入（`botmux/src/services/group-creator.ts`） |
| F3 | 建群时拉人 | ✅ | `user_id_list` ≤50，支持 `user_id_type=union_id`；`uuid` 参数可让同一请求 10 小时内只建一个群（天然幂等）；可指定 `owner_id` + `set_bot_manager=true` |
| F4 | 接收群里**未 @** 的消息 | ⚠️ 需新增权限 | 需要 `im:message.group_msg`（敏感权限，**默认模板不含**）。该权限只推送**用户**消息、不推送 Bot 消息，对防回环有利（[接收消息事件](https://open.feishu.cn/document/server-docs/im-v1/message/events/receive)） |
| F5 | 给已有应用补权限 | ✅ | 扫码注册的确认页支持 `addons` 增量申请权限；传 `clientID=<app_id>` 走"更新已有应用"流程（[Go SDK 文档](https://open.feishu.cn/document/mcp_open_tools/integrating-agents-with-feishu/create-an-app-in-one-click-go)，实现见 `oapi-sdk-go/scene/registration`：`addons` = gzip+base64url 编码的 JSON，挂在二维码 URL 的 query 上） |
| F6 | 把群里的 @Bot 映射回 Agent | ✅ | Multica 已实测：不同应用视角下 Bot 的 `open_id` 不一致，**`union_id` 一致**，已存为 `channel_installation.config.bot_union_id`（`server/migrations/112_lark_installation_bot_union_id.up.sql`）；事件 `mentions[].id.union_id` 可直接匹配 |
| F7 | 话题回复 / 识别话题 | ✅ | 回复接口 `reply_in_thread=true` 已在用（`server/internal/integrations/lark/http_client.go`）；入站事件带 `root_id` / `thread_id`，已解码 |
| F8 | 编辑同步 | ✅ | 发出的消息统一用卡片，`PatchInteractiveCard` 已有（默认模板有 `im:message:update`） |
| F9 | 区分发送者是人还是 Bot | ✅ | 事件 `sender.sender_type`（user / app），当前解码器**未保留**，需补 |

**剩余风险（只能实测验证）**

- R1 `im:message.group_msg` 是敏感权限，企业租户可能要求管理员审批。**降级**：未获批时只同步 @Bot 的消息（默认模板已有 `im:message.group_at_msg.include_bot:readonly`），其它功能不受影响。
- R2 Multica 自己实现了注册客户端（`lark/registration.go`），未接入 `addons`；需实测 PersonalAgent 注册流程能正确读取 `addons`（官方 SDK 走的就是同一个 `archetype=PersonalAgent` 端点，预期可行）。
- R3 `union_id` 在"同一开发商"范围内稳定。不同员工扫码建的 PersonalAgent 应用是否属于同一开发商（租户），需在真实租户验证；MUL-2671 的结论说明至少同一安装者的多个 Bot 之间成立。
- R4 一个群最多 15 个 Bot：Squad 超过 14 个 Agent 时，超出的 Agent 不进群（以文字形式发言由管家代发）。
- R5 以应用身份拉人进群时，被拉的人必须在该应用的**可用范围**内。PersonalAgent 应用默认的可用范围可能只有创建者本人，需要在开放平台把可用范围扩大到团队，否则拉人会失败（失败的成员会记录在日志中，群仍会创建）。

**P0 验收结果（2026-11，字节跳动租户，4 个 PersonalAgent Bot）**

- R1、R2：扫码追加的权限生效，群里未 @ 的消息能写成评论。
- R3：Bot 的 union_id 查不到，与租户无关——通讯录接口查询 Bot 自身需要覆盖 Bot 的通讯录范围，部门范围不够，全员范围申请不下来。已改为在 union_id 缺失时按 Bot 显示名识别 @（同名不匹配）。
- R4：未触发（Squad 只有 4 个 Agent）。
- R5：本次只验证了发起人本人入群，拉其他成员时仍需留意应用可用范围。

## 3. 总体架构

```
              ┌────────────── Multica Server ───────────────┐
飞书群消息 ──▶ │ Feishu WS (管家 Bot 的长连接)                   │
              │   └▶ engine.Router                              │
              │        ├─ 普通群/私聊 → 现有 Chat 链路（不变）    │
              │        └─ 已绑定 issue 的群 → IssueChat 入站      │
              │              → 创建 comment（复用 comment 触发）  │
              │                                                 │
              │ events.Bus ──▶ IssueChat 出站同步器              │
              │   comment:* / issue:updated / task:*            │
              │        → 选发送 Bot → 发/改卡片 → 记录映射        │──▶ 飞书群
              │                                                 │
              │ IssueChatService.EnsureChat (建群/开话题/补成员)   │
              └─────────────────────────────────────────────────┘
```

新增代码集中在 `server/internal/integrations/issuechat/`（平台无关的绑定、路由、出站编排）和 `server/internal/integrations/lark/`（飞书实现）。现有 Chat 链路只在入口加一个分支。

## 4. 数据模型

遵循仓库规则：无外键、无级联；索引一律 `CREATE INDEX CONCURRENTLY`，每个索引单独一个迁移文件。

### 4.1 `channel_issue_chat` —— issue 与群/话题的绑定

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | UUID PK | |
| `workspace_id` | UUID | |
| `issue_id` | UUID | 被绑定的 issue |
| `root_issue_id` | UUID | 拥有这个群的顶层 issue（自身为根时 = `issue_id`） |
| `channel_type` | TEXT | `feishu` |
| `chat_id` | TEXT | 飞书 `oc_…` |
| `thread_root_message_id` | TEXT NOT NULL DEFAULT '' | 空串 = 群主消息流（根 issue）；非空 = 该子 issue 的话题根消息 `om_…` |
| `steward_installation_id` | UUID | 管家 Bot 的 `channel_installation.id` |
| `status` | TEXT | `active` / `detached` |
| `created_by` | UUID | 发起人 |
| `created_at` / `updated_at` | TIMESTAMPTZ | |

索引：`UNIQUE (id)`；`UNIQUE (issue_id, channel_type)`；`UNIQUE (channel_type, chat_id, thread_root_message_id)`。

### 4.2 `channel_issue_message` —— 消息映射（防回环、编辑、引用回复）

| 列 | 类型 | 说明 |
|---|---|---|
| `id` | UUID PK | |
| `workspace_id` / `issue_id` / `binding_id` | UUID | |
| `installation_id` | UUID | 发送或接收这条消息的 Bot |
| `channel_message_id` | TEXT | 飞书 `om_…` |
| `direction` | TEXT | `inbound` / `outbound` |
| `kind` | TEXT | `comment` / `activity` / `task` / `thread_root` / `intro` |
| `comment_id` | UUID NULL | |
| `task_id` | UUID NULL | |
| `created_at` | TIMESTAMPTZ | |

索引：`UNIQUE (channel_message_id)`；`(comment_id)`；`(task_id)`。

## 5. 管家 Bot 的选择规则

对**根 issue**按顺序取第一个满足的：

1. 指派给 Squad → 该 Squad Leader Agent 的有效飞书安装；
2. 指派给 Agent → 该 Agent 的有效飞书安装；
3. 都没有 → 不能开启，返回明确错误（"该 issue 的负责 Agent 未连接飞书"）。

结果写入 `steward_installation_id`，之后入站只认这个安装。Leader 更换或管家 Bot 断开时，P1 提供"交接"：新管家入群 → 更新字段 → 旧管家退群。

## 6. 建群 / 开话题：`IssueChatService.EnsureChat(issueID, actor)`

幂等：重复调用返回已有绑定。

**根 issue（无已绑定祖先）**

1. 选管家（§5）。
2. 收集成员：
   - Bot：Squad 内所有 Agent 的有效飞书安装 app_id（不含管家，最多 14 个）；
   - 人：Squad 人类成员 + issue 创建者 + 发起人，通过 `channel_user_binding` 取 `union_id`；无绑定的人跳过，并在介绍卡片里列出"未绑定飞书，未能拉入"。
3. 调用建群：`name = "[MUL-12] <标题>"`，`uuid = issue_id`（飞书侧幂等），`user_id_type=union_id`，`owner_id = 发起人`，`set_bot_manager=true`；Bot ≤5 个时放进 `bot_id_list`，超出部分建群后按 5 个一批加入。
4. 写入 `channel_issue_chat`（唯一索引兜底并发）。
5. 建群时只拉人。任务详情 Tab、介绍卡片（即置顶进度卡）、产物 Tab 等到该任务（或其子任务话题）第一次 `task:running` 时由 `activateChat` 补上；已有进度卡的群不再重复。启动前评论照常同步，元数据和产物变化不写 Tab。介绍卡片包含标题、状态、负责人、Web 链接、"在群里 @Agent 即可让它干活"，以及未绑定飞书、未能拉入群的成员。

**绑定已有群**：`BindChat` 把已有群设为根任务的群（`root_issue_id = issue_id`，话题根为空），不建新群。

- 校验：管家 Bot 必须已在群里（`GET /im/v1/chats/{chat_id}`，默认模板的 `im:chat:read` 即可；返回 232011 或只有公开字段都视为不在群）；外部群拒绝；一个群只承载一个根任务，已被其他任务占用返回 409。任务已绑定同一个群时直接返回；已绑定其他群时返回 409。
- 绑定后只拉小组成员入群（同建群），群名、群主和群公告不变；Tab 和置顶同样等到首次运行再补。
- 绑定后群里的消息都按 issue 群处理（写成评论），该群原有的普通会话链路不再生效。

**产物 Tab**：任务首次运行补发进度卡时，以及之后任务进度、metadata 或产物刷新时，使用群对应根任务的产物 `title` / `url`，以及 metadata 中所有有效的 HTTP(S) 链接，添加或更新顶部网页 Tab。没有产物名称白名单，也不根据 `kind` 决定是否展示。标题来自登记数据；metadata 链接使用原键名。产物记录优先于同键名的 metadata 链接；两处指向同一 URL 时也优先使用登记的标题，只显示一次。已有 metadata 链接需要补充展示标题时，使用相同键名登记产物即可。所有网页 Tab 设置 `is_built_in=true`，在飞书内打开。各任务独立同步，子任务话题的链接不会覆盖根任务的 Tab。原卡片链接及"查看任务"按钮行为保持不变。

**产物登记**：每个任务有独立的产物记录，存储稳定 `key`、展示 `title`、HTTP(S) `url` 和可选的自由文本 `kind`。`key` 用于更新同一条记录，`kind` 仅做分类；新增产物类型不需要新增枚举或修改卡片、Tab 和置顶规则。标题支持中文。登记命令：

```sh
multica issue artifact set DEV-43 --key design --title "技术方案" --url "https://example.feishu.cn/docx/design" --kind design
multica issue artifact set DEV-43 --key load_test --title "压测报告" --url "https://example.feishu.cn/docx/report" --kind load-test
multica issue artifact list DEV-43 --output json
multica issue artifact delete DEV-43 --key load_test
```

API 为 `GET /api/issues/{id}/artifacts`、`PUT /api/issues/{id}/artifacts/{key}`、`DELETE /api/issues/{id}/artifacts/{key}`。PUT 正文为 `{ "title": "技术方案", "url": "https://example.feishu.cn/docx/design", "kind": "design" }`，按键原子替换完整记录；省略 `kind` 会清除分类。键名遵循 `^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$`，标题为 1–120 个字符的单行文本，链接上限 2048 字节，分类上限 64 个字符，单个任务产物数据上限 16KB。产物变更发布 `issue_artifacts:changed`，刷新原有置顶卡及该任务的顶部标签；重复写入相同内容和删除不存在的键不会再次刷新。现有 metadata 保持标量 KV 契约，产物独立存储，不向 metadata 双写。

- 自动创建的标签 ID 按稳定产物键保存在 `channel_issue_chat.artifact_tabs`。刷新先读取已有 Tab，再按持久化 ID 更新或补建；标题、链接和内打开设置未变时不写入，重复刷新不新增副本。修改产物标题和链接会更新同一个自动标签。
- 未登记 ID 的标签保留，不凭名称或 URL 接管，包括手工创建的同名网页标签及历史标签。标题碰撞时为新标签加序号。飞书最多允许 20 个自定义标签；数量满时继续更新已有自动标签，不再创建新标签，完整链接仍展示在卡片上。顶部标题截取为最多 30 个 UTF-8 字节，卡片保留完整标题。移除 metadata 链接或产物时不自动删除群 Tab；卡片同步移除对应链接。保留键与标签 ID 的对应关系，重新登记同一产物时可复用原标签。
- 同步失败仅记日志，卡片和任务流转继续。飞书每群最多 20 个自定义 Tab；已达上限、缺少权限或目标站点登录限制仍适用。已有群在下一次任务事件时同步，不在启动时批量修改。

**子 issue（存在已绑定的祖先）**

1. 找到最近的已绑定祖先所在的群。
2. 管家在群主消息流发一张子任务卡片：保留完整标题，副标题展示父子任务编号路径、状态和负责人，其 `message_id` 作为话题根。
3. 写入绑定：`chat_id` = 祖先群，`thread_root_message_id` = 话题根，`root_issue_id` = 根 issue。
4. 子 issue 的负责 Agent/成员不在群里时补拉入群。

## 7. 入站：群消息 → comment

在 `engine.Router.processClaimed` 中，**在群聊 @Bot 过滤之前**插入一个分支。核心逻辑不读平台原始数据，平台相关部分放在飞书的 resolver 里：

```
route, bound := IssueChat.Lookup(chat_id, root_id)     // 先按话题根查，再按群查
if bound:
    if inst.ID != route.StewardInstallationID → 丢弃（issue_chat_non_steward）
    if 发送者是 Bot                            → 丢弃（issue_chat_bot_sender）
    identity := 解析发送者（含 union_id 兜底，§9）
    IssueChat.RecordIssueComment(...)           // 持久化待同步消息，随后返回飞书回执
    return
```

`FeishuIssueChatRouter.Run` 在后台处理 `channel_issue_comment_pending`。同一绑定按消息顺序处理，不同绑定最多四个并行；媒体下载与上传最多等待 45 秒。进程重启后继续处理待同步消息，数据库等临时故障会重试。后台处理时重新检查安装状态、成员身份和任务归属。

- **其它 Agent 的 Bot**也会收到 @它的事件，统一按 non_steward 丢弃。这样一条消息只会被处理一次，不需要跨 Bot 去重。
- **mention 转换**：`mentions[].id.union_id` 命中本工作区某个飞书安装的 `bot_union_id` → `[@Agent](mention://agent/<id>)`；命中用户绑定 → `[@名字](mention://member/<user_id>)`；`@所有人` 和无法识别的 → 纯文本。对管家 Bot 自身的 @：如果管家就是 Leader，转换成 `@Leader` mention，等同于在 issue 里 @Leader。
- **引用回复**：`parent_id` 命中 `channel_issue_message` → 作为 comment 的 `parent_id`（落入同一 comment 串）。
- **话题内消息**：`root_id` 命中话题根 → 写入对应子 issue。
- **未绑定的人**：沿用现有的绑定提示卡片；消息不写入 issue。
- **附件/图片**：复用飞书媒体下载、对象存储和上传意图清理链路。独立图片、文件和富文本内的媒体转成 Multica 附件，正文使用 `/api/attachments/{id}/download`；按消息结构保留图文位置，同一资源重复出现时复用附件。下载失败的位置保留 `[Attachment unavailable]`，其余文字与附件仍同步。音频、视频沿用同一媒体链路。

**comment 写入的复用方式**：`Handler` 实现 `lark.IssueCommentWriter`，通过进程内请求复用 `handler.CreateComment`。评论、附件、上传意图认领、`channel_issue_message(direction=inbound)` 和队列确认在同一事务内提交；队列租约失效则回滚。提交后沿用普通评论的权限检查、`triggerTasksForComment` 和事件发布，因此 @智能体时附件已经可读。`comment:created` 带 `channel_origin`，避免把来源消息再镜像回群。只处理新增群消息，不补历史附件。

## 8. 出站：issue timeline → 群

`issuechat.OutboundSyncer` 订阅 `events.Bus`。Bus 是同步调用，HTTP 发送放到 goroutine 异步执行，并设超时。

| 事件 | 群内表现 | 发送者 |
|---|---|---|
| `comment:created`（Agent 写） | 卡片：正文 Markdown。由某次运行写出、且该运行已有运行卡片时，**直接把运行卡片改成这条评论**，不再另发一张；Agent @ 了成员时卡头为橙色「待 xx 处理」 | 该 Agent 自己的 Bot（在群里时）；否则由管家代发，卡头写 Agent 名 |
| `comment:created`（人在 Web 写） | 卡片：卡头 `张三 · 来自 Multica`，正文 | 管家 |
| `comment:created`（来自飞书） | **不发**（payload 带 `channel_origin`） | — |
| `comment:updated` | 更新原卡片 | 原发送者 |
| `comment:deleted` | 原卡片改为"该评论已删除" | 原发送者 |
| `issue:created` / `issue:updated` / `issue_metadata:changed` | 状态、负责人、标题或资料链接变化时，原地刷新根群置顶的进度卡；子任务同时刷新自己的话题首卡，不发新消息。展示实际状态、负责人、子任务进度和 metadata 中已有的资料链接 | 每张卡片的原发送 Bot，管家交接不改变旧卡的更新身份 |
| `issue:created` 子 issue（P1） | 按 §6 开话题 | 管家 |
| `task:*` Agent 运行进度（P2） | 运行超过 20 秒才发卡片（Leader 分派这类短运行不打扰）；无标题，显示状态、已运行时长和最近 2 条动作（只显示动作类别，如「运行命令」「修改文件」，不暴露参数），最多每 3 秒刷新；运行写了评论时卡片被评论接管（见上）；没写评论则结束时只留一行「已完成 · 用时」，失败时显示原因；已写过评论的运行不再开卡片；附「查看详情」链接 | 该 Agent 的 Bot |

- **展示模板**：进度卡和话题首卡使用轻量分区：当前处理人、子任务进度、相关资料、主按钮、辅助说明。进度按阶段靠左、状态及负责人靠右排列；相关资料位于主按钮前，以两列文字链接展示，不加背景、边框或额外按钮。资料名称直接取产物 `title` 或 metadata 键名，不在展示代码中翻译类型名称或按类型排序。产物按稳定键名排序，metadata 链接排在其后；无链接时省略整个资料区。子任务列表简写阶段名，话题首卡保留完整标题和父任务编号，便于独立查看。状态颜色来自内置状态或自定义状态的生命周期分类；自定义状态展示其实际名称，不推断其具有内置评审行为。
- **置顶预览**：根群卡片标题为"任务进展与产物"，完整任务名保留在正文，副标题展示状态和负责人。摘要展示当前阶段或阻塞，有未关闭的协助请求时优先提示"待协助"；父任务已进入验收或结束时展示该状态。不用子任务完成比例代表整体进度，新增或调整子任务只更新实际列表。
- **链接操作**：主按钮只声明 `open_url`，打开现有任务页面；`in_review` 显示"查看交付"，`done` 显示"查看结果"。提醒卡提供定位到原评论的"查看讨论"入口。没有有效地址时省略入口，不生成体验地址或自测报告，不新增卡片回调、直接验收或认证检查。
- **人工协助**：评论创建时显式设置 `request_assistance=true`（CLI：`issue comment add --request-assistance`），且正文必须包含当前工作区成员的有效真人 mention。普通 @ 继续正常通知，不会自动变成协助请求。评论保存独立的 `assistance_status=none/open/closed`，与讨论解决状态、任务状态无关。根群置顶卡在进度前汇总 `open` 请求，展示接收人、任务编号、正文摘要，以及原消息的飞书定位链接和任务详情处理入口；子任务的话题卡保留完整请求。缺少飞书定位元数据时保留处理入口，不构造虚假话题地址。
- **协助关闭与刷新**：任务详情中提供"标记已处理"和"重新打开请求"；智能体确认成功后可调用 `issue comment assistance-close <comment-id>`，明确再次需要时调用 `assistance-reopen`。接口为 `POST /api/comments/{commentId}/assistance/close`、`POST /api/comments/{commentId}/assistance/reopen`，沿用当前工作区成员与智能体身份验证。创建、编辑、关闭、重开和删除后更新原话题协助卡并重算根群置顶卡，关闭后原卡显示"协助已处理"，置顶列表移除该条。普通回复、讨论已解决、任务完成不会改变协助状态；不新增飞书按钮回调、不重复发卡。状态持久化，后续刷新从数据库重建。
- **路由**：comment 所属 issue 有绑定 → 发到群主消息流，或用 `reply_in_thread` 回复到话题根。Web 上对某条 comment 的回复 → 引用回复它对应的那条飞书消息。
- **mention 渲染**：mention 了已绑定飞书的成员时，用发送 Bot 视角下的身份（该 Bot 下的绑定 open_id）生成原生 `<at id=…>`，成员会收到飞书提醒；用户在该 Bot 下没有绑定时退化为粗体名字；mention Agent 一律渲染为粗体名字，不用 `<at>` 去 @Bot，避免对方 Bot 收到事件。
- **防回环三道闸**：① `channel_origin` 跳过回发；② 入站丢弃 `sender_type=app`；③ `channel_issue_message.channel_message_id` 唯一。
- **投递语义（P0）**：尽力而为。失败时重试 3 次（指数退避），仍失败则记日志和指标，不阻塞 issue 写入。是否需要持久化 outbox 见 §13 D2。

## 9. 身份

- `channelMessageFromLark`（`lark/feishu_channel.go`）补齐 `SenderStableID = sender.union_id`、`SenderType`，并把 mention 列表（key、name、open_id、union_id）带进 `Raw`。
- 飞书 `ResolveSender` 增加兜底：`(installation, open_id)` 查不到绑定时，用 `union_id` 在**同工作区**的其它飞书安装下查找绑定；找到后为当前安装补建一条绑定，后续直接命中。仍然每次校验工作区成员身份。
- 拉人入群使用 `union_id`，不依赖用户与管家 Bot 是否私聊过。

## 10. 权限升级

- 新安装：注册客户端 `decorateQRCodeURL` 增加 `addons`，内容为 `{"scopes":{"tenant":["im:message.group_msg","im:chat.members:write_only","im:chat.tabs:read","im:chat.tabs:write_only","im:chat.top_notice:write_only"]}}`（Tab 读取/写入权限用于查询已有标签页、添加任务详情和同步关键产物；top notice 权限用于置顶进度卡）。
- 已有安装：Agent 的飞书集成设置页新增"升级飞书权限"入口，生成带 `clientID=<app_id>` 和同样 `addons` 的二维码，由安装者扫码确认。
- 开启 issue 群前检查管家 Bot 是否具备 `im:message.group_msg`（通过 `application:application:self_manage` 自查授权列表）。未具备时仍允许开启，并在介绍卡片和 Web 上提示"仅同步 @Bot 的消息"。

## 11. 入口

| 入口 | 期次 | 行为 |
|---|---|---|
| issue 详情页按钮"创建飞书群"/"打开飞书群" | P0 | `POST /api/issues/{id}/channel-chat`，`GET` 返回绑定和 applink |
| 接口创建任务时绑定已有群 | — | `POST /api/issues` 带 `channel_chat_id: "oc_…"`：任务先创建，再同步绑定该群，`issue:created` 不再自动建群。绑定失败不影响创建，201 响应中的 `channel_chat_error` 返回 `code` 和 `message`；成功时返回 `channel_chat`。重试：`POST /api/issues/{id}/channel-chat` 带 `{"chat_id": "oc_…"}`（无 body 时仍为建群）。错误码：`invalid_chat_id` 400、`chat_not_found` 404、`chat_already_bound` / `issue_chat_already_bound` / `steward_bot_not_in_chat` / `no_steward_bot` 409、`external_chat_unsupported` 422、`lark_api_error` 502 |
| Web 上把 issue 指派给 Squad、或 comment `@Squad` | P1 | Squad 开关开启（默认开启）时，订阅事件后自动 `EnsureChat`，见 D1 |
| Agent 创建子 issue | P1 | 祖先有群 → 自动开话题 |
| 飞书里 `/issue <需求>` 发给 Leader Bot | P2 | 该 Agent 恰好领导一个 Squad 时指派给该 Squad 并自动建群；否则保持指派给 Agent，见 D3 |

前端改动：`packages/core`（API schema + query hook，响应走 zod `parseWithFallback`）、`packages/views`（issue 详情页按钮，web 和 desktop 共用）、所有已支持语言的文案。

## 12. 分期

**P0：最小闭环**
1. 迁移：两张表 + 各自的索引迁移文件；`make sqlc`。
2. 飞书解码补 `sender_type`、`union_id`、mentions；`ResolveSender` 增加 union_id 兜底。
3. `EnsureChat`（仅根 issue）+ 手动入口 API + issue 详情页按钮。
4. 入站分支：群消息 → comment（文本、mention、引用回复）。
5. 出站：comment 的创建/编辑/删除 → 群卡片，含防回环。
6. 权限：新安装带 `addons`；已有安装扫码升级；缺权限时降级提示。

**P1：自动化 + 结构**
状态/指派同步；Squad 开关 `auto_create_channel_chat`（默认开启）+ 设置页开关；指派给 Squad 或 `@Squad` 时自动建群；子 issue 话题；成员变化补拉；管家交接。

**P2：体验**
飞书 `/issue` 走 Squad；Agent 运行进度实时卡片；附件双向同步。

## 13. 决策记录

- **D1 自动建群的范围（已定）**：新增 Squad 级开关"指派时自动创建飞书群"，**默认开启**。
  - 数据：`squad` 表新增 `auto_create_channel_chat BOOLEAN NOT NULL DEFAULT true`；已有 Squad 迁移后同样为开启。
  - 生效条件：开关开启，并且 Leader 已连接飞书（能选出管家 Bot）。Leader 未连接飞书时**静默跳过**，不报错、不写系统 comment。
  - 触发：issue 被指派给该 Squad，或 comment 中 `@Squad`。已有绑定时不重复建群。
  - 管理：Squad 设置页提供开关；API 和前端 schema 同步增加该字段（缺省值视为 `true`）。
- **D2 出站可靠性（已定）**：P0 用尽力而为 + 重试（进程崩溃时可能丢失少量群消息，issue 数据不受影响）。不做持久化 outbox。
- **D3 飞书 `/issue`（已定，方案 A）**：Bot 对应的 Agent **恰好领导一个 Squad** 时，`/issue` 创建的 issue 指派给该 Squad（Leader 以协调者身份分派，并按 D1 自动建群）；领导多个 Squad 或不是 Leader 时，保持现有行为（指派给该 Agent）。回复中注明实际指派对象。
- **D4 群主（已定）**：群主为发起人（真人），管家 Bot 为群管理员。

## 14. 测试计划

- Go（`make test`，使用 `testutil` / `dbfx` 夹具）：
  - 路由：已绑定群里，管家收到的消息 → 生成 comment；非管家安装 → 丢弃；`sender_type=app` → 丢弃；未绑定群 → 走原 Chat 链路不变（回归）。
  - mention：Bot 的 union_id → `mention://agent`，并能触发 agent task；在群里 @Leader → Leader 的 task 入队。
  - 话题：`root_id` → 对应子 issue；引用回复 → `parent_id`。
  - 身份：union_id 兜底补建绑定；非工作区成员被拒。
  - 出站：带 `channel_origin` 的 comment 不回发；Agent comment 由其自己的 Bot 发送；编辑 → 更新卡片。
  - `EnsureChat` 幂等、Bot 分批加入、并发调用只产生一条绑定。
  - 自动建群：开关开启并指派给 Squad → 建群；开关关闭 → 不建；Leader 未连接飞书 → 静默跳过。
  - 飞书 HTTP 全部使用 fake `APIClient`，不访问真实网络。
- 前端（`pnpm typecheck` / `pnpm lint` / `pnpm test`）：schema 的异常响应测试；按钮的状态切换。
- 真机验收（需要你在飞书里配合）：两个 Agent Bot + Leader Bot，验证 R1–R3。

## 15. P0 实现位置

| 部分 | 位置 |
|---|---|
| 表结构 | `server/migrations/595`–`601`，查询 `server/pkg/db/queries/channel_issue_chat.sql` |
| 入站分支 | `engine.Router.processIssueChat`（`server/internal/integrations/channel/engine/router.go`），飞书实现 `lark/issue_chat.go` |
| 写评论 | `handler.CreateChannelIssueComment`（`server/internal/handler/issue_channel_chat.go`），在进程内复用评论接口 |
| 建群与出站同步 | `lark/issue_chat_service.go`、`lark/issue_chat_card.go`、`lark/http_client_chat.go` |
| 身份合并 | `feishuIdentityResolver.ResolveSender`（union_id 兜底并补建绑定） |
| 权限 | `lark/registration_addons.go`；已连接的 Agent 再次扫码会更新同一个应用（`clientID`） |
| API | `GET/POST /api/issues/{id}/channel-chat` |
| 前端 | issue 详情页右上角按钮 `IssueChannelChatButton`；Agent 飞书集成区的"更新飞书权限" |

### P1 实现位置

| 部分 | 位置 |
|---|---|
| Squad 开关 | 迁移 `602`（`squad.auto_create_channel_chat`，默认开启）；`PATCH` 小组接口字段同名；小组详情页「自动建飞书群」开关 |
| 自动建群 | `lark/issue_chat_activity.go`：`issue:created` / `issue:updated`（指派变更）/ `comment:created`（@Squad）触发 `autoCreate` |
| 子 issue 话题 | `EnsureChat` 发现已绑定的祖先时调用 `openTopic`：管家发根卡片，子 issue 绑定到该卡片；子 issue 的评论回复到话题里 |
| 状态 / 指派 / 标题 / 资料更新 | `refreshProgress` → `refreshBoundIssueCard`，同时刷新子任务话题首卡和根群进度卡 |
| 成员补拉 | 指派变更和开话题时调用 `inviteParticipants` |
| 管家交接 | 指派变更时 `handover`（旧管家先拉新 Bot 入群）；管家 Bot 断开时 `currentSteward` 改用根 issue 当前的管家 |

### P2 实现位置

| 部分 | 位置 |
|---|---|
| `/issue` 交给小组 | `engine.Router.leaderSquad`：Bot 的 Agent 恰好领导一个小组时指派给该小组（`ResolverSet.IssueToLeaderSquad`，仅飞书开启）；回复注明小组名 |
| 运行进度卡片 | `lark/issue_chat_runs.go`：`task:running` 由 Agent 自己的 Bot 发卡片，`task:progress` 最多每 3 秒更新一次，结束时定稿；迁移 `603`、`604` 为消息映射加 `task_id` |

## 16. 不做

- 不把群内全部历史回填到 issue（只同步开启之后的消息）。
- 不同步飞书侧的消息撤回和编辑到 issue（P0 只做 issue → 群方向的编辑同步）。
- 不支持外部群和外部用户。
- 不为其它 IM（Slack、钉钉等）实现；表结构保留 `channel_type`，方便后续扩展。


## 合并转发入站

- 群内 `merge_forward` 写成一条评论，正文使用可展开的会话记录。按原消息顺序保留发言人、时间、文字和嵌套转发；历史 @ 和命令作为引用内容，不转换为新的智能体触发。
- 图片、文件、音视频及富文本内附件下载时使用外层合并转发消息 ID，子消息 ID 用于存储去重和正文定位，复用附件存储和授权下载链接，并保留在原消息中的位置。交互卡片同步为只读文字和媒体快照，不执行卡片按钮或回调。
- 子消息读取与附件同步在持久化入站 worker 内执行。单次准备有 45 秒预算，为评论、附件和来源映射的原子提交保留时间；单个失败保留明确提示，成功部分仍可查看。
- `channel_issue_message.sync_state` 保存转发源树、已绑定附件和最后同步正文。评论创建、附件绑定、同步状态及入站队列确认在同一事务提交；重放不创建重复评论。
- 失败评论提供重试入口：`POST /api/channel-messages/{messageID}/retry`，成功返回 204，无 JSON 响应体。仅原评论作者或当前工作区管理员可操作；同步只补缺失附件，沿用已成功附件的 ID，并更新原评论。评论被编辑或删除后拒绝覆盖。重试仅发布 `comment:updated`，不重新执行评论触发。
- 重试前后的同步状态不作为公开评论 API 字段返回。已安装客户端继续读取普通 Markdown 和附件列表；Web/Desktop 共用可折叠渲染及重试按钮，移动端沿用现有 HTML 去标签处理展示正文。
- 嵌套读取设有 10 层、1000 条消息的预算，超出或源内容不可读取时显示提示，避免静默丢弃。飞书未提供可下载内容的消息类型只能保留类型提示。

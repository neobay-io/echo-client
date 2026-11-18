---
status: accepted
date: 2026-11-12
---

# Agent × Platform 能力拉齐（Claude Code / Codex / Antigravity / Kimi × Feishu / Telegram）

## 0. 结论速览

- **Agent 支持**：Claude Code、Codex 已有；**Antigravity CLI（`agy`）和 Kimi Code CLI 在本 fork 中不存在**，上游 cc-connect 已有适配器，已在 scratch 克隆中验证可以低成本移植（编译、单测均通过）。
- **平台**：Feishu、Telegram 都已支持，但 Telegram 有明显缺口（agent → 用户发文件不可用、命令菜单丢掉带连字符的命令、卡片退化成文本）。
- **功能拉齐**：大部分命令已经存在，但“能用”的程度差异很大：
  - `/auth` 只支持 Claude（`core/agent_auth.go:362` 硬编码）；
  - `/model`、`/effort` 会**清空当前会话**，且设置是整个 project 共享、不持久化；
  - Codex 的 `auto-edit/full-auto/auto` 模式**目前已损坏**（本机 codex 0.153 已删除 `--full-auto`，实测 exit 2）；
  - 所有 agent 都**不能在运行中的一轮里插入消息**，只能排队；`/stop` 是直接杀进程；
  - `/loop` 只有固定间隔，没有自定节奏。
- **空闲自动升级**：两个服务都开启了（`policy = "idle_only"`，每 24 小时），每天也确实在跑，但**对 Codex 实际无效**。升级器用登录 shell（`/bin/sh -lc`）探测和升级 npm 装的那份 codex，而 agent 进程按服务 PATH 实际执行的是 `~/.local/bin` 下的 standalone codex 0.153，npm 从来升级不到它。详见 §2.5。
- **上游 CLI 的新能力足够支撑拉齐**：Claude 的 stream-json 控制协议、Codex 的 `app-server`、agy/Kimi 的 ACP 都支持热切换模型 / effort / 权限模式、轮内插入（steer）、远程登录（device code / URL + 粘贴 code）。
- **建议策略**：**选择性移植上游，不全量合并**（全量合并有 579 个冲突块、83 个文件）。按第 6 节的阶段推进，总量约 5–6 人周。

## 1. 目标与非目标

**目标**

1. 四个主力 agent（Claude Code、Codex、agy、Kimi Code）在 Feishu 和 Telegram 上提供一致的用户能力：
   远程登录、最新模型列表与选择、思考强度、权限模式（至少 `bypassPermissions` 与 `auto`）、会话列表与切换、绑定同一会话的 cron、loop、排队与插入消息、以及 Claude Code 的其他常用能力（compact、usage、中断、提问卡片等）。
2. 新 agent 能作为 Echo worker 注册并执行任务；Echo 能按任务指定模型 / effort / 模式 / 会话。
3. 某个 agent 不支持某能力时，命令给出明确提示，而不是静默降级。

**非目标**

- 全量合并上游（见 §4）。
- Gemini CLI、Qoder：保持现状不回归，不做新能力。
- Telegram 换库（上游已改用 `go-telegram/bot`，本期不跟进）。
- 上游的 management API / Web UI / bridge / observer / 其余 13 个平台。

## 2. 现状盘点（main @ 25e72977）

### 2.1 Agent × 功能

| 功能 | Claude Code | Codex | agy | Kimi | 备注 |
|---|---|---|---|---|---|
| 接入 | ✅ 常驻进程，stream-json | ✅ 每轮一个 `codex exec --json` 进程 | ❌ | ❌ | 上游有 `agent/antigravity`、`agent/kimi` |
| 远程登录 `/auth` | ⚠️ 只有 login/code/cancel/status，**无 logout**；PTY 驱动 `claude auth login --claudeai` | ❌ | ❌ | ❌ | `core/agent_auth.go:362` 硬编码 claudecode |
| 模型列表 | ✅ 动态（`/v1/models` → `claude --print /model` → 静态） | ✅ `app-server model/list` | — | — | Claude 的 `/v1/models` 只取第一页、带 key 跟随重定向（`agent/claudecode/claudecode.go:321`） |
| 思考强度 `/effort` | ⚠️ auto/low/medium/high/max，**缺 xhigh、ultracode** | ⚠️ low…xhigh，**缺 max/ultra**，不能回到默认 | — | — | 选项固定，不随模型变化 |
| 权限模式 `/mode` | ✅ default/acceptEdits/plan/auto/bypassPermissions/dontAsk；能把权限请求转成卡片 | ❌ **auto-edit/full-auto 已损坏**（`agent/codex/session.go:104` 仍传 `--full-auto`）；只有 yolo 可用；无审批转发 | — | — | 非法值静默回到 default |
| 会话 `/list /switch /new /name /delete` | ✅（另有 `/attach`） | ✅ | — | — | `/delete` 只删本地记录，从不调用 `SessionDeleter` |
| Cron 绑定同一会话 | ⚠️ 支持 `/cron session` 绑定；但 cron 写入后，常驻的 Claude 进程看不到这些轮次 | ✅ | — | — | 本地 API `CronAddRequest` 无 `agent_session_id` |
| `/loop` | ✅ 固定间隔 + `<loop-control>pause</loop-control>` | ✅ | — | — | 无自定节奏 / goal |
| 排队 | ✅ `/queue`（按聊天，20 条） | ✅ | — | — | 设计文档写的“按 agent 会话共享队列”未实现（P5 修订了设计文档：按聊天排队是有意的） |
| 轮内插入 | ❌ | ❌ | — | — | |
| `/stop` | ⚠️ 杀进程 | ⚠️ 杀进程 | — | — | 无优雅中断 |
| `/compact` | ✅ | ⚠️ 当普通文本发给 `codex exec` | — | — | |
| 提问 / 计划审批卡片 | ⚠️ 只有通用 allow/deny；AskUserQuestion、ExitPlanMode 无专门处理 | ❌ | — | — | |
| usage / context / cost | ❌ | ❌ | — | — | `/status` 不显示模型、effort、用量 |

另外两个全局问题：

- `/model`、`/effort` 会清空当前聊天的 agent 会话（`core/engine.go:5100, 5210`）；`/mode` 不会。
- model / effort / mode 都是**整个 project 共享**，且不写回配置；重启丢失；Echo 任务会被聊天里的切换悄悄影响。

### 2.2 平台 × 功能

| 功能 | Feishu | Telegram |
|---|---|---|
| 卡片（`/list /cron /loop /queue /help`） | ✅ 原生卡片 | ⚠️ 文本 + inline keyboard 退化 |
| 选择按钮（model/effort/mode/权限） | ✅ | ✅ |
| `/auth`（仅私聊） | ✅ | ✅ |
| agent → 用户发文件 | ✅ | ❌ 没有实现 `FileSender` |
| 收图片 / 文件 / 语音 | ✅ | ✅ |
| 原生命令菜单 | n/a | ⚠️ 带连字符或大写的命令被丢弃（如 `agent-upgrade`），`attach/loop/tts` 缺 i18n 描述 |
| 模型选择器显示“不在列表中的当前模型” | ✅ | ❌ |

### 2.3 Echo 协同现状

- 协议：echo-client 主动 WebSocket 连到 `{server}/api/v1/workers/ws`，`hello → register_host → register_agents → heartbeat`，Echo 下发 `assign_task / cancel_task` 和一组工作区 / git 操作；进度通过每 500ms 轮询的全量快照 `task_progress / task_result` 回传（`core/worker_client.go`）。
- Echo 侧**没有 agent_type 白名单**，新增 kimi/agy 不需要改 Echo 代码即可注册。
- echo-client 侧有多处**硬编码的 agent 列表**需要更新：`isEchoYoloEquivalentMode`（`core/worker_client.go:912`）、`nativeSkillTargetKind`（`core/native_skills.go:31`）、`CC_EXTRA_WORK_DIRS`（`core/job_runner.go:468`）、doctor、agent_upgrade、config 等。
- `assign_task` **没有** model / effort / mode / session 字段；每个 Echo 任务都新开会话，结果里的 `session_id` 也不回传给 Echo。
- 可靠性问题：WebSocket 断开时结束的任务，其 `task_result` 只发一次就丢；进度发送失败后 watcher 永久退出；重连后不重新同步运行中的任务；`request()` 没有超时。

### 2.4 顺带发现的问题

1. Codex `--full-auto` 已从 CLI 删除，`auto-edit/full-auto/auto` 模式不可用。
2. 本机 Codex CLI 是 0.153.0，最新稳定版 0.167.2；服务日志里的 ``Model metadata for `gpt-6-astra` not found`` 很可能就是 CLI 过旧。
3. 本机只装了已归档的 legacy `kimi-cli` 1.22.0（`~/.local/bin/kimi-cli`），**没有** Kimi Code CLI（`kimi`）。
4. 上游 agy 适配器有两处已过时：认为 agy 没有 `--model`（实际 1.3.4 起已支持）；把 plan 模式映射成 `--sandbox`（应为 `--mode plan`）。
5. 上游 Kimi 适配器在 Kimi Code 2.x 上把 `--plan` 和 `--prompt` 一起传，会直接报错。

### 2.5 Agent 空闲自动升级现状

实现位于 `core/agent_upgrade.go`（`AgentUpgradeManager`），配置为 `[agent_updates]`，聊天命令是 `/agent-upgrade`。

**两个服务的配置**

| 服务 | policy | 启用的目标 |
|---|---|---|
| `com.echo-client.cc-connect` | `idle_only`，24h | claudecode |
| `com.echo-client.cc-connect-codex` | `idle_only`，24h | codex、gemini（observe）、qoder |

**日志里看到的实际情况（11-05 至 11-11）**

- 每天都会触发一次 `agent upgrade auto-run`。
- **Codex**：11-05 探测到的版本是 `codex-cli 0.166.0`。11-06 起，升级前和升级后的版本都是空的（探测失败），`changed=false`。
- npm 日志显示每天都执行一次 `npm install -g @openai/codex@latest`，每次耗时 8–15 分钟。**即使已经是最新版也会重装。**
- 11-11 那次安装中途中断：日志停在下载 tarball，没有 exit 行。结果 `/opt/homebrew/lib/node_modules/@openai/codex` 已经是 0.167.1，但 `/opt/homebrew/bin/codex` 这个 bin 链接不见了。

**根因：升级的不是 agent 实际运行的那个二进制**

1. **本机有两份 codex**：
   - `~/.local/bin/codex`：standalone 安装，指向 `~/.codex/packages/standalone/current`，版本 0.153.0。它自带 `codex update`（自我升级）和 `codex doctor`，doctor 报告 `install consistent`，并提示 0.167.2 可升级。注意 `~/.codex/packages` 是指向外置卷 `/Volumes/extended/OffloadedHome/...` 的符号链接；
   - npm 全局：`@openai/codex`。包目录是 0.167.1，但缺少平台依赖 `@openai/codex-darwin-arm64`，运行时直接报错，已经无法使用。
2. **升级器和 agent 用的 PATH 不同**：
   - 升级器通过 `/bin/sh -lc` 执行命令（`core/agent_upgrade.go:906`）。登录 shell 会经过 `path_helper` 和 `~/.profile` 重排 PATH，以前 `/opt/homebrew/bin` 排在前面，所以探测到的是 npm 那份。
   - agent 进程直接用服务的 launchd PATH 执行 `codex`，`~/.local/bin` 排第一，**一直跑的是 standalone 0.153**。服务日志里的 ``Model metadata for `gpt-6-astra` not found`` 也是这个原因。
   - 今天 09:51，Antigravity 安装器往 `~/.profile` 里加了 `export PATH="$HOME/.local/bin:$PATH"`，登录 shell 的解析结果也随之变了。PATH 顺序是由别的安装器顺手决定的，很脆弱。
3. **Codex 不检测安装路线**：Claude 会识别 native / brew / npm（`resolveClaudeCodeUpgradeTarget`），Codex 固定用 npm。

**其他缺陷**

- **失败不可见**：
  - `auto-run` 日志不打印 `Err` 和输出；
  - 版本探测失败直接吞掉；
  - 失败计数只在内存里，重启后清零。
  - 所以 11-06 起探测为空的具体原因已经无法从日志还原。
- **重复重装**：没有“检查最新版本”这一步，每天无条件跑 `@latest`，一次 10 分钟以上，中途被杀就会留下坏掉的安装。
- **调度不可靠**：
  - 下次运行时间 = 进程启动时间 + 24h，不持久化，每次重启都会重新计时；
  - “空闲”只是触发那一刻没有忙碌的聊天会话。是否包含 Echo 任务和 cron 进程还没有确认；
  - 没有可配置的静默时间窗；
  - 升级过程中不会阻止新的轮次开始。
- **两个服务互不协调**：同一台机器上两个服务各自升级同一套二进制，没有锁。
- **升级之后**：
  - 常驻的 Claude 进程不会重启，会继续跑旧版本；
  - 模型缓存、能力探测不会刷新；
  - 没有升级后的冒烟检查，也不会通知任何人。
- **覆盖面**：
  - 没有 Kimi、agy 的升级目标；
  - Telegram 菜单里丢掉了 `agent-upgrade` 命令（§2.2）。

## 3. 各 agent 最新能力与透传方式

> 调研日期 2026-11-12。本机版本：Claude Code 2.1.322（最新）、Codex 0.153.0（最新 0.167.2）、agy 1.5.6（最新）、Kimi Code 未安装（最新 2.5.3）。

### 3.1 能力对照

| 能力 | Claude Code | Codex | agy | Kimi Code |
|---|---|---|---|---|
| **推荐接入方式** | `claude -p --input-format stream-json --output-format stream-json` + `control_request` 控制协议（现有方式，扩展使用） | `codex app-server`（JSON-RPC over stdio；官方 SDK 也基于它） | 第一期：每轮 `agy -p --output-format stream-json`；第二期：`agy acp` | 第一期：每轮 `kimi -p --output-format stream-json`；第二期：`kimi acp` |
| **远程登录** | PTY 跑 `claude auth login`，转发 URL，用户回贴 code（现有）；`claude auth status --json`；`claude auth logout`。另有未公开的 `claude_authenticate` 控制请求 | `account/login/start {type:"chatgptDeviceCode"}` → `verificationUrl + userCode`，等待 `account/login/completed`；或 `codex login --device-auth`；`codex login status`；`codex logout`。设备码需在 ChatGPT 安全设置中启用 | PTY 跑 `agy acp --login`，转发 URL、回贴 code；无 status/logout 子命令，可用 `agy -p /usage --output-format json` 探测 | `kimi login` 设备码流程，URL 和 code 打到 stderr，最容易转发；无 status/logout 子命令（ACP 有 logout） |
| **模型列表** | 控制请求 `list_models`（返回 `supportedEffortLevels`、`supportsAutoMode`） | `model/list`（返回默认模型、`supportedReasoningEfforts`） | `agy models`（`id<TAB>label`，id 带 effort 后缀） | `kimi provider list --json`（**含密钥，禁止记日志**）；或静态别名表 |
| **切换模型** | 控制请求 `set_model`，热切换，不丢会话 | `turn/start {model}`，对后续轮次生效 | `--model` | `-m <alias>` |
| **思考强度** | `--effort` / `apply_flag_settings {effortLevel}`：low/medium/high/xhigh/max/ultracode（随模型） | `turn/start {effort}`：low…max/ultra（随模型） | `--effort low…max`（随模型） | 无 CLI 参数；需 `KIMI_MODEL_THINKING_EFFORT` 或每会话 `KIMI_CODE_HOME` 配置；ACP 有 config option |
| **权限模式** | `set_permission_mode` 热切换：default/acceptEdits/plan/auto/dontAsk/bypassPermissions | `turn/start {approvalPolicy, sandboxPolicy, approvalsReviewer}`：on-request（转发审批）/ auto_review（“替我审批”）/ yolo；`collaborationMode: plan` | `--mode accept-edits\|auto\|plan`、`--dangerously-skip-permissions`；默认模式下需审批的操作会被软拒绝（上游用 PreToolUse hook 转发到聊天） | `-p` 下**固定为自动通过**，不能和 `--yolo/--auto/--plan` 同用；只有 ACP 能切换 |
| **会话** | `--resume`，transcript 在 `~/.claude/projects/` | `thread/list / resume / fork / name/set / archive / delete` | `--conversation <id>`；无非交互列表命令（扫描本地文件） | `kimi session list --json`、`-S <id>` |
| **轮内插入** | stream-json 用户消息带 `priority:"next"`（当前工具调用结束后读入）、`"later"`（排到下一轮）、`"now"` | `turn/steer`；另有 `thread/queue/*` 原生队列 | 仅 ACP（运行中发 prompt 即 steer）；`-p` 不支持 | 仅 ACP / `kimi web` API（`prompts:steer`）；`-p` 不支持 |
| **中断** | 控制请求 `interrupt` | `turn/interrupt` | ACP `cancel`；`-p` 只能杀进程 | ACP `cancel` |
| **loop / 调度** | `/loop`、`ScheduleWakeup`、`/goal`（会话内 Cron 工具，但 stream-json 模式下进程不会等待唤醒） | 无 cron；`thread/goal/set` | `/schedule`、`/goal`；`-p` 会等后台任务 | Cron 工具、`/goal`；`-p` 会等后台任务 |
| **用量** | `result.total_cost_usd`、`rate_limit_event`、控制请求 `get_usage`（5h / 7d）、`get_context_usage` | `account/rateLimits/read`、`thread/tokenUsage/updated` | `-p /usage`；结果里有 token 用量 | 仅 TUI `/usage` |
| **提问 / 计划审批** | `can_use_tool` 里的 AskUserQuestion、ExitPlanMode | `item/tool/requestUserInput`、`item/*/requestApproval` | `-p` 自动跳过 | `-p` 下 plan 不可用 |
| **compact** | `/compact` | `thread/compact/start` | `/compact` | `/compact` |

### 3.2 透传原则

1. **能热切换的就热切换**：Claude 用控制协议、Codex 用 `turn/start` 参数，切换模型 / effort / 模式不再重建会话。
2. **每轮一个进程的 agent（第一期的 agy、Kimi）**：把设置作为下一轮的启动参数，在下一条消息生效，并在回复里说明。
3. **选项从 CLI 实时获取**：模型及其支持的 effort 列表来自 `list_models` / `model/list` / `agy models`，不再硬编码。
4. **标注不稳定接口**：Claude 的 `list_models`、`get_usage`、`claude_authenticate` 等控制请求未写入文档。使用时必须有回退路径，且调用失败不能影响主流程。

## 4. 上游 cc-connect：取舍

上游（chenhg5/cc-connect）自 2026-03-10 分叉后有 1341 个新提交；本 fork 有 209 个独有提交（Echo worker、jobs、review 流程、prompt queue、`/auth`、cron 会话绑定、模型发现等）。试合并结果：**579 个冲突块，83 个文件**，`core/engine.go` 一个文件就有 116 处；还有同一功能两套实现（队列、reasoning 接口、cron 会话模式、多工作区）以及 `AgentSession.Send`、`FileSender` 等接口签名变化。

**结论：选择性移植。**

| 上游能力 | 处理 |
|---|---|
| `agent/kimi`、`agent/antigravity` + `antigravityhook` | **移植**（需补 4 个 core 小改动，已在 scratch 克隆验证编译与单测通过），并修正 §2.4 的问题 |
| `/timer`（一次性定时任务） | 移植，复用 fork 的 cron 投递路径 |
| `/cancel`（停止并新开会话） | 移植 |
| `max_turn_time_mins`、`agent_session_idle_timeout_mins` | 移植 |
| Claude `LiveModeSwitcher` | 用控制协议重新实现（见 T3.1） |
| `/ps` 轮内插入 | 思路参考，按 §3 各 agent 原生能力重新实现 |
| 生命周期 hooks、webhook | 可选（P5 之后） |
| Feishu 稳定性修复（open_id 拉取失败即拒绝、名字查询 5 秒超时、连发图片合并、大文件分块下载、瞬时错误重试） | 逐个 PR 挑选移植 |
| `agent/acp` 通用 ACP 适配器 | 第二期移植，用于 agy / Kimi 的轮内插入与热切换 |
| management API / Web UI / bridge / observer / heartbeat / 其他平台 / Telegram 换库 | 不做 |

上游**没有**通用的远程登录能力（`core/setup.go` 只是 Feishu / 微信扫码建应用），这部分只能自己做。

## 5. 设计

### 5.1 Agent 能力声明（Capabilities）

新增可选接口，代替散落各处的 `agent.Name() == "xxx"` 判断：

```go
// core/capabilities.go（P0 已实现）
type AgentCapabilities struct {
    Auth           AuthCaps    // login / status / logout，Flow: url_code | device_code
    Models         bool        // 由 ModelSwitcher 推导
    LiveModel      bool        // 不重启进程即可切换（P3/P4 声明）
    Efforts        []string    // 由 ReasoningLevelSwitcher 推导；按模型变化留到 P3
    EffortViaModel bool        // 思考强度由模型档位决定（Qoder），/effort 提示改用 /model
    Modes          []string    // 由 ModeSwitcher 推导；规范值与别名见 core/permission_mode.go
    LiveMode       bool
    Sessions       SessionCaps // list / delete / history，由接口推导
    Steer          bool        // 轮内插入
    Interrupt      bool        // 优雅中断
    Compact        bool        // 由 ContextCompressor 推导
    Usage          bool
    AskUser        bool        // 提问 / 计划审批卡片
}
```

能推导的字段（模型、强度、模式、会话、compact）一律从 agent 实现的接口推导，声明不能把它们关掉；只有无法推导的字段（Auth、Live*、Steer、Interrupt、Usage、AskUser、EffortViaModel）通过 `CapabilityProvider` 声明。`core.ValidateCapabilities` 检查“声明即实现”：Steer / Interrupt / Usage / AskUser 在 core 有对应接口之前一律不允许声明为 true。

用途：`/help` 和各命令据此显示或隐藏选项；不支持时给出明确提示；Echo 注册时作为 metadata 上报（§5.6）；单元测试据此校验“声明即实现”。

### 5.2 统一的权限模式

聊天中只暴露五个规范值，各 agent 映射到原生参数：

| 规范值 | Claude | Codex（app-server） | agy | Kimi（-p） |
|---|---|---|---|---|
| `default` | `default` | `approvalPolicy=on-request`，审批转发为卡片 | 默认 + PreToolUse hook 转发 | 不支持 |
| `acceptEdits` | `acceptEdits` | `sandbox=workspace-write` + on-request | `--mode accept-edits` | 不支持 |
| `plan` | `plan` | `collaborationMode=plan` | `--mode plan` | 不支持（第二期 ACP） |
| `auto` | `auto` | `approvalsReviewer=auto_review`（`--approve-for-me`） | `--mode auto` | 不支持（`-p` 不审查任何动作，按 bypass 算；P6 用 ACP） |
| `bypassPermissions` | `bypassPermissions` | `--yolo` 等价 | `--dangerously-skip-permissions` | ✅（同上） |

已有别名继续兼容（`yolo` → `bypassPermissions`，Codex 的 `full-auto` → `auto`）。非法值报错，不再静默回到 default。`isEchoYoloEquivalentMode` 改为查这张表。

### 5.3 设置的作用域与持久化

- model / effort / mode **改为按聊天会话保存**，并持久化到 session 存储；project 配置只作为默认值。
- `/model`、`/effort` **不再清空会话**：能热切换的立即生效，每轮新进程的 agent 从下一轮生效。
- Echo 任务不再受聊天中切换的影响，而是用任务自带的选项或 project 默认值（§5.6）。

### 5.4 命令语义（四个 agent 统一）

| 命令 | 语义 | 说明 |
|---|---|---|
| `/auth login\|code\|status\|logout\|cancel [agent]` | 远程登录 | 仅私聊、仅白名单用户；URL / 设备码以卡片发送；code 与 token 一律脱敏，不写日志 |
| `/model` | 列出当前账号可用的最新模型（卡片），选择即生效 | 来源见 §3.1 |
| `/effort` | 选项随当前模型变化 | 不支持的 agent 给出提示 |
| `/mode` | §5.2 的五个规范值 | |
| `/list /switch /new /name /delete` | 会话管理 | `/delete` 同时调用 agent 的 `SessionDeleter` |
| `/cron` | 支持绑定会话；新增每个任务自己的 model / effort / mode | 本地 API 增加 `agent_session_id` |
| `/timer` | 一次性定时任务 | 移植自上游 |
| `/loop` | 固定间隔；新增自定节奏：agent 输出 `<loop-control>next 20m</loop-control>` 决定下一次运行时间 | 复用现有 loop-control 解析 |
| `/goal <条件>` | 透传原生 goal（Claude `/goal`、Codex `thread/goal/set`、agy/Kimi `/goal`） | |
| 忙时新消息 | 默认：支持 steer 的 agent **插入当前轮**，否则排队；卡片提供“插入 / 排队 / 取消”按钮 | 新增 `/steer <消息>` 强制插入、`/queue` 管理队列 |
| `/stop` | 优雅中断（Claude `interrupt`、Codex `turn/interrupt`），超时后再杀进程 | |
| `/cancel` | 停止并新开会话 | |
| `/compact` | 各 agent 原生 compact | |
| `/usage`、`/context` | 用量 / 额度 / 上下文占用 | Claude、Codex 先做 |
| `/status` | 增加 agent、CLI 版本、模型、effort、模式、登录状态、用量 | |

### 5.5 平台拉齐

- **Telegram**：实现 `FileSender`；命令菜单把 `-` 转成 `_` 注册并做反向映射；补 i18n 描述；模型选择器显示当前模型；改进 `/list /cron /queue` 的 keyboard 呈现。
- **Feishu**：挑选移植上游的稳定性修复（§4）；AskUserQuestion、计划审批、登录、插入 / 排队 都使用卡片。
- 每个新能力都要在两个平台各过一遍验收清单（§7）。

### 5.6 与 Echo 协同

echo-client 侧：

1. `register_agents` 的 `metadata` 增加 `capabilities`（§5.1）、`cli_version`、`auth_status`、`default_model / effort / mode`。
2. `assign_task` 增加可选 `agent_options {model, effort, mode, resume_session_id}`；按任务生效，不修改共享 agent 状态。
3. `task_result` 回传 `agent_session_id`，Echo 下次可以续接同一会话。
4. 可靠性：`task_result` 在重连后补发；重连后重新同步运行中的任务；`request()` 加超时；进度发送失败时 watcher 不退出。
5. kimi / agy 加入 yolo 等价表、native skills 目录、extra work dirs、doctor、agent_upgrade。

Echo 侧（独立仓库，单独 PR）：

- 接受新 agent_type **不需要改代码**。
- 可选：调度时按 `agent_type` / capabilities 选择 agent；从 `task.input` 透传 `agent_options`；`AgentSessionSummary.source` 增加 `kimi`；更新部署脚本与文档中的 agent 列表。

## 6. 任务列表

> 估时按一人计；依赖关系标在“依赖”列。第 0 阶段之后，P1、P2、P3、P4 基本可以并行。

### P0 基础与修复（约 3 天）

| ID | 任务 | 依赖 | 验收 |
|---|---|---|---|
| T0.1 | 新增 `AgentCapabilities` 接口；`/help`、`/status` 按能力显示；去掉 `agent_auth.go:362`、`engine.go:5131` 等处的硬编码判断 | — | 能力矩阵单测通过 |
| T0.2 | 规范化权限模式（§5.2）；非法值报错；`isEchoYoloEquivalentMode` 改为查表 | T0.1 | 各 agent 的模式映射单测 |
| T0.3 | model / effort / mode 按聊天会话保存并持久化；`/model`、`/effort` 不再清空会话 | T0.1 | 切换后继续对话，上下文仍在；重启后设置仍在 |
| T0.4 | 修复：Codex `--full-auto`（先用 `--sandbox workspace-write` + approval 参数替代）；`/delete` 调用 `SessionDeleter`；Claude 模型发现的分页与重定向 | — | 回归测试 |
| T0.5 | 运维：本机 Codex 只保留 standalone（§8 问题 9 已定）。**2026-11-12 已完成**：删掉 npm 那份（先清掉 11-10 中断安装留下的 0.167.0 暂存目录 `.codex-QDftHxCm`，否则 npm 卸载报 `ENOTEMPTY`）；`~/.cc-connect-codex/config.toml` 的 `[agent_updates.codex]` 改为 `strategy = "observe"` 加 `version_command = "/Users/edward/.local/bin/codex --version"`（备份为 `config.toml.bak-20261112-codex-standalone`），已重启服务。`codex update` 报 “Could not detect the Codex installation method”，因为 `~/.codex/packages` 链接到外置卷，安装被识别为 “other”，所以不能用它升级。**2026-11-17**：`[agent_updates.codex]` 改为 `strategy = "custom"`，`update_command` 先把官方安装脚本 `https://chatgpt.com/codex/install.sh` 下载到临时文件，再以 `CODEX_NON_INTERACTIVE=1` 执行。没有用 `curl \| sh`，是为了让下载失败能返回非零退出码。已是最新时脚本不会重新下载。备份为 `config.toml.bak-20261117-codex-installer`，已重启服务。**待确认**：下一次自动运行（约 11-18 10:57，空闲时）后，版本应从 0.153.0 变成最新。另外安装 Kimi Code CLI 2.5.x；doctor 显示 CLI 版本、实际解析到的路径、PATH 上的重复安装 | — | `echo-client doctor` 输出四个 agent 的版本与路径；服务实际执行的 codex 是最新版 |

**P0 实现记录（2026-11-17，分支 `feat/p0-agent-parity-foundation`）**

- **T0.1**：`core/capabilities.go`（`AgentCapabilities`、`CapabilityProvider`、`CapabilitiesOf`、`ValidateCapabilities`）。`/auth` 按 `Auth` 能力放行，`/effort` 的 Qoder 提示改为 `EffortViaModel`；`/help` 隐藏当前 agent 不支持的命令块，末尾的权限模式提示换成 agent 自己的模式；`/status` 显示当前聊天的模型、强度、模式和能力摘要。Claude 声明 `Auth`，Qoder 声明 `EffortViaModel`。其余按名字区分的地方（native skills、extra work dirs、agent_upgrade、`shouldResetAgentSessionOnResult`）属于 P1 / P7 / P10，未改。
- **T0.2**：`core/permission_mode.go` 是唯一的模式对照表，覆盖全部 7 个 agent。全部 7 个适配器（审查后补齐）和 `isEchoYoloEquivalentMode` 都查这张表；旧 switch 接受的值全部保留（有回归单测）。`/mode` 遇到未知值报错，不再静默回到 default。Codex 的原生模式键改为规范值（`default / acceptEdits / auto / bypassPermissions`），`suggest / auto-edit / full-auto / yolo` 作为别名继续可用。Codex exec 没有 plan 模式，`/mode plan` 会报错（P4 后再支持）。
- **T0.3**：新增 `SessionOptionsInjector`；设置保存在 `Session.AgentSettings`，随会话文件持久化（顺带修复 `NeedsReplay` 一直没有写盘的问题），`/new` 沿用当前聊天的设置，`/switch` 恢复目标会话自己的设置。`/model`、`/effort`、`/mode` 不再清空会话，只把进程标记为 stale，下一轮用新设置续接同一个 agent 会话，正在运行的一轮不会被打断。cron 使用所绑定聊天的设置，relay 使用自己的会话，Echo 任务始终用项目默认值。没有实现该接口的 agent（gemini、qoder 等）保持原来的项目级行为，但同样不再清空会话。
- **T0.4**：Codex 改用 `-c sandbox_mode / approval_policy / approvals_reviewer`。本机 0.153 实测：`exec resume` 拒绝 `--full-auto` 和 `--sandbox`（旧代码在续接时传的正是 `--full-auto`），而 `-c` 配置项能正常通过参数解析。`acceptEdits` 用 `approval_policy="never"`，因为 exec 无法转发审批（P4 再改为卡片审批）。Claude `/v1/models` 改为分页（`has_more / after_id`，最多 10 页），并改用不跟随重定向的 client，避免 API key 被转发到别的域名。
- **`/delete` 与原文的偏差**：`SessionManager.DeleteSession` 的注释写明有意保留 agent transcript（2026-09 的决定，transcript-collector 也依赖这些文件），所以默认行为不变；新增 `/delete <序号> purge` 才调用 `SessionDeleter`。如果该 transcript 仍被引用（范围见下方审查第 1 条），就只删本地记录、保留 transcript。
- **P0 代码审查（2026-11-17，Codex + Kimi Code，使用仓库的 comprehensive-review 与 security-review skill）**：去重核实后 8 条成立，均已修复并补回归测试：
  1. purge 的引用检查扩展到本进程内同类 agent 的所有项目会话、所有 cron 任务和正在运行的聊天（原来只查本项目的会话记录）。
  2. 设置命令与 `/new`、`/switch` 共用会话切换锁，避免新会话继承旧的 bypass 模式。
  3. Gemini、Qoder、OpenCode、iFlow、Cursor 的模式解析也改用共享表（原来 Cursor 不认 `bypassPermissions`，Echo 注册能通过但实际以 default 运行）。
  4. purge 先删 transcript、成功后再删本地记录（失败可重试）；Claude 按 ID 在所有项目目录中精确查找，不再依赖当前 work_dir。
  5. purge 的检查和删除与 `/attach` 共用进程级的 transcript 锁。
  6. 删除前校验 agent 会话 ID 格式；Codex 从 `rollout-<时间戳>-<id>.jsonl` 中解析完整的 thread id 再比较（子串和后缀匹配都可能命中别的会话）。
  7. `/auth` 运行时同时要求“该 agent 和流程确有登录实现”，并按 login / status 分别检查能力。
  8. `/status` 和模型选择器按“聊天设置 → provider 模型 → 项目模型”显示，与实际启动一致。

  剩余风险：purge 看不到另一个服务进程里的引用（Claude 服务和 Codex 服务各是一个进程）；切换设置会重置“本会话全部允许”的状态（与改动前的 `/mode` 一致，属于有意保留）；Codex 的模式参数只用假 CLI 和 `exec resume` 参数解析验证过，还没跑过真实的一轮（2026-11-17 已补实测，见下一条）。
- **P0 实测（2026-11-17，`go test -tags live`，真实 CLI 与账号）**：
  - Codex：`acceptEdits`、`auto`、`default` 三种模式下，新线程和续接线程（`exec resume`）都能完成一轮；前两种模式实际写出了文件。修复前续接线程会因为 `--full-auto` 直接失败。
  - 更正一处描述：`default` 不加任何覆盖，写不写得了文件取决于 `~/.codex/config.toml`。本机设置了 `approvals_reviewer = "auto_review"`，所以 `default` 也能写，并不是“只读沙箱”。README、配置示例和模式说明已改正。
  - Claude：按会话设置的 model / effort / mode 确实传给了 `claude` 进程；从 haiku 切到 sonnet 并 `--resume` 后，仍然记得上一轮的内容，说明 `/model` 切换不会丢上下文。
  - 还需要人工验收的部分：Feishu 和 Telegram 里 `/status`、`/model`、`/mode` 的卡片与按钮显示。
  - 实测用例在 `agent/codex/live_test.go`、`agent/claudecode/live_test.go`、`core/agent_upgrade_live_test.go`，都带 `live` build tag，不进默认测试。
- **T0.5**：`core/agent_cli.go` 与新的 `echo-client doctor [agent-type...]` 子命令：按当前进程的 PATH 解析 CLI，输出版本、实际路径、符号链接目标，并标出 PATH 上的其他副本和损坏的安装；聊天里的 `/doctor` 用同一套逻辑。**Kimi Code CLI 尚未安装**：需要从 npm 安装第三方包，留到 P1 开始时由你确认后再装。

### P1 接入 agy 与 Kimi（约 3 天）

| ID | 任务 | 依赖 | 验收 |
|---|---|---|---|
| T1.1 | 移植 core 小改动：`core/cmdopts.go`、`GetProviderModel(s)` + `ProviderConfig.Models`、`ContinueSession`、`Event.ToolStatus/ToolSuccess` | — | `go build ./...` |
| T1.2 | 移植 `agent/antigravity`、`agent/antigravityhook`、`_agy-permission-hook` 子命令；修正：`--model` 透传、`--effort`、`--mode accept-edits\|auto\|plan`、用 `agy models` 列模型 | T1.1 | 真实 agy 冒烟：对话、续接、切模型、切 effort、默认模式下审批卡片 |
| T1.3 | 移植 `agent/kimi`，只保留 Kimi Code 2.x（legacy 已归档）；修正：`-p` 下不传 `--plan/--yolo`，只声明 `auto / bypassPermissions`；用 `kimi session list --json` 列会话；模型列表读取时脱敏；effort 先做 spike（`KIMI_MODEL_THINKING_EFFORT` 或每会话 `KIMI_CODE_HOME`） | T1.1, T0.5 | 真实 kimi 冒烟：对话、续接、切模型、图片 |
| T1.4 | 更新硬编码列表：main.go 导入、doctor、agent_upgrade、`config.AgentUpdates`、native skills、`job_runner` extra dirs、yolo 等价表、`config.example.toml`、README | T1.2, T1.3 | 两个 agent 都能作为 Echo worker 注册 |

**P1 实现记录（2026-11-17）**

- **T1.1**：只移植了真正需要的 `core/cmdopts.go`（`ParseCmdOpts`、`ParseConfigEnv`，连同上游测试）。`GetProviderModel(s)`、`ContinueSession`、`Event.ToolStatus/ToolSuccess`、带 messageID 的 `Send` 都没有移植：两个新适配器改写成本 fork 的接口，不需要它们。新增 `core.ResolveAgentCLI`：PATH 上找不到时去厂商默认安装目录找（目前只有 Kimi 的 `~/.kimi-code/bin`，服务的 PATH 通常没有它），适配器、doctor、升级器共用。
- **T1.2 agy**（`agent/antigravity`，agy 1.5.10 实测）：每轮 `agy -p --output-format stream-json`，会话 ID 直接取 init 事件的 `conversation_id`，续接用 `--conversation`；模型用 `agy models`，按基础模型分组（`gemini-4-argon` + low/medium/high），切换用 `--model <基础模型> --effort <档位>`（agy 拒绝“带档位后缀的 id + --effort”）；五个模式：`default`（PreToolUse hook 经 `--gemini_dir` 配置覆盖层转发到聊天审批，只读工具直接放行）、`acceptEdits`/`plan`/`auto`（`--mode`）、`bypassPermissions`（`--dangerously-skip-permissions`）；`/list` 用 sqlite3 命令只读查询 `conversation_summaries.db`，没有 sqlite3 时只列出该目录最近一次会话。真实冒烟：默认模式下审批卡片拒绝后文件确实没写、acceptEdits 直接写、续接时切模型和 effort 上下文仍在、`/list` 能看到。
- **T1.3 Kimi**（`agent/kimi`，Kimi Code 2.5.3 实测，只支持 2.x）：每轮 `kimi -p --output-format stream-json`，续接用 `-S <id>`；`-p` 不能和 `--plan/--yolo/--auto` 同用、且不经确认执行所有工具，所以只提供 `bypassPermissions` 一个模式（`auto` 不作为别名，因为没有任何审查）；模型来自 `kimi provider list --json`，只解码模型别名、显示名和支持的 effort，凭据字段不读不记日志；effort spike 结论：环境变量 `KIMI_MODEL_THINKING_EFFORT` 对托管的 Kimi Code 模型生效（会话记录里 LLM 请求的 `thinkingEffort` 随之改变），模型不支持该档位时不传。会话列表和历史直接读 `~/.kimi-code/sessions/*/<id>/` 下的 `state.json`、`agents/main/wire.jsonl`。真实冒烟：写文件、续接时切模型和 effort、图片、`/list` 和历史都通过。
- **T1.4**：两个 agent 加入 main.go、doctor、`agent_updates`（T10.6）、native skills（`.agents/skills`）、job runner 的 extra dirs、模式表、`config.example.toml`、README。作为 Echo worker 注册时需要配置 `mode = "bypassPermissions"`（Kimi 也要显式写，和它实际行为一致）。
- **T10.6**：Kimi 默认 `kimi upgrade --yes`、agy 默认 `agy update`（两者都实测可无人值守运行），命令针对实际解析到的那份二进制执行；npm 装的 Kimi 自动改用 npm。legacy `kimi-cli` 1.22.0 仍通过 uv 装着（`~/.local/bin/kimi-cli`，数据在 `~/.kimi`）；它没有 `kimi` 链接，不会遮住 Kimi Code，适配器也不再支持它。是否卸载（`uv tool uninstall kimi-cli`）由你决定，未自动清理。
- **顺带修复**：Codex、Kimi、agy 在每轮提示里附带的“选项按钮”说明会出现在 `/list` 摘要和历史里（Codex 一直如此）；现在统一用 `core.StripInteractionOptions` 去掉。Kimi 和 agy 把这段说明放在用户消息之后，因为 agy 只保存消息开头的一小段预览。
- **尚未做**：两个 agent 的轮内插入、热切换、Kimi 的其他模式都等 P6（ACP）；agy 会话历史（`/history`）需要解析 protobuf，没做；聊天里的卡片和按钮还需要在飞书 / Telegram 上手动过一遍。

**第二轮 review（2026-11-17，Codex + Kimi Code，范围 `adf799f7..abe2bb9c`）**：16 个问题全部修复，主要行为变化：

- Kimi 必须在配置里明确写 `mode = "bypassPermissions"`，否则不启动；聊天会话里存着、但当前 agent 不提供的模式（比如换 agent 之前存的 `plan`）既不生效也不在 `/status` 里显示。
- Kimi、agy 的 `/stop`、超时和关闭会杀掉整个进程组（工具起的命令一起停）；`Send` 与 `Close` 并发不再可能 panic；每轮只有一个结束事件；输出读取出错时结束这一轮而不是卡住；启动失败时删除已保存的图片；日志里的提示内容只保留长度。
- agy 审批卡片显示命令所在目录。
- relay 每轮结束都关闭 agent 会话，取消时立即返回；Echo 任务、cron、relay 启动 agent 前会等待正在进行的升级。
- 升级器：配置为 `observe` / `disabled` 的目标不会再被安装路线改成升级；拿不到跨进程锁就跳过；状态文件按顺序保存；Homebrew 装的 Codex 设置 `pin_version` 会被拒绝而不是改用 npm；`window` 写错在加载配置时报错，运行中遇到也会暂停自动升级。
- 所有平台都起不来时服务继续运行并在后台重试；第一次启动平台的过程中关闭服务，平台不会在关闭后又跑起来。
- 未处理：`go test -race ./core` 有 25 个测试报数据竞争，修复前就存在（多数在测试桩里，`core/job.go` 有一处在生产代码中）。

### P2 通用远程登录（约 3 天）

| ID | 任务 | 依赖 | 验收 |
|---|---|---|---|
| T2.1 | 把 `AgentAuthManager` 抽象为 `AgentAuthenticator`：`Start → {url, user_code?, needs_code}`、`SubmitCode`、`Status`、`Logout`、`Cancel`；`/auth` 支持指定 agent | T0.1 | 单测覆盖状态机与超时 |
| T2.2 | Claude：保留 PTY 登录；新增 `status`（`claude auth status --json`，含过期时间）与 `logout` | T2.1 | 真实登出 / 登录一轮 |
| T2.3 | Codex：设备码登录（`codex login --device-auth`；P4 之后改走 `account/login/start`）；`status`、`logout` | T2.1 | 真实设备码流程 |
| T2.4 | Kimi：解析 `kimi login` 的 stderr，发出 URL + code；status 用 spike 确定的方式探测；不支持 logout 时明确提示 | T2.1, T1.3 | 真实登录一轮 |
| T2.5 | agy：PTY 跑 `agy acp --login`，转发 URL、回贴 code；status 用 `agy -p /usage` 探测 | T2.1, T1.2 | 真实登录一轮 |
| T2.6 | Feishu、Telegram 私聊登录卡片；code / token 脱敏审计 | T2.1 | 两个平台各走通一次 |

**P2 实现记录（2026-11-18）**

- **T2.1**：`AgentAuthManager` 改为按 agent 的登录驱动（`core/agent_auth_drivers.go` 的 `authDriver`：`start`、`prompt` 解析登录提示、`status`、`logout`、覆盖登录的环境变量），仍然每个服务同时只允许一个登录。`/auth [status|login|logout] [claude|codex|kimi|agy]`，不写名字时用当前项目的 agent（需要它声明了登录能力）；`/auth code`、`/auth cancel` 作用于自己发起的那个登录。登录成功或退出后，只有用这个 agent 的项目会在下一轮重启进程。
- **T2.2 Claude**：PTY 登录不变；`status` 显示登录方式、订阅和过期时间（`claude auth status`）；新增 `logout`（`claude auth logout`）。
- **T2.3 Codex**：`codex login --device-auth`（PTY），从输出里取 `auth.openai.com` 的链接和 `XXXX-XXXX` 代码；不需要回贴，CLI 轮询到授权就结束。`status` 用 `codex login status`，`logout` 用 `codex logout`。需要先在 ChatGPT 安全设置里启用设备码登录。P4 之后可改走 app-server 的 `account/login/start`。
- **T2.4 Kimi**：`kimi login --region <本机已存的区域>`（不传会交互式询问区域），取 `kimi.com` / `kimi.ai` 的链接（链接里带 `user_code`）。没有 status 命令：看 `~/.kimi-code/credentials/*.json` 里有没有 refresh token（只判断是否存在，不读出值）。没有 logout 命令，`/auth logout kimi` 明确提示。
- **T2.5 agy**：PTY 跑 `agy acp --login`，取 `accounts.google.com` 的链接，等出现 “paste the authorization code” 后提示 `/auth code`。`status` 用 `agy -p /usage`（登录时输出额度）。没有 logout 命令。
- **T2.6**：登录提示在支持按钮的平台（飞书、Telegram）是卡片：“取消登录”，设备码登录多一个“查看状态”；按钮发出的仍是 `/auth …` 命令，私聊和管理员检查照常生效。退出登录需要 `/auth logout <agent> confirm`。审计：`/auth` 消息不进历史、不发给 agent，引用里出现时整体脱敏；status / logout 的 CLI 输出只取第一行；飞书解析失败的富文本不再把原文写进错误日志。所有链接都要求 https、没有端口和用户信息，且主机在各 CLI 的白名单内。
- **测试**：所有驱动在测试里都换成假的，测试不会运行真实的登录、状态或退出。真实登录 / 退出各一轮留到最后的整体验证时和你一起做（会让本机 CLI 退出登录）。

**P2 review（2026-11-18，Codex + Kimi Code，范围：P10 收尾 `c7cb7504` 和 P2）**：两边合并后 16 个问题，全部修复：

- 升级回滚：备份改为独立副本（APFS 上是写时复制克隆，不占空间），就地改写二进制的更新器不会把备份一起改掉；安装程序删掉二进制后也能恢复；回滚失败时保留记录和备份，下次启动重试；回滚只改符号链接，从最里层开始，回到旧二进制就停，指向同一位置的链接（相对 / 绝对、`/var` 与 `/private/var`）不动；从状态文件读回的记录要和当前安装的 CLI 路径一致才处理，备份只在备份目录里删；关闭服务时正在做的升后检查不再误判为失败。
- 登录：PTY 登录、status、logout 都按进程组结束，并限制输出等待时间；登录和退出不会同时进行；设备码只从完整的行里解析，链接里带的 `user_code` 必须是普通代码；Kimi 没有存过区域时要求 `/auth login kimi mainland-cn|global`，并遵循 `KIMI_CODE_HOME`；登录结束后的状态检查用新的 30 秒期限；logout 失败只记退出码，不记 CLI 输出。
- 按钮：Telegram 和飞书的按钮回调现在带上私聊标记（飞书根据之前收到的 p2p 消息记住私聊），“取消登录”“查看状态”不再被拒绝。
- 升级期间排队：消息在开始前碰上新开始的升级时放回队首，不丢。
- CLI 变化检测看的是适配器实际运行的二进制（Kimi、agy 的配置路径或安装目录）。
- 已是最新判断：预发布版本（`1.2.3-rc1`）低于正式版。
- 另外修复：工作区 `git fetch` 超时后，ssh 子进程占着输出会让命令永远不返回（这台机器上连 `example.invalid` 的 ssh 会挂住，测试因此卡死）；git 命令现在也按进程组结束。

### P3 Claude Code 深度能力（约 4 天）

| ID | 任务 | 依赖 | 验收 |
|---|---|---|---|
| T3.1 | 用控制协议热切换：`set_model`、`apply_flag_settings{effortLevel}`、`set_permission_mode` | T0.3 | 切换后同一进程继续，无重启 |
| T3.2 | 用 `list_models` 列模型，effort 选项随模型变化（含 xhigh、max、ultracode）；失败时回退到现有发现逻辑 | T3.1 | 卡片显示 Opus 5.5 / Fable 5.2 等最新模型 |
| T3.3 | 轮内插入：忙时发送 `priority:"next"`；卡片提供“插入 / 排队”；新增 `/steer` | T0.1 | 长任务中插话，agent 在同一轮内看到 |
| T3.4 | `/stop` 改为 `interrupt` 控制请求，超时后再杀进程 | — | 中断后会话可继续 |
| T3.5 | AskUserQuestion、ExitPlanMode 专用卡片（选项按钮 / 批准计划） | — | 两个平台都能回答问题、批准计划 |
| T3.6 | `/usage`（`get_usage` + `rate_limit_event`）、`/context`（`get_context_usage`）；`/status` 显示花费 | T0.1 | |
| T3.7 | cron 写入同一会话后，常驻 Claude 进程检测到 transcript 变化就重新 resume | — | cron 跑完后，聊天里能接着 cron 的上下文继续 |

**P3 实现记录（2026-11-18）**

控制协议在本机 Claude Code 2.1.326 上实测：`control_request {request_id, request:{subtype}}` → `control_response {subtype: success|error, request_id, response}`。`list_models`、`get_usage`、`get_context_usage`、`set_model`、`apply_flag_settings`、`set_permission_mode`、`interrupt` 都可用，不发提示词就不会调用模型。core 新增可选接口（`core/agent_live.go`）：`LiveSettingsApplier`、`Steerer`、`Interrupter`、`UsageReporter`、`ContextReporter`、`ModelEffortProvider`，Codex（P4）、agy / Kimi（P6）后续接同样的接口。

- **T3.1**：`/model`、`/effort`、`/mode` 先对运行中的进程热切换（只发变化的项），进程和上下文保留；切换失败（或清空 effort 这种做不到的）才回到原来的“下一轮重启进程”。切到 bypassPermissions 时自动批准跟着变。
- **T3.2**：模型列表优先用 `list_models`（不配 API key 时），缓存 10 分钟，失败回退到原来的 API / `/model` 解析。`/effort` 的选项按当前模型支持的级别过滤（加上 xhigh），模型列出的新级别（如 ultracode）会补进来。测试里禁止启动真实 Claude。
- **T3.3**：`/steer <内容>` 插入正在进行的这一轮（用户消息带 `priority:"next"`，实测同一轮内读到）；没有进行中的轮次就按新消息发送。忙时排队的消息回复里带“立即插入”按钮（`/queue steer <id>`）。插入的内容记入会话历史。
- **T3.4**：`/stop` 对支持中断的 agent 发 `interrupt`，等这一轮结束（最多 15 秒）；进程和会话保留，之后还能继续（实测）。等不到才回到原来的结束进程。这一轮的结果标为“已中断”，不当成错误，也不提示“空回复”。中断在这一轮刚结束时才到达的话，不会误标下一轮。
- **T3.5**：AskUserQuestion 变成问题卡片（每个选项一个按钮，可回序号、选项文字，或直接回复自己的答案，多选回 `1,3`），答案按 `{"answers": {问题: 选项}}` 回给工具；多个问题依次问。ExitPlanMode 显示计划和“批准计划 / 继续规划”，直接回复文字就是修改意见（拒绝并带上意见）。这两种在 bypass 和“全部允许”下也一定问用户。
- **T3.6**：`/usage` 显示套餐、5 小时 / 7 天额度用量和重置时间、本进程花费（有运行中的进程就问它，否则起一个只发控制请求的临时进程）；`/context` 显示上下文用量和主要组成；`/status` 增加“花费（当前进程）”。`rate_limit_event` 暂未单独处理（`/usage` 已能看到额度）。
- **T3.7**：cron 的独立轮次续写了某个 agent 会话后，持有这个会话的聊天会在下一轮前重启进程，resume 到包含 cron 内容的 transcript。

**P3 review（2026-11-18，Codex + Kimi Code）**：两边合并后 14 个问题，全部修复：

- 问题卡片的按钮发送选项序号，不再发送选项文字（选项文字是 agent 写的，可能像 `/mode yolo` 这样的命令）；同一个问题的多个回答只取第一个（加锁，避免并发回答 panic 或重复回复）。
- 离开 bypass 时先关掉自动批准再请 CLI 切换；热切换成功后清掉之前的“全部允许”；正在进行的轮次不做热切换（共享聊天里别人的 `/mode` 不会在这一轮中途放宽权限），改为下一轮重启生效。
- 插话只在这一轮还没收到最终结果时接受；“立即插入”按钮绑定的是这条消息自己的队列 ID；`/steer` 保留引用的内容。
- `/stop`：控制请求的写入也有期限，CLI 不读 stdin 时不会卡住；中断和“这一轮结束、下一轮开始”之间加锁，中断只会到达当前这一轮；中断时撤掉待回答问题的按钮；中断在后台进行，不阻塞 Telegram 的消息循环。`/usage`、`/context` 也在后台执行；`/effort` 只用缓存的模型列表，没有就后台刷新。
- `list_models` 新出现的强度级别（如 ultracode）能真正传给 Claude。
- 只发控制请求的临时 Claude 进程使用和正常会话相同的环境（包括 Claude Code Router 设置）。
- 待你确认：共享聊天（`share_session_in_channel`）里，任何允许的成员都能插话、回答别人的问题或批准计划；这和现在任何成员都能批准权限请求的模型一致，本次没有改。

### P4 Codex 迁移到 app-server（约 5 天）

| ID | 任务 | 依赖 | 验收 |
|---|---|---|---|
| T4.1 | 新的 Codex 会话实现：`initialize` → `thread/start\|resume` → `turn/start`，事件映射到 core 的 `Event`；`codex exec` 作为旧版 CLI 的回退，用配置开关切换 | T0.5 | 现有 Codex 冒烟测试全部通过 |
| T4.2 | 轮内插入 `turn/steer`；中断 `turn/interrupt` | T4.1 | 同 T3.3、T3.4 |
| T4.3 | 每轮的 model / effort / approval / sandbox；按 §5.2 映射模式（auto = `auto_review`，plan = `collaborationMode`）；effort 增加 max / ultra | T4.1, T0.2 | 五种模式都能用 |
| T4.4 | 审批转发（`requestApproval`）、提问（`requestUserInput`）卡片 | T4.1 | default 模式下聊天内审批 |
| T4.5 | `thread/list`、`name/set`、`delete`；`thread/compact/start`；`account/rateLimits/read` 用于 `/usage`；登录改走 `account/login/start` | T4.1 | |

**P4 实现记录（2026-11-18）**

协议以本机 Codex 0.153 生成的 schema（`codex app-server generate-json-schema --experimental`）为准，并用真实 app-server 跑过：一轮对话、插话（同一轮内读到）、中断（轮次状态 `interrupted`）、`account/rateLimits/read`、`thread/tokenUsage/updated`。

- **T4.1**：新的 `agent/codex/appserver.go`：每个聊天会话一个 `codex app-server --stdio` 进程（按进程组结束），`initialize`（开 experimentalApi，用于 plan）→ `thread/start` 或 `thread/resume`（找不到线程时报“会话不存在”，引擎会重置）→ 每条消息一个 `turn/start`。事件映射与 exec 版一致：中途的 agent 消息（phase 为 commentary，或在工具调用之前）算思考，最后的算回复；命令、文件修改、MCP、搜索算工具调用；`turn/completed` 的 completed / interrupted / failed 分别是结果、已中断、错误。项目选项 `backend = "app-server"`（默认）或 `"exec"`；CLI 起不了 app-server 时自动退回 exec。冒烟检查加了 `codex app-server --help` 的 `--stdio`。
- **T4.2**：`/steer`、“立即插入”走 `turn/steer`（带 expectedTurnId）；`/stop` 走 `turn/interrupt`，进程和线程保留。
- **T4.3**：model、effort、模式都是 `turn/start` 的参数，热切换不需要新进程，下一轮生效。这些覆盖会留在线程上，所以每一轮都完整给出：default 回到线程开始时 Codex 报告的设置（approvalPolicy、approvalsReviewer、sandbox、模型、effort）；acceptEdits = never + workspace-write；auto = on-request + auto_review + workspace-write；bypass = never + dangerFullAccess；plan = collaborationMode plan（离开 plan 时显式发 default）。额外目录作为 workspace-write 的 writableRoots。`/effort` 的选项来自 model/list 的 supportedReasoningEfforts，max / ultra 等能传给 Codex。
- **T4.4**：`item/commandExecution/requestApproval`、`item/fileChange/requestApproval`、`item/permissions/requestApproval`（以及旧版的 execCommandApproval / applyPatchApproval）变成聊天里的权限卡片，允许 / 拒绝回 accept / decline；`item/tool/requestUserInput` 变成 P3 的问题卡片，答案按问题 id 回给 Codex。其它服务端请求回“不支持”，不会卡住这一轮。
- **T4.5**：`/usage` 用 `account/rateLimits/read`（运行中的进程或临时起一个）；`/context` 用最近一次 `thread/tokenUsage/updated`。会话列表、删除仍读 `~/.codex/sessions`（app-server 和 exec 共用同一份存储）。原生 `/compact` 和 `account/login/start` 登录留到 P5（T5.6）/ 不改（P2 的设备码登录已可用）。
- **测试**：`agent/codex/appserver_test.go` 用测试二进制模拟 app-server（批准、问题、插话、中断、失败、线程不存在、退回 exec、按轮设置）；`go test -tags live -run TestAppServerLiveTurn ./agent/codex` 跑一轮真实对话。

**P4 review（2026-11-18，Codex + Kimi Code）**：两边合并后 19 个问题（其中 1 个严重），全部修复：

- **默认模式的来源（严重）**：turn/start 的覆盖会留在线程上，resume 后线程报告的是上一次的覆盖（比如 bypass 的 dangerFullAccess），不能当作默认值。现在 default 用 `config/read` 读到的用户 Codex 配置（approval_policy、sandbox_mode、writable_roots、model、effort），读不到时用 on-request + workspace-write + medium；额外目录加到 workspace-write 的 writableRoots。新进程的第一轮总是显式给出协作模式，resume 到停在 plan 的线程也能回到 default；turn/start 失败时不改变本地记录的 plan 状态；effort 回到“自动”时显式发配置里的 effort。
- **传输**：写入超时或 turn/start 等不到回复时直接结束进程（不让晚到的请求以旧设置执行），下一条消息起新进程并 resume；所有写入（包括通知、错误回复）都有期限。
- **退回 exec**：只有 CLI 明确没有 app-server（进程在 initialize 前退出、方法不存在，包括 thread/start）才退回；超时不退回；plan 模式不退回（报错，不会变成会编辑的 default）。只有明确的“线程不存在”才重置会话。
- **审批与问题**：权限请求在卡片上完整列出（网络、每条读写路径），太长显示不下就拒绝；没有命令可看的审批（如向终端写入）拒绝；旧版审批的拒绝用 `{"denied":{"rejection":…}}`；要求保密输入的问题不在聊天里收集；同样文字的问题自动区分，答案各自回到自己的 id；Codex 自己结束等待（`serverRequest/resolved`）或进程退出时，聊天里的等待也结束（core 新增可选接口 `PendingRequestWatcher`）。
- **其它**：resume 带 `excludeTurns`（不拉整段历史）；子线程（sub-agent）的通知不进聊天；plan 项作为最终回复；晚到的 turn/start 回复不会把已结束的轮次当成进行中。

### P5 通用功能拉齐（约 4 天）

| ID | 任务 | 依赖 | 验收 |
|---|---|---|---|
| T5.1 | `/timer` 一次性定时任务（移植上游，复用 cron 投递） | — | |
| T5.2 | `/loop` 自定节奏（`<loop-control>next …</loop-control>`）；`/goal` 透传 | — | |
| T5.3 | cron：本地 API 增加 `agent_session_id`；每个任务自己的 model / effort / mode；卡片显示绑定的会话 | T0.3 | |
| T5.4 | `/cancel`；`max_turn_time_mins`；`agent_session_idle_timeout_mins`（移植上游） | — | |
| T5.5 | 队列 key 与设计文档对齐（按 agent 会话共享）或修订设计文档；更新 `2026-07-22-prompt-queue.md` 的状态 | — | |
| T5.6 | `/compact` 对四个 agent 都走原生实现 | T4.1 | |

**P5 实现记录（2026-11-18）**

- **T5.1 `/timer`**：一次性定时任务是 cron 的一种（kind = `timer`，`run_at` 指定时间），沿用 cron 的投递：忙时进队列、在自己的 agent 会话里跑、结果回到聊天。`/timer <时间> <内容>`，时间可以是延时（`30m`、`1h30m`、`2d`）、时刻（`14:30`，取下一个）或日期（`2026-11-20 09:00`、RFC 3339）；`/timer exec <时间> <命令>` 跑 shell；`/timer list`、`/timer del <id>`。运行后自动停用，在列表里显示一天再清掉；服务停机错过的定时，15 分钟内的启动后补跑，更久的记为“错过”不再补跑。定时任务不出现在 `/cron` 里，也不能被重新启用；排在忙碌聊天后面、又被 `/cancel`、`/new`、`/queue clear` 丢弃的定时，记为已结束并写明原因。
- **T5.2 自定节奏**：`/loop auto <内容>` 让 agent 每次在回复末尾用 `<loop-control>next 20m</loop-control>` 决定下一次（1 分钟到 7 天，超出就取边界；没给就按 30 分钟），也可以用 `<loop-control>pause</loop-control>` 停下；普通 loop 开着 primitive 时同样认 `next`。`/goal` 原样交给 agent，不再提示“不是 echo-client 命令”。
- **T5.3 cron**：本地 API（`/cron/add`、`cc-connect cron add`）可以建 cron、loop（`--loop 30m|auto`）、timer（`--at`），可以指定 `agent_session_id` 和这个任务自己的 model / effort / mode；mode 和 effort 按 agent 实际提供的选项检查，未知值拒绝；`/cron/edit` 改这些字段时同样检查。任务没设的项沿用绑定聊天当前的设置。卡片和列表本来就显示绑定的会话。
- **T5.4**：`/cancel` = 停止当前轮次（结束 agent 进程）+ 丢弃这个聊天排队的消息 + 开新会话，保留 `/quiet`。单轮时限沿用已有的 `turn_timeout_mins`，同时认上游的名字 `max_turn_time_mins`，项目里可以用 `max_turn_time_mins` 单独设置。`agent_session_idle_timeout_mins`（全局或按项目）：常驻 agent 进程空闲这么久就关闭，会话 ID 保留，下一条消息起新进程续上；正在跑、有待回答的审批 / 问题、或聊天忙时不动。
- **T5.5**：保留按聊天排队（cron 和用户消息在同一聊天里要串行），修订了 `2026-07-22-prompt-queue.md` 并标为已实现；共享同一 agent 会话的不同聊天由 agent 会话锁串行。
- **T5.6 compact**：core 新增会话接口 `NativeCompactor`。Codex（app-server）用 `thread/compact/start`，压缩作为一轮跑完；exec 后端没有压缩，明确拒绝而不是把 “/compact” 当文字发给模型。Claude Code 的 `/compact` 本来就是原生命令。agy 的 print 模式没有压缩，Kimi 在 `-p` 下没有验证，两者放到 P6（ACP）。
- **测试**：`core/p5_test.go`（时间解析、定时的运行 / 错过 / 清理、`/timer`、自定节奏、`/goal`、任务自己的设置、API、`/cancel`、空闲进程、原生压缩），`agent/codex` 的压缩场景，`config` 的新配置项。

**P5 review（2026-11-18，Codex + Kimi Code）**：

- **任务自己的设置（高）**：只有能按会话接收设置的 agent（Claude Code、Codex、Kimi、agy）才接受任务自己的 model / effort / mode；其它 agent（如 gemini）以前会接受 `mode = plan` 却按项目的 yolo 运行，现在直接拒绝。
- **`/stop`、`/cancel` 能停掉定时任务的轮次（高）**：cron / loop / timer 在自己的 agent 会话里跑，不在聊天的进程表里；现在按聊天登记正在跑的定时轮次，`/stop`、`/cancel` 会取消它（记为“被用户停止”，不报失败）。`/cancel` 先开新会话、清队列，再停定时轮次和聊天进程。
- **旧会话的轮次不会接管聊天（高）**：进程记住自己属于哪个本地会话，聊天换了会话（`/new`、`/switch`、`/cancel`）后不会再复用它；排在别的聊天后面等 agent 会话锁的轮次，轮到时如果聊天已经换了会话就丢弃并告知（以前会在旧会话里跑，进程还会留给新会话用）。
- **空闲关闭**：关闭期间进来的消息排队后没人启动，现在关闭结束后会启动它；被标为待重启（stale）的空闲进程也会被关闭。
- **loop 控制只看最后一行（中）**：`<loop-control>…</loop-control>` 只在回复最后一行的末尾才算数，前文引用的网页或工具输出里的标签不再能改期或暂停 loop。
- **积压的运行（中）**：同一个任务在忙碌聊天的队列里最多等一份；agent 改期或暂停时，已排队的那一份丢弃，不会紧接着再跑一次。
- **其它**：几秒后的定时不再被推迟到 30 秒后（启动时的宽限只用于已经过期的定时）；`/cancel` 保留“关闭安静模式”的选择。
- **旧会话的轮次拿不到进程**：聊天已经换了会话时，旧会话的轮次在创建进程那一步（与 `/cancel` 同一把锁下）就被拒绝并告知，不会再起进程，也不会影响新会话的进程。
- **排队中的定时跨重启**：定时在忙碌聊天里排队时记下 `queued_at`；重启后这样的定时照样运行，不按“错过”处理。正在执行时重启的定时仍按“过期 15 分钟内补跑”处理。
- **改任务的项目**：`/cron/edit project` 会用目标项目的 agent 重新检查任务自己的设置，不合适就拒绝（以前换到不提供该模式的 agent 时，模式会在启动时被悄悄丢掉）。
- **exec 后端的 `/compact`**：回复“不能压缩”，不再关闭当前进程（新错误 `ErrCompactUnsupported`）。
- **说明**：agent 选的下一次时间只在内存里，服务重启后回到 loop 的间隔；正在执行的定时如果遇到重启，不能区分“已执行到一半”和“还没开始”。cron / timer 的 `exec` shell 命令不受 `/stop`、`/cancel` 影响（只受任务超时限制）。

### P6 agy / Kimi 进阶：ACP（约 5 天，可选）

| ID | 任务 | 依赖 | 验收 |
|---|---|---|---|
| T6.1 | 移植上游 `agent/acp` 通用适配器 | T1.* | |
| T6.2 | agy、Kimi 改走 ACP（配置开关）：轮内插入、热切换 model / thinking / mode、审批转发、Kimi 的 plan 模式 | T6.1 | 两个 agent 达到与 Claude / Codex 相同的能力矩阵 |

### P7 Echo 协同（echo-client 约 3 天，Echo 仓库另计）

| ID | 任务 | 依赖 | 验收 |
|---|---|---|---|
| T7.1 | 注册 metadata 上报 capabilities、CLI 版本、登录状态、默认设置 | T0.1 | Echo 仪表盘能看到 |
| T7.2 | `assign_task.agent_options`（model / effort / mode / resume_session_id），按任务生效；`task_result` 回传 `agent_session_id` | T0.3 | 用 kimi / agy 跑通一次 Echo 任务 |
| T7.3 | 可靠性：结果补发、重连后同步、请求超时、watcher 不因发送失败退出 | — | 断网重连测试 |
| T7.4 | Echo 仓库：透传 `agent_options`、按能力调度（可选）、`AgentSessionSummary` 增加 kimi、更新文档 | T7.2 | 独立 PR |

### P8 平台拉齐（约 3 天）

| ID | 任务 | 依赖 | 验收 |
|---|---|---|---|
| T8.1 | Telegram：`FileSender`、命令菜单 `-`→`_`、i18n 描述、模型选择器显示当前模型、卡片退化效果 | — | |
| T8.2 | Feishu：挑选移植上游稳定性修复（§4） | — | |
| T8.3 | 两个平台的验收清单与回归记录 | 全部 | |

**P8 实现记录（2026-11-17，T8.1 的 Telegram 部分）**

- **命令菜单**：Telegram 只接受小写字母、数字和下划线，带 `-` 的命令（`agent-upgrade`，以及带连字符的 skill）以前直接被丢掉。现在按 `agent_upgrade` 注册，收到时再换回引擎的名字；不合法的名字仍跳过，描述超长时按 UTF-8 字符截断。
- **命令描述**：`attach`、`loop`、`tts` 补了五种语言的描述；`agent-upgrade` 的描述原本存在但键名是 `agent_upgrade`，查不到，现在查找时也会试下划线形式。
- **发文件**：Telegram 实现了 `FileSender`，`echo-client send-file` 在 Telegram 上以文档发送（保留原文件，超过 50 MB 时报错，说明超过 1024 字的另发一条）。
- **模型选择器**：当前模型不在 agent 的列表里时，所有平台都把它列在最前面（原来只有飞书）；Telegram 的模型按钮改成每行一个，避免长 id 被截断。
- **尚未做**：卡片在 Telegram 上的显示效果还要实际看过再定（目前是文本加内联按钮）；T8.2 飞书稳定性修复、T8.3 两个平台的验收记录。

**第三轮 review（2026-11-18，Codex + Kimi Code，范围 `abe2bb9c..16f5f0c6`）**：10 个问题全部修复，主要行为变化：

- 聊天遇到正在进行的升级时立即回复“正在升级”，不再在全局锁里等待（以前可能把所有聊天和 `/stop` 卡住整个升级过程）；只有 Echo 任务、cron、relay 会等升级结束。
- 升级命令在自己的进程组里运行，超时连同安装程序的子进程一起杀掉，输出最多再等 10 秒；命令成功但留下后台进程占着输出时按成功处理。
- 拿不到跨进程升级锁算作失败：会通知、计入失败退避。定时器到点后重新检查开关和时间窗，不在时间窗内就重新排期。
- 服务关闭时，还没轮到启动的平台也标为已停止，不会在关闭后再启动。
- Telegram：换算后撞名的命令（如 `deploy-prod` 和 `deploy_prod`）里，本来就合法的名字保留，换算来的那个不注册并打警告；`SendFile` 只接受普通文件（拒绝管道和设备文件），自己打开文件、按检查过的大小读取并负责关闭；超过 1024 字的说明按 4096 字（UTF-16 计数）切成多条纯文本发送，说明发送失败时文件照发并返回错误。
- 日志脱敏：`-p`、`--prompt` 后面的参数一律脱敏（包括以 `-` 开头的提示词）；`--print` 只在后面不是选项时脱敏。
- 测试：`TestJobManagerReloadMarksNonTerminalJobsOrphaned` 原来在磁盘写入前就重新加载，偶尔失败；改为等 runner 被调用后再加载。
- 不改：`send-file` 可以发送服务进程读得到的任何绝对路径，飞书和 Telegram 一致；2026-11-18 决定不限制在工作目录内。

### P10 Agent 空闲自动升级（约 3 天）

目标：每个 agent 在空闲时自动升级到最新版；**升级的就是 agent 实际运行的那个二进制**；失败可见；升级后新版本马上生效。

| ID | 任务 | 依赖 | 验收 |
|---|---|---|---|
| T10.1 | **对准目标**：用 agent 启动时的同一份环境（服务 PATH，不经过登录 shell）解析二进制的绝对路径；版本探测与升级都针对这个路径；按路径识别安装路线：Codex 区分 standalone / npm / brew，Claude 沿用现有的识别；发现 PATH 上有多份安装时告警 | — | 在本机复现“两份 codex”，升级后 agent 实际运行的版本变成最新 |
| T10.2 | **先检查再升级**：先查最新版本（npm `view`、GitHub releases、CLI 自带的检查），已经最新就跳过；升级超时或中断后检查安装是否完整（bin 链接、`--version`），不完整就修复或回滚到上一版本 | T10.1 | 已是最新时不再重装；人为中断安装后能自动修好 |
| T10.3 | **可观测**：日志打印错误和输出尾部；把每个目标的上次运行时间、前后版本、错误、连续失败次数持久化到磁盘；`/agent-upgrade status` 展示这些信息；升级成功或失败时通知管理员会话（可配置） | — | 失败原因能在聊天和日志里直接看到；重启后状态还在 |
| T10.4 | **调度**：持久化下次运行时间；支持静默时间窗配置（如 `window = "03:00-06:00"`）；空闲判定包括聊天会话、Echo 任务、cron/loop 进程，并要求最近 N 分钟没有轮次；升级期间新消息进入队列；同一台机器的多个服务用文件锁互斥 | — | 两个服务不会同时升级同一个 CLI；忙碌时推迟到下一个窗口 |
| T10.5 | **升级后生效**：重启空闲的常驻 agent 进程（Claude），下一轮 resume 原会话；刷新模型缓存与能力探测（§5.1）；跑一次冒烟检查（`--version` + 能力探测），失败就回滚并告警 | T0.1 | 升级后不用重启服务，新版本马上生效 |
| T10.6 | **新目标**：Kimi Code（npm `@moonshot-ai/kimi-code` 或 CLI 自带的更新命令，待 spike 确认）、agy（安装器或 CLI 自带的更新命令，待 spike 确认；agy 几乎每天发版，默认每天一次足够）；清理 legacy `kimi-cli` | T1.2, T1.3 | 四个主力 agent 都纳入自动升级 |
| T10.7 | **Echo 联动**：heartbeat / 注册 metadata 带上各 CLI 的版本；Echo 任务运行中不升级 | T7.1 | Echo 仪表盘能看到每台主机的 CLI 版本 |

**P10 实现记录（2026-11-17，核心部分：T10.1、T10.3、T10.4）**

- **T10.1**：每个目标先按服务自己的 PATH 定位 CLI（与 agent 进程一致），版本探测直接执行这个二进制；按实际路径识别安装路线（Codex：standalone / npm / Homebrew；Claude：native / Homebrew / npm；Gemini：npm / Homebrew；Qoder：自更新），没有手动配置命令时自动选用对应的升级命令（Codex standalone 用官方安装脚本）。手动配置的 `version_command` / `update_command` 始终优先。命令执行改为非登录 shell（`/bin/sh -c`），不再读 `~/.profile`。PATH 上有其他副本时在日志和 `/agent-upgrade status` 里告警。本机实测四个目标都定位正确。
- **T10.3**：自动运行失败时日志带上错误和输出尾部；计划、失败退避、每个目标的上次结果、通知会话都持久化到 `<data_dir>/agent-upgrade-state.json`；`/agent-upgrade status` 显示二进制路径、安装路线、副本告警、上次结果；新增 `/agent-upgrade notify on|off|list`（仅 `allowed_user_ids`），版本变化或失败时通知已登记的会话。
- **T10.4**：下次运行时间按“上次运行 + 间隔”计算并持久化，重启不再重新计时；服务停机期间错过的运行在启动约 2 分钟后补上；新增 `window = "HH:MM-HH:MM"`（可跨午夜）；空闲判断加入 Echo 任务、隔离 cron 运行和 relay 轮次；用户缓存目录下的文件锁让两个服务不会同时升级同一个 CLI，持有锁期间另一个服务的新消息也会等待。
- **尚未做**：另一个服务进程里正在运行的轮次看不到（锁只保证不会同时升级）；T10.7（T10.6 随 P1 完成，见上；T10.2、T10.5 见下）。

**P10 收尾（2026-11-18，T10.2、T10.5、升级期间排队）**

- **T10.2 先检查再升级**：升级前先查最新版本：Claude、Codex、Gemini、Kimi 用 npm registry 的 `latest`（`npm view <包> version`；这是所有渠道里最新的版本，已经不低于它就肯定是最新），Qoder 用 `qodercli update --check`，agy 没有便宜的查法，交给它自己的更新器判断。已经是最新就跳过（“already the latest version”），不再重装；查不到时照常升级。可用 `latest_command` 覆盖。
- **T10.2 / T10.5 冒烟检查与回滚**：升级前记录安装状态：解析路径上的每个符号链接及其原目标（包括 Codex 的 `standalone/current` 这种目录链接）；二进制就地替换的 CLI（Kimi、agy）在状态目录里留一个硬链接备份（不占空间；跨卷时不备份，那些 CLI 自己保留旧版本）。升级后（安装命令失败或超时也一样）检查：二进制能解析、`--version` 成功、`--help` 里仍有适配器会传的每个选项（各 agent 包用 `core.RegisterAgentCLIChecks` 登记，`echo-client doctor` 也显示这项检查）。升级前通过、升级后不通过就回滚：链接改回原目标，就地替换的从硬链接恢复，npm 安装重装旧版本，Homebrew 不支持回滚。回滚算失败：会通知、计入退避。升级前就不通过的不回滚（不是这次升级弄坏的）。可用 `smoke_check = false` 关掉选项检查。
- **中断恢复**：记录写进状态文件（`in_flight`），升级结束才删除；服务在升级中途被杀掉的话，下次启动时检查，弄坏了就按记录回滚并通知。
- **T10.5 升级后生效**：每轮开始前比较 CLI 二进制（解析后的路径、大小、修改时间）。变了（本服务或另一个服务升级、手动升级、Claude 自己的后台更新）就让所有聊天在下一轮重启 agent 进程并 resume 原会话（不打断正在进行的轮次），并清空 Kimi、agy 的模型列表缓存。
- **升级期间排队**：聊天消息不再提示“稍后重试”，而是进入该聊天的队列（回复“已排队 #N”），升级结束后自动发送；队列里原有的消息也等升级结束再发。不能排队的消息（未知的 `/` 命令）仍提示稍后重试。
- **本机验证**：六个 CLI 当前版本全部通过选项检查（`go test -tags live -run TestAgentCLIChecksPassOnInstalledCLIs ./cmd/cc-connect`）。

### P9 文档与测试（贯穿）

- 能力矩阵单测：每个 agent 声明的能力都要有实现，没有声明的命令要给出提示。
- 每个 agent 一个真实 CLI 冒烟脚本（打 build tag，不进 CI 默认集合）。
- 更新 README、`config.example.toml`、`docs/feishu.md`、`docs/telegram.md`；本文档发布后改为 `accepted`，完成后改为 `shipped`。

**总量**：P0–P5 + P7 + P8 + P10 约 28 人日；加上可选的 P6 约 33 人日。

## 7. 测试与验收

每个 agent × 平台组合都按以下清单走一遍：

1. `/auth status` → `/auth logout` → `/auth login` → 回贴 code → `status` 显示已登录
2. `/model` 列表包含最新模型 → 选择 → 继续对话，上下文仍在
3. `/effort` 只显示当前模型支持的选项 → 选择生效
4. `/mode auto`、`/mode bypassPermissions` 都能完成一个会写文件、跑命令的任务；`default` 下能在聊天里审批
5. `/new` → `/list` → `/switch` 回到旧会话 → 上下文正确
6. `/cron` 绑定当前会话 → 运行后，聊天里能接着 cron 的结果继续
7. `/loop` 固定间隔与自定节奏各跑 3 次 → 暂停
8. 长任务中插话：支持 steer 的 agent 当轮看到，其余进队列；`/queue` 可调整顺序
9. `/stop` 后会话可以继续；`/compact`、`/usage`、`/status` 输出正确
10. 作为 Echo worker 注册，执行一个带 `agent_options` 的任务

## 8. 待决问题（需要你确认）

> 2026-11-17：文档已确认，1–6、8 按建议执行；7（Echo 协议扩展的时机与 Echo 仓库由谁改）仍待定，到 P7 前再确认。

1. **策略**：选择性移植上游，不全量合并？（建议：是）
2. **设置作用域**：model / effort / mode 按聊天会话保存并持久化？（建议：是）
3. **Codex 迁移到 app-server（P4）**：这是 Codex 获得插入、热切换、审批、设备码登录的前提。（建议：做，保留 `exec` 回退）
4. **agy / Kimi**：先用每轮进程接入，ACP 作为第二期？还是直接上 ACP？（建议：先每轮进程，P6 再上 ACP）
5. **Kimi**：只支持 Kimi Code 2.x，放弃已归档的 legacy kimi-cli？（建议：是）
6. **Claude 未公开控制请求**（`list_models`、`get_usage`、`claude_authenticate`）：允许使用（带回退）？（建议：`list_models`、`get_usage` 用；登录仍走 PTY）
7. **Echo 协议扩展（P7）**：本期做，还是等 agent 拉齐之后再做？Echo 仓库的改动是否也由我来做？
8. **执行顺序**：建议 P0 → P10（T10.1–T10.5，先修好现有升级） → P1（含 T10.6） → P2 → P3 / P4 → P5 → P8 → P7 → P6。
9. ~~**本机 Codex 保留哪一份安装**~~ **已决定（2026-11-12）：保留 standalone**（`~/.local/bin/codex`，agent 实际在用），npm 那份已删除。standalone 的升级方式是官方安装脚本（`codex update` 内部调用的也是它，但本机因为外置卷符号链接识别不到安装方式）。执行情况见 T0.5；T10.1 识别 standalone 时，要把 `~/.codex/packages` 被符号链接到别处的情况也算进去。

## 9. 风险

- **CLI 变化快**：agy 几乎每天发版，Kimi Code 2.x 刚推出，Codex 每周都有版本。对策：启动时探测 `--help` 能力（沿用上游 kimi 的做法），doctor 提示版本，冒烟脚本每次升级后跑一遍。
- **自动升级可能把能用的 CLI 升坏**：新版本删掉旧参数的情况已经发生过（Codex 删除 `--full-auto`，Kimi Code 删除 `--print` 等）。对策：T10.5 升级后冒烟检查加回滚；结合启动时探测 `--help` 能力。
- **CLI 装在外置卷上**：standalone codex 的 `~/.codex/packages` 链接到 `/Volumes/extended/...`。开机时外置卷还没挂载的话，服务找不到 codex。对策：T0.5 的 doctor 检查二进制能不能解析、所在卷有没有挂载；T10.3 在探测失败时发通知。
- **未公开接口**：Claude 的部分控制请求随时可能变。对策：只作为增强，失败就回退。
- **设备码登录的前置条件**：Codex 需要在 ChatGPT 安全设置中启用设备码；Kimi `provider list --json` 会输出密钥。对策：在文档中写明；读取结果一律脱敏。
- **改动面大**：`core/engine.go` 9000 行。对策：新逻辑尽量放进新文件（capabilities、auth、steer），engine 只做接线；每个阶段单独 PR。

## 附录：资料来源

- Claude Code：code.claude.com/docs（headless、cli-reference、authentication、permission-modes、model-config、scheduled-tasks、goal、agent-sdk）；anthropics/claude-code CHANGELOG；本机 2.1.322 实测。
- Codex：github.com/openai/codex releases（0.159–0.167.2）；learn.chatgpt.com/docs（app-server、auth、models、permission-modes）；本机 0.153 生成的 app-server schema 与 `model/list` 实测。
- agy：antigravity.google/docs（cli/headless、ACP、install、models、hooks）；google-antigravity/antigravity-cli releases；本机 1.5.6 实测（只读 slash 命令）。
- Kimi Code：github.com/MoonshotAI/kimi-code（`apps/kimi-code/src/cli`）、moonshotai.github.io/kimi-code 文档；legacy MoonshotAI/kimi-cli（已归档）。
- 上游 cc-connect：`upstream/main` @ b62c735a（2026-11-11），CHANGELOG 与 `agent/kimi`、`agent/antigravity`、`core/timer.go` 等源码。
- Echo：`~/Projects/echo` 的 `src/echo_server/api/worker_gateway.py`、`core/worker_gateway.py`、`docs/designs/*echo-v2-extension.md`。

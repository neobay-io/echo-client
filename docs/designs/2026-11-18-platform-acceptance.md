# 飞书 / Telegram 验收清单与回归记录

状态：自动化部分已通过（2026-11-18，`acd6f9a9` 之后的 P8 改动）；实机部分在整体验证时与使用者一起完成。

对应 `2026-11-12-agent-platform-parity.md` 的 T8.3。每一项写明怎样验证：**自动**（测试名，`go test ./platform/... ./core/...` 覆盖）或 **实机**（在真实聊天里操作，结果记在下面的回归记录里）。

## 1. 清单

| # | 项目 | 飞书 | Telegram | 验证方式 |
|---|---|---|---|---|
| 1 | 私聊、群聊 @ 机器人收发文本 | ✅ | ✅ | 实机；群聊 @ 判断：自动 `TestGroupMessagesFailClosedWithoutBotIdentity` |
| 2 | 机器人身份拿不到时群消息不处理（fail closed），私聊照常，后台重试 | ✅ | n/a | 自动 `TestGroupMessagesFailClosedWithoutBotIdentity`、`TestIdentifyBotRetriesTransientFailures` |
| 3 | 发送遇到网络抖动重试且不重复（同一 uuid）；可能已送达时不补发纯文本；原消息被删时改发到聊天 | ✅ | — | 自动 `TestSendRetriesWithOneDedupKey`、`TestUnconfirmedSendIsMarked`、`TestNoPlainTextCopyAfterUnconfirmedButtons`、`TestReplyFallsBackToChatWhenTargetGone` |
| 4 | tenant token 被吊销后自动换新（发消息、上传、下载） | ✅ | n/a | 自动 `TestRevokedTokenIsReplaced`、`TestUploadReplacesRevokedToken`、`TestPlatformsShareTheTokenCache` |
| 5 | WebSocket 静默断线后重连（SDK v3.12.2 按 ping 间隔设读超时） | ✅ | n/a | SDK 自带测试；实机：断网 1 分钟后恢复，新消息能收到 |
| 6 | 收图片 / 文件 / 语音 / 视频；大文件分块下载（上限 100 MiB，逐块校验） | ✅ | ✅ | 自动 `TestLargeResourceDownloadsInChunks`、`TestResourceSentWholeWhenRangeIgnored`、`TestResourceErrorCodeReported`、`TestJSONResourceDownloads`、`TestBadRangesAreRejected`、`TestOversizedResourceRejected`；实机：发一个 >5 MB 的文件 |
| 7 | 连发多张图片合成一条（`image_batch_window_ms`，默认 500，0 关闭；最多 20 张、5 秒）；同一会话按到达顺序处理 | ✅ | — | 自动 `TestImageBurstBecomesOneMessage`、`TestHeldImagesGoBeforeText`、`TestImageBurstCap`、`TestStoppedPlatformDropsHeldImages` |
| 8 | 引用消息带入上下文 | ✅ | ✅ | 自动 `TestExtractQuotedMessage*` |
| 9 | 卡片 / 按钮（`/list /cron /loop /queue /help`、model / effort / mode、权限、提问、计划） | ✅ 原生卡片 | ⚠️ 文本 + 内联按钮 | 自动 `TestRenderCardMap_*`、core 的 P3 卡片测试；实机：看 Telegram 的显示效果 |
| 10 | 按钮回调保留私聊标记（`/auth` 只在私聊） | ✅ | ✅ | 自动 `TestCardActionKeepsPrivateChatFlag`、`TestCallbackKeepsPrivateChatFlag`、`TestAuthPrivateChatMetadata` |
| 11 | agent 发文件给用户（`send-file`） | ✅ | ✅ | 自动 `TestSendFileUploadsDocument`、`TestSendFileRejects*`；实机：让 agent 发一个文件 |
| 12 | 命令菜单（带 `-` 的命令换成 `_`，描述齐全） | n/a | ✅ | 自动 `TestBotCommandsRenameHyphenatedCommands`、`TestBotCommandsCollisionsDoNotShadow`、`TestRestoreCommandMapsMenuNamesBack`、`TestEveryBuiltinCommandHasADescription` |
| 13 | 模型选择器列出“不在列表中的当前模型” | ✅ | ✅ | 自动 `TestModelPickerShowsCurrentModelEverywhere` |
| 14 | 日志不含凭证 | ✅ | ✅ | 自动 `TestDirectedMessageLogsDoNotContainCredentials`；飞书 SDK 的连接日志带 `access_key`，读日志时需要过滤 |
| 15 | 长消息切分（Telegram 4096 字、说明 1024 字） | n/a | ✅ | 自动 `TestSplitTelegramText` |

## 2. 实机检查（整体验证时完成）

每个平台在一个私聊和一个群里各做一遍：

1. 发文本、@ 机器人、引用一条消息提问。
2. 连发 3 张图片，再发一句文字，确认 agent 一次看到 3 张图。
3. 发一个大于 5 MB 的文件。
4. `/model`、`/effort`、`/mode` 按钮切换；default 模式下审批一次。
5. `/queue` 卡片、`/loop list`、`/timer list`。
6. 让 agent 用 `send-file` 回一个文件。
7. 飞书：断网约 1 分钟再恢复，确认之后的消息仍能收到、回复不重复。

## 3. 回归记录

| 日期 | 版本 | 范围 | 结果 |
|---|---|---|---|
| 2026-11-17 | `16f5f0c6` | Telegram：命令菜单、发文件、模型选择器、i18n | 自动全部通过；实机：命令菜单注册成功（日志无 “not allowed” 警告） |
| 2026-11-18 | P8 提交 | 飞书：SDK v3.5.3 → v3.12.2；fail closed；发送重试与去重；token 换新；分块下载；图片合并；所有 API 调用有超时 | 自动全部通过（含 `-race`）；部署后两个服务的飞书连接正常；实机项待整体验证 |
| 2026-11-18 | 整体验证提交 | 引擎与四个 agent 按 parity 文档 §7 跑 live 验收（`e2e/parity_live_test.go`，进程内聊天平台代替飞书 / Telegram）；Claude 自己发起的回合作为单独消息发到聊天 | 引擎侧全部通过（结果见 parity 文档 §7.1）；本文 §2 的实机项仍待与使用者一起做 |
| 2026-11-20 | `dbb3e25b`（已部署） | 飞书实机，用网页版飞书操作：与 CC 的私聊；群「社媒运营」（4 个机器人） | 私聊：文本、引用、model / effort / mode 按钮（切换后上下文保留）、default 模式审批一次、`/queue`、`/loop list`、`/timer list` 通过；send-file 日志显示已发出，网页版是空白气泡，待桌面端确认。群聊：@ 4 个机器人都回复，不 @ 不处理，`/auth` 在群里被拒，引用内容带入（引用里的 @ 提及会丢）。`/auth status` 在 4 个飞书机器人里都正常（logout / login 按使用者的选择没测）。发现 3 个问题，见下一行。网页版限制：schema 2.0 卡片和 post 消息不显示，代码里的 `-` 显示成 `&#45;` |
| 2026-11-20 | 本次修复提交 | ① agent 的旧会话已被删除（Claude Code 清理旧记录）时，消息不再丢：新开会话、带最近聊天记录重发一次，并告诉用户；② 聊天历史只存用户原文，不再存带回放的整段 prompt（之前每次回放会套上一层）；③ 飞书卡片里的 `<number>` 之类被吞掉、cron 卡片的 `*` 被当成斜体：卡片 markdown 转义 `<` `>`，cron 描述只写能准确描述的形式，表达式放进代码格式。复审后另修：引擎自己换掉会话时，排队中的消息不再被当成“会话已切换”跳过；进程重启时发现会话已删，也带上最近聊天记录；已插入（steer）过消息的回合不重试 | 自动全部通过（含 `-race`、lint）；用新代码渲染的卡片发到 CC 私聊，网页版上占位符和 `*/20 * * * *` 显示正确。仍待使用者：3 张图片、>5 MB 文件、断网 1 分钟、Telegram、桌面端看 send-file 文件和卡片里的 `-` |

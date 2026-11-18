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

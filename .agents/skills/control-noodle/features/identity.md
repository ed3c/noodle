# 身分與選版

操作者能看出預設前端、選定 Host 和仍服務原任務的舊代各是哪一版。

## Sub-features

- I1：原 CLI 驗證 Host pool custody，回傳 active 與原 owners。
- I2：新外部命令沿選定前端；既有 worker 保留原版。
- I3：新前端讀取原 request，仍返回原 task owner。

## How to get to it (user POV)

`host switch --status`、`host resources`、`host status --request-file`、`version`。

## Driving it with original CLI

Preconditions: 選定 binary、Host root。原 task 查詢另需 accepted request。

- 用 Skill Doctor 命令讀回 pool 與 resources；要求 instance 一致。rolling 額外核對 selected SHA。
- 在 rolling helper 的 A/B 共存時，A 前端的 `version` 顯示 B；已有 worker 身分的 A 命令保留 A。
- 用 B 執行原 request status，核對原 task 仍在 A，request bytes 不變。
- 缺件或漂移時拒絕；不自動啟動 Host。空 legacy Host 不要求尚未產生的 task receipt。

## Gotchas

前端路徑不等於服務版本。Doctor 成功不保證 admission 可立即啟動。Legacy 尚無 task receipt 時，binary correspondence 保持未證實。

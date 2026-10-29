# 舊版首次維護

操作者將支援的舊 Host 轉為 rolling Host，保留原 requests、候選與停放原因。

## Sub-features

- L1：原控制器轉 manual，等待 worker 完成後保存最後狀態。
- L2：證實舊 Host 空閒後，僅送一次正常退出訊號。
- L3：缺原來源的任務停放；維護期間的新 admission 保留原 request 等待。
- L4：未知效果讀回同一 journal；完成後可修復公開 socket。

## How to get to it (user POV)

`host switch --binary "$NOODLE_TARGET" --initial-maintenance --request-file "$NOODLE_REQUEST"`。多個原 requests 重複提供 `--request-file`。

## Driving it with original CLI

Preconditions: macOS、原 CLI 支援的 exact legacy binary、原 requests、選定目標。正式維護需要原授權；測試使用獨立 Host。

- 隔離測試使用 Skill Helpers 的 legacy 命令。先 Doctor，保存原 resources、receipts、effective mode、pause、snapshot 及候選 bytes。
- 使用上述入口；讀取原 `host switch --status`。`draining` 時保留原 worker，不能另開 Host。
- 原 worker 自然退出後，核對 manual ACK、最後 checkpoint、migration-detach、資源全空、舊程序／group 消失與新 active Host。
- 缺 config、Git、原環境或 mode 時，檢查對應 owner 的具體缺件；不能建立假的原始值。
- 對同一維護重讀，核對原 request 與已知退出不被重播。隔離測試才可注入 socket publication 故障。

## Gotchas

這是一次性的 legacy 入口，不是一般 rolling 路徑。新 Host active 不表示停放 task 已接回。不可向未證實空閒的舊 PID 或 group 送訊號。保留完整失敗歷史。

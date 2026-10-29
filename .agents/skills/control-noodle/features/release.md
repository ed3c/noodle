# 自動釋放與 SwapManager

任務完成後，原生 worker、其已觀測 MCP 程序及配額退出；App SwapManager 使用它自己綁定的真實事件與 carrier。

## Sub-features

- A1：真實 stdio MCP 被原生 worker 呼叫；completion 後卸載、程序退出、租約和配額歸還。
- A2：manual／paused／pending review 不能被 auto cleanup；unknown release 保留配額與原讀回。
- A3：App Interrupt 凍結 binding；SubagentStop 才沿原 reclaim；Stop／SessionEnd 保留各自語義。

## How to get to it (user POV)

原 Noodle completion／resources／cleanup；Codex App 真實 Interrupt、SubagentStop 及其原 SwapManager readback。

## Driving it with auto_release_probe.py and original App carrier

Preconditions: 選定 rolling Host、真實 Codex、macOS、本機服務可用。App 路徑另需實際載入的 SwapManager transport、專用 binding、原 parent/caller/capability 及真實 native event。

- 執行 Skill Helpers 的 auto-release 命令。helper 建立隔離 repository/profile、受控 admission 與私人 MCP，模型服務只回傳測試資料。
- 核對 MCP witness 的 started+ping；`native.json` 的 completed、unload readback、unsubscribe、server_exit=0、observed_children 退出及 released；另核對 `raw.ndjson` 的 notLoaded／thread/closed。
- 核對 `terminal_cause=auto-completed`、quota 回到 baseline、原 owners 不變。成功路徑不得靠 helper detach 才得到 auto-completed。
- 沿原 Test Manager 選 pause、interrupt、lost reply、仍 loaded 與退出未知的 focused controls。fixture PASS 不能替代真實 App 事件。
- App drive 必須讀原綁定、觸發真實專用事件、讀其 transport journal 與原 owner next。unknown 不轉用另一 carrier 或重送效果。缺真實 prerequisite 時記錄 attempted route 與 blocked 項。

## Gotchas

ACK 或 worker=0 不是退出證據。observed_children 不是一生所有 MCP generations 的完整登錄。缺 stdio_closed witness 不宣稱 graceful EOF。App thread 卸載不證明 Noodle quota 歸還；Noodle 自動退出也不證明 App SwapManager 已驗收。SessionEnd idle release 需要原路徑的額外實證。

# Rolling 與共用配額

新任務使用選定版本，原任務在舊代完成當輪；最多兩代共存，第三代等待。

## Sub-features

- R1：兩個不同版本同時服務不同任務，使用同一 quota。
- R2：第三代排隊及相同版本去重；queued 項按原次序處理。
- R3：原輪次停派、退出、移交；原 request 與 thread 保留。

## How to get to it (user POV)

`host switch --binary`、`host switch --status`、原 attach/status、`host rebuild --request-file`。

## Driving it with native_rolling_probe.py

Preconditions: macOS、真實 Codex、三個獨立 SHA 的 builds。保留 A；分別以 `GOFLAGS='-ldflags=-X=main.canonicalVersion=control-noodle-b' pnpm build` 及 C 標記編譯 B、C，複製到各自 immutable fixture 路徑。

- 執行 Skill Helpers 的 rolling 命令。helper 在隔離 Host 啟動 A 的原生 worker，選 B，並在 B 完成另一個 worker。
- 在 A 尚在執行時選 C；核對 queue 去重、最多兩代、原 worker 未被中止。
- 讓當輪結束、配額歸還，觀察原 owner 自動移交，再沿原 request 讀回並 resume。核對同一 thread、worktree、原 pause 及 candidate review。
- 查看 `binary-identities.json`、`cross-version-frontend.json`、`coexist-active.json`、`summary.json` 及 cleanup 證據。

## Gotchas

不同檔名但相同 SHA 不能證明跨版本相容。queued 順序不表示被 blocked 的項目阻擋所有後續項目。低於上限的 live 執行不證明配額上限拒絕；需要原 shared-quota controls。fixture 的固定模型回應僅驗證 runtime。

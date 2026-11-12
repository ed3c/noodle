# Goal Host

這份文件描述目前 source 的 CLI 與狀態，不證明既有 Host 已部署這個版本。

## Goal 與 Host 選擇

每個 Goal 可保留原生 Host selection。CLI 以原 Goal 檔案的 canonical path 與 bytes 摘要綁定 selection，再由原 Host owner 產生 Host 身分、profile、端點與續行資料。
正常 Soodles 任務保留語義 Goal、task 與授權；原 Host／control owner 產生執行所需的綁定資料。這些 CLI 不是另一份任務規格或 admission 授權。

```sh
noodle host template --profile /absolute/installed/profile.json
noodle host ensure --goal-file /absolute/original/goal.json
```

`template` 保存已安裝 profile 及其相依來源摘要。新 Host 使用該 template；來源變更會被拒絕。
`ensure --profile /absolute/profile.json` 可明確選擇 template override。已有 selection 時，不同 override 會被拒絕。
`ensure` 初次準備隔離 profile 並啟動 Host。後續呼叫讀回原 selection；建立效果未知時返回 `unknown`，不重播啟動。

```sh
noodle host ensure --goal-file /absolute/original/goal.json \
  --new-host --request-file /absolute/original/new-host-operation.json
```

`--new-host` 與 `--request-file` 必須一起提供。CLI 保存原 operation 來源；重送相同 operation 讀回原選擇。
尚未完成的 Host 建立不接受另一個新選擇。原 profile 讀回有效時，正常 `ensure` 可在原 Host 明確 blocked，或原程序及群組已確認不存在後，為後續新工作選擇替代 Host。
此 selection 不遷移已綁定或執行中的 request。原 request、未知效果與 native execution custody 仍屬原 owner，需原讀回。

## Worker 名額與隔離

每個 Goal Host 最多有 9 個 worker。同一 Host 的 rolling generations 合計共用 9 個名額；不同 Host 各有自己的帳本。
OpenShell gateway resident 另列 allocation，不扣 worker 名額。名額用完時，原佇列留在同一 Host 等待；capacity 不構成 Host fault。
memory pressure、Swap 使用量、換出速率與採樣失敗都是觀察資料，不是 admission gate。未解決的 ownership、quota 寫入與歸還仍會阻擋 admission。

原 profile materializer 為每個 Host 建立獨立 gateway 設定、loopback 端點、mTLS／JWT／加密 key、client 設定、SQLite、VM state 與 custody 路徑。
payload 與 route spec 保留原來源。materializer 只準備本機檔案，不執行模型 credential provisioning；隔離設定不代表 gateway、VM 或模型憑證已可使用。
`status=ready` 只表示已讀回選定 Host 的 admission 身分與政策；`runtime_readiness=not_observed` 保留原生 runtime 與 credential readiness 的證據缺口。

## 沿用登入與憑證續供

新 Goal profile 的 `credential_source` 保留原登入 gateway、workspace 與 operator mTLS 來源。materializer 不複製 OAuth refresh grant。從另一個 Goal profile 建立替代 Host 時，來源仍指向原登入 gateway。
這個登入服務是明確的共用依賴。任務 gateway、SQLite、VM 與 custody 維持各 Host 隔離。

worker 使用前，原 Gateway owner 只取得該 route 所需 provider 的 runtime credential，並配送到任務 gateway 的 external `stable_placeholder` provider。它不另建 refresh owner。
原 Gateway owner 按實際 expiry 持續更新 provider。placeholder 保留原 VM 與 worker 身分；更新成功仍須原 mutation／installation readback，不能從 redacted provider 資料推定 ACK。
登入服務故障時，原 owner 保留錯誤與續供義務。需要新憑證的工作不能冒充可用；已有有效憑證的 worker 不因登入服務故障被停止。授權失效沿原登入 owner 處理。

配送前，原 custody 以 schema v5 保存來源綁定、原 request ID 與加密 payload。未知回覆保留同一筆請求；超過原服務的 24 小時去重界線後，不換新 ID 重播。
一般 receipt 與 log 不保存 token。解密依賴該任務 gateway 原 mTLS private key；該 key 與 custody 必須一起保留，不能用另一個 Host 的 key 接管。
未使用配送的既有 custody 維持 schema v4。standalone custody 與原 v3→v4 handover 不接受 v5；拒絕不會把 credential state 降版或丟棄。

這些是來源可追溯的實作行為。真正的模型登入、同 VM 跨 token 到期與 worker 返回，仍須原生驗收；本文件不代表已部署或驗收通過。

## Control checkout

```sh
NOODLE_HOST_DIR=/absolute/selected/host noodle host control \
  --task-file /absolute/original/task.json
```

原 task 提供 `repository` 與可選的 `base_ref`。`control` 由原 Git owner 建立標準 worktree 及獨立 branch，保留 source checkout 的 HEAD 與修改。
這項 admission checkout 準備不啟動 worker，也不取得 worker 名額。
首次建立使用 task 的 `base_ref`；缺少時使用 `origin/HEAD`。重送讀回原 control。
原 checkout 遺失、branch 或 ancestry 不符時拒絕，不重建一份取代原證據。
`prepared` 不代表 task admission、worker 執行或候選驗收。

## 原未接納請求

```sh
noodle host admission-readback --request-file /absolute/original/attach.json
noodle host admission-transfer --request-file /absolute/original/attach.json \
  --target-host-root /absolute/selected/target \
  --config-file /absolute/original/successor.toml
noodle host admission-transfer-readback --request-file /absolute/original/attach.json
```

`admission-readback` 讀取原 Host 的 durable pre-admission refusal。單純缺少 receipt、context 或 owner 不能證明 `not_admitted`。
`admission-transfer` 核對原 refusal、未持有 native execution custody、目標 Host、binary 及保留 task policy 的設定，保存一個 successor request。它不派發 worker，也不接受 task。
若設定已綁定 `runtime.return_handoff`，此入口會拒絕 transfer。原 descriptor 綁定 Host；此入口沒有原 task owner 的新 Host 重綁證據。
目標 OpenShell profile 必須屬於選定目標目錄，且保留原 dispatch 與 gateway policy。原 request 與失敗原因不改寫。
`admission-transfer-readback` 只讀已 sealed transfer、原 proof 與目前 successor bytes；不聯絡兩個 Host，也不建立缺少的 proof。
`input_required` 保留缺少的原證據；pending、running 或 unknown successor 仍沿原 request 讀回，不因 transfer 準備成功而重試效果。

相關政策與維護邊界見 [CLI Reference](cli.md#共享資源額度swap-manager)。

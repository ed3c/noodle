# CLI Reference

All commands accept the global `--project-dir` flag. When omitted, Noodle uses the current directory (or the `NOODLE_PROJECT_DIR` environment variable).

## Global flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--project-dir` | string | current directory | Project directory. Also settable via `NOODLE_PROJECT_DIR` |

---

## `noodle start`

Start the noodle loop. Spawns agent sessions and manages the full lifecycle.

Auto-starts a web server on port 3000 (configurable via `[server]`). Opens a browser unless `NOODLE_NO_BROWSER=1` is set.

```
noodle start [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--once` | bool | `false` | Run one scheduling cycle and exit |

---

## `noodle host`

Host 讓不同 project 共用 Noodle 服務。正常時使用一代；換版時最多兩代短暫共存。每個 project context 保留自己的設定、環境與 worktree。
Host 最多接上 16 個 context；同一 project 同時只能接上一個。Context 必須設定 `server.enabled=false`。
原 Loop 仍負責排程與工作佇列。Host 的共享資源額度決定何時可以啟動 worker。
沒有 worker 的 context 不占 worker 額度。共享 Host 不支援 Sprites 或 Cursor runtime，會在建立 runtime 前拒絕。Standalone 遠端模式不受此限制。

```sh
noodle host capabilities
noodle host resources
noodle --project-dir /path/to/project host once --request-file /path/to/run/once.json
noodle --project-dir /path/to/project host attach --request-file /path/to/run/attach.json
noodle --project-dir /path/to/project host status --request-file /path/to/run/attach.json
```

`once` 使用 manual mode 初始化，不啟動 worker。`attach` 使用設定中的 mode，或明確的 `--mode`。
原生 CLI 在送出請求前產生 request ID，並保存 project、config、binary 與 host directory 綁定。
同一請求重用同一個 `--request-file`。合法的新一輪使用不同檔案。
回應遺失時，`status` 依原檔查詢該請求，不以相同設定的歷史結果代替。

`detach` 接受原 `--request-file`，以及 receipt 的 `--context-id` 和 `--instance-id`。
Loop 確認沒有 worker、合併工作或待處理輸入後，才釋放 context。
Host 等待 process、stream 與 watcher 結束，再移除該 context。其他 context 與主進程繼續運作。
Host 不切換全域環境或工作目錄。每個 context 的原生命令使用自己的 PATH、HOME 與環境快照。

鎖定範圍是同一使用者的 cache directory 下 `noodle/host`。
新版 `start` 共用此鎖。`NOODLE_HOST_DIR` 可指定隔離的測試端點。
此鎖不能約束舊 binary 或另一個 OS 帳戶。歷史進程需要原 owner 確認結束後遷移。
Host 重啟若發現未解決的 active receipt，會回傳 `unknown_effect` 並拒絕新工作。
這不表示工作已失敗或可以重播；原 owner 必須先取得效果讀回。

### 兩代 Host 換版

`noodle host switch --binary /absolute/immutable/noodle` 保存下一版的選擇。
目標 binary 必須是不可變的普通可執行檔，並提供 `rolling_host_version=1` 與 `shared_worker_quota=1`。
`noodle host switch --status` 只讀原生版本、任務 owner 與更新佇列，不啟動 Host。
原入口仍由未綁定 rolling generation 的 Host 持有時，CLI 返回 `maintenance_required`、原生資源讀回與空 `effects`；這不是已完成換版。

首次維護使用 `noodle host switch --binary /absolute/immutable/noodle --initial-maintenance`。
對沒有外部 config 路徑的 context，重複提供原 `--request-file`。
此相容入口只接受已核對的 macOS 舊版 `2b636db20166ada9e304ac3462db52dd07c69899`。
原 Host 先保存完整 checkpoint，再進入 manual 並等待本輪 worker 完成。
前景切換程序轉送原資源與狀態讀回；新 admission 返回 busy 並保留原 request。
全部 context 合法 detach、資源歸還與原程序身分確認後，程式只送一次 SIGTERM，使用舊 Host 的正常關閉處理。
stopping 的重入只讀回，不重送訊號。未知效果仍需原讀回，不能假定沒有執行。
新 Host 啟動失敗時保留同一維護紀錄，不自動回復舊版。

啟動 override 可能與舊 canonical mode 不同。CLI 保存控制前的 effective mode，並以最後 ACK 保存 dispatch state。
缺模式、環境來源、Git 或必要檔案時，任務保留具名缺口並停放；這不等於任務恢復或 Parent Goal 完成。
legacy 沒有必要環境來源紀錄時，只有完整原 environment 摘要相符才能建立第一次 recipe。
這項證據要求不套用到已有必要來源 recipe 的正常換版。
Runtime 與 worker 目錄保存在 checkout 外；原路徑保留 alias。
原 Git owner 會修正 worker registration 的實體 backpointer，避免 App 清理 alias 後 prune 刪除原 index。
此保存範圍不代表已封存 control checkout 的所有來源與未提交修改。

換版不能中斷正在執行的 worker。因此舊版繼續服務已接納的本輪，新版接納新請求。
兩代共用一份持久配額與啟動時間窗，不能各算一份全機額度。
共用檔案鎖阻止仍使用獨立配額的舊 launcher 同時啟動。
最多兩代可以存活。後續版本依 FIFO 等待；相同排隊或 blocked 請求的重送讀回原紀錄。

舊版停止派發下一個 worker，但保留本輪的原 runtime 與必要服務準備。
新版不在舊 worker 完成前搶先啟動 resident。同名 resident 也必須等待原服務真正退出，不能建立第二個 owner。
原 Loop 在沒有活動 worker、合併、完成事件或待處理輸入時，保存有效 mode 與 dispatch state，再沿原 Release 收尾。
舊版透過私有 Unix socket 傳遞下一輪所需的環境，不把憑證寫入版本紀錄。
新版沿同一 request、project、設定與原工作資料接續合法下一步。
paused、draining 與 manual 保持原狀；已完成任務保留 terminal，不重新派發。
控制訊息仍依原任務紀錄路由至實際持有本輪的 runtime。

固定入口的外部 CLI 會選用原生 pool 中的 active binary。已執行 worker 內的 CLI 保留原版本。
原 request 檔不改寫。Soodles 的既有任務 reader 讀取原生 owner；新 admission 才綁定目前選定的 binary。
封包 ACK 只證明回應到達。舊版還須確認新版保存了相同原請求的 native context，才解除自己的續行責任。
回覆遺失時先讀取原 receipt，不另派一份。
配額寫入失敗時，Host 保留原租約身分並暫緩新派發。原 Host 的採樣事件會重讀並重試同一筆歸還；不重派 worker。
只有未交付的 reservation 或已確認結束的 worker 可以進入這條歸還路徑。未確認歸還前不報 released。
舊版在 context、待確認續行、自己的配額與 resident 都已收尾後自行退出。
新版核對原程序及群組退出後，才將該代標為 `exited` 並啟動佇列中的後續版本。

`active` 表示入口已選到該版本，不表示所有舊 worker 已結束、配額已歸還或 Codex thread 已恢復。
原生身分、效果或配額未知時保留原紀錄，拒絕重派，不用 PID kill 補成成功。
首次導入前，legacy Host 必須沿原機制完成一次維護窗；目前 rolling 命令不會強行取代仍持有獨立鎖與環境的 legacy Host。
隔離驗收已使用實際 Codex app-server 與本機離線 provider，觀察活動 worker 在舊版完成、配額歸還、舊代退出，以及同一 thread／worktree 在新版接續。
同一驗收也覆蓋暫停本輪收尾、第三版排隊及控制重送去重。這些結果不證明正式共用 Host 已切換。
首次導入仍須完成原 legacy Host 的安全維護窗。

### 已確認 round 的 context 重建

`noodle host rebuild --request-file /original/host-request.json` 沿原 request 讀取任務與選定 Host。
舊 runtime 仍存在時，CLI 先向實際持有者要求原 round 邊界。活動 worker、待處理輸入及未知效果會阻擋釋放。
只有原 Host 已確認 migration detach 並保存 continuation，選定 Host 才建立新的 runtime handle、context 及配額歸屬。
重送讀取原 receipt，不增加 attempt。原 request、授權、task、native thread、worktree、歷史及成果保留。

Host 在 admission 時保存必要來源。紀錄包含任務 HOME、native home、工具路徑、設定摘要及具名環境要求。
`runtime.process.rebuild_environment` 指定必須保持值相同的環境名稱；紀錄只保存摘要。
`runtime.process.credential_environment` 指定須重新取得的憑證環境名稱；紀錄不保存憑證值，也不要求輪替後的值相同。
原 native home 的認證檔保留來源角色。重建核對必要檔案及設定，不複製認證內容。
未宣告的環境差異不要求整份 EnvironmentSHA 相同；原 admission 的 transport 摘要仍不改寫。

正常換版從存活 producer 的私有 socket 接續。producer 已確認退出而 round 已保存時，新 Host 也會沿既有換版 tick 嘗試重建。
缺少必要環境、設定、snapshot、native attempt 紀錄或 local-task worktree 時，只保留該任務的具體缺口。
這條自動路徑不猜憑證，也不重試未變的缺件。補齊來源後，可沿同一原 request 呼叫 rebuild。
pause/manual 由原 continuation 保留。新 context 及新租約不能代替 worker 執行或 Parent Goal 驗收。
未支援 round producer 的 legacy Host 仍需原安全維護入口；本命令不能繞過它。

### 原生 session 退出讀回

`noodle --project-dir /original/project host task session-readback --session-id ORIGINAL_SESSION` 只讀既有 attempt，不啟動 Host 或模型。
CLI 核對原生 terminal、unsubscribe ACK、空 loaded-list、foreground 退出碼與原程序啟動身分。
尚在等待卸載、效果未知或原程序仍存活時，命令拒絕回報已釋放。
`process_and_group_absent=true` 只適用於這個已綁定 attempt 的觀察範圍，不能代替所有 Desktop 程序盤點。

原生 local task 完成後仍須候選審查。保存、request-changes、角色接續與失敗恢復使用原 `native.json`，不補造 `process.json`。
已綁定任務的失敗 order 在 Host 重載後保留；原 requeue 續用相同 order、attempt 歷史與 worktree。其他未綁定任務沿用原失敗整理政策。

### 共享資源額度（Swap Manager）

有舊維護紀錄時，先用 `noodle host switch --maintenance-plan --binary /absolute/candidate/noodle` 唯讀列出原 Host 身分、各 context 的原 request／設定／快照缺口，以及帳本移轉的前置條件。
它不建立或修改 Host 目錄、送控制、啟動 daemon、改寫 quota 或重跑舊換版請求；`--request-file` 只讀既有的 accepted envelope。
計畫摘要綁定原 owner 的核心世代、原 accepted request、設定、控制邊界、舊帳本與候選 bytes；`executable=true` 表示前置條件可核對，不授權執行，也不證明移交完成。
確認後用 `noodle host maintenance --plan /absolute/plan.json --confirm-plan-sha256 EXACT_DIGEST` 接續同一份計畫；來源漂移即拒絕。
`noodle host maintenance --plan /absolute/plan.json --status` 只讀原維護紀錄及原／新 Host，不能恢復派工。
原 single-use bootstrap 不接受另一組來源或目標；受限入口另建 epoch，保存舊 bootstrap／pool／quota 原 bytes，不重播舊換版請求。
舊 owner 與新移交的未完成工作標記 `maintenance_hold`，拒絕顯式及自動 round rebuild；待執行的舊換版項停放，保留原狀態。這輪沒有解除 hold 或恢復業務的入口。

缺快照但原 accepted request、原設定副本及活躍 legacy owner 都核對成功時，計畫明列原生 checkpoint recovery。
它先沿原 mailbox 送一次 pause，核對 ACK／paused 狀態，再送一次 mode=manual，要求原 actor 保存完整 canonical state 與 effect ledger。
原設定的 admitted order 必須仍在原生快照內，否則保留 hold；不從 legacy 平面檔案拼造快照，也不還原設定到活躍 checkout。
App 清理後已失去的 linked runtime lock 由前景維護程序暫持現有路徑的鎖，持久核對 inode；未知建立效果或其他持有者都拒絕續行。

context 排空並確認原生 detach、原 Host 及群組退出、全部舊 quota writer 退休後，才在 quota 原鎖內核對未變的空 schema-1 帳本，備份原 bytes 並移轉 schema 2。
容量、全機額度與 last-start window 保留。已記錄但結果不明的 quota／pool 寫入只讀回，不再次改寫。
候選沿原 Host 啟動入口啟動一次，先讀回政策、15 GB、零 context／allocation 與原工作停放，再發布入口；沒有維護 daemon。
取消或失敗不會自動 resume，也不在 schema 2 上重開舊 Host；原 owner 已退出後，回復須另核對新 owner／租約並沿原維護入口處理。

原 bootstrap 的 hold 現在先送一次原生 `pause`，核對 ACK 與 paused 狀態，再改為 manual 並核對完整 checkpoint。
manual 本身不代表暫停。缺原狀態或 checkpoint 時，在送 pause 前拒絕；舊紀錄沒有已核實的 pause 邊界時只讀回。
detach 在呼叫前持久保存原意圖。後續呼叫只查同一 context 的 status；busy、回覆遺失或未知效果都不重送 detach。
維護期間對外只轉送 resources／status；resident 啟停、釋放及未識別操作均拒絕。

單一 Host 不代表單一 worker，也不代表可以無上限啟動 worker。
每個 worker 及其準備工作都可能使用記憶體與磁碟。因此 Host 在建立 worktree、安裝依賴或啟動 worker 前，先取得共享額度。
每個 Loop 原有的並行設定仍有效；共享額度是額外的全 Host 限制；rolling 兩代共用同一份上限。

macOS Host 每 5 秒讀取系統 memory pressure、`vm.swapusage` 已使用容量與累計 Swapouts bytes。每次採樣最多等待 1 秒。
Swapout 速率是相鄰有效樣本的差值除以時間，不是目前 Swap 使用量；一般 normal／warning 派發只觀察速率，不以正流量阻擋；明確 warning grant 仍要求零換出速率。
Host 在系統 Swap 已使用容量達到 15 GB（≥15,000,000,000 bytes）時停止接納新 worker。這不是每個 worker 的配額，也不是 OS 的硬上限。
初次採樣、計數器重設、超過 15 秒沒有有效樣本、採樣失敗、critical／未知壓力，或容量達到上限，都會停止接納新 worker。
連續三個 normal／warning、容量低於上限的有效樣本後，額度逐次恢復。每個正常採樣最多提供一次啟動機會，不累積突發啟動額度。
Host 啟動時可用 `NOODLE_HOST_SWAP_USED_LIMIT_BYTES` 覆寫容量上限。數值須為 0 至 9007199254740991 的整數 bytes；格式錯誤時 Host 拒絕啟動。
設定格式的範圍不是已驗證的安全範圍。15 GB 是選定的派發政策，不保證 worker 啟動後容量不超標。
warning 不單獨阻擋一般派發；critical、未知讀值與硬體並行上限仍有效。既有明確任務 warning grant 保留原綁定、單次及期限規則，不能繞過已達容量上限。換版不自動建立 grant。
執行中的 Host 不因另一個呼叫者更改環境而更新政策。兩代必須使用相同容量與並行政策。新准入政策使用 quota schema 2；舊 schema 1 與不同容量的帳本拒絕加入，且保留原檔。現有換版入口不自動移轉或改寫帳本政策，須先沿原 owner 完成維護與租約核對。
warning grant 同樣在達到容量上限時拒絕。其共享 allocation 在真正退出前阻止另一代追加 worker。
`host resources` 的 `worker_admission_policy` 應為 `swap-used-critical-guard-v2`；`swap_used_limit_bytes` 與 `swap_used_bytes` 分別讀回上限及實際容量。缺少有效容量樣本時後者為 `null`。
候選程式或測試通過不代表目前 Host 已載入。未啟用的換出速率設定已移除，不把容量轉成 bytes/second。

macOS Host 每 5 秒讀取系統 memory pressure、目前 Swap 已用 bytes 與累計 Swapouts。每次採樣最多等待 1 秒。
預設容量門檻為 **15,000,000,000 bytes（15 GB）**，已用量 **大於或等於** 門檻就停止接納新 worker。
warning pressure 與正的 Swapout 速率本身不會提早停派；critical、未知壓力、採樣失敗或超過 15 秒沒有有效樣本仍會停派。
初次採樣及計數器重設先建立基線；連續三個有效、壓力為 normal／warning 且容量低於門檻的樣本後，額度逐次恢復。
每個有效採樣最多提供一次啟動機會，不累積突發啟動額度。
`NOODLE_HOST_SWAP_USED_LIMIT_BYTES` 可在啟動 owner 前指定整數 bytes；同帳本的所有世代必須使用相同門檻。

等待不計為 worker 失敗，也不增加重試次數。已啟動的 worker 繼續執行；完成處理與清理不等待新額度。
等待期間仍可處理 pause、drain、kill、stop 與 stop-all。原控制檔內容不變時，Loop 不重寫它，避免觸發新的檔案事件。

硬體上限為 `min(CPU 數量, max(1, floor((RAM − 4 GiB) / 3 GiB)))`。
16 GiB、10 CPU 的機器最多同時持有 4 個 worker 額度。這是保守的初始政策，並非實測最佳吞吐量或每個 worker 的記憶體配額。
非 macOS 平台會回報 `unsupported_monitor=true` 並停止新 admission；缺少 Swap 讀值時不假設容量安全。

不支援 Swap 監控的平台會回報 `unsupported_monitor=true`，有效額度為 0；取得可驗證讀值前不接納新 worker。

`noodle host resources` 不啟動 Host。它讀回目前 Host PID、context 數量、有效額度、使用中的額度、壓力與 Swapout 速率。
不能計算速率時，`swapout_bytes_per_second` 為 `null`。
額度涵蓋已接納的準備工作與尚未確認結束的 worker，所以 `active_workers` 不等於瞬間 OS 進程數量。
替換不可即時 steer 的 worker 時，Host 先確認資源條件，再停止並等待舊 worker 結束，沿用同一額度。
本機連線結束不能證明遠端 worker 已退出。共享 Host 的 OpenShell 路徑會保留額度，直到原 VM 的停止回覆與 fresh same-ID Stopped 觀察都成立；Sprites 與 Cursor 仍在啟動前拒絕。

這個機制控制啟動速度與同時執行數量。它不限制 worker 自行建立的子進程、單次寫入大小或 RSS，也不能保證 Swap 不增加。
它不修改 Spotlight、Time Machine、Gatekeeper 或系統 Swap 設定。Poteto playbook 與原有 Issue admission 保持原本的責任。

### 受限 Host 維護交接

先以 `noodle host switch --maintenance-plan --binary /absolute/immutable/noodle --request-file /absolute/original-request.json` 讀回計畫。
多個原請求逐一加上 `--request-file`。計畫核對原 accepted request、設定來源、程序世代、快照、歷史紀錄與空帳本；缺證據會列為 blocker。
只有確認這份來源與目標後，才使用 `noodle host maintenance --plan /absolute/plan.json --confirm-plan-sha256 <digest>`。
`host switch --status` 讀取既有世代紀錄；`host maintenance --plan /absolute/plan.json --status` 只讀回原交接狀態，不推進操作。

狀態機先封住新 admission，再由原 owner 各送一次 pause、原生 checkpoint 與 detach。
原程序及其群組確認退出、原 runtime 鎖釋放後，才移轉空的 schema-1 帳本至 schema 2，並啟動一次新世代。
移轉保留原 bytes、容量門檻、啟動窗口及未知欄位；非空帳本、政策不一致或未知寫入一律拒絕。
每項效果先保存 intent；回覆遺失或未知時只讀回，不能藉重跑命令重送控制、重置帳本或啟動另一代。

此相容入口只支援已驗明的原版 `2b636db20166ada9e304ac3462db52dd07c69899`；原 binary 必須有乾淨的 build revision 且符合計畫 digest。
其他來源需提供原生維護契約，不能套用這個例外。未完成交接時 admission 保持關閉。
原證據與 binary 可供讀回，但 schema 2 不能直接交給 schema-1 owner；回復不得覆寫帳本或清空未知租約。

歷史 owner、原 request／binding、設定檔、原執行模式與派工狀態保留，所有原工作加上 maintenance hold。
本入口不消費歷史換版佇列、不重建 context，也不恢復業務派工。一般 rolling 升版與停放工作的恢復需另有明確契約。
新世代透過共享帳本鎖計入 worker 與 resident 額度；Host 結束本身不會清掉未證實退出的租約。
這項交接不包含 iOS、REPL、原生 Codex 派工、聊天搬移或封存。

### 原 Host 的 OpenShell 完成證據

```sh
noodle --project-dir /path/to/control host task session-readback \
  --request-file /path/to/original-attach.json \
  --order-id EXACT_ORDER --session-id EXACT_SESSION
```

必須使用原 attach 的 binary、request 與仍持有該 context 的 Host。此命令不啟動缺失的 Host、不接管舊 session，也不重新派工。它核對 canonical attempt、執行結果、成果回寫、乾淨候選 HEAD，以及原 gateway 的 fresh same-ID Stopped 證據。

`status=observed` 表示這些完成證據一致；canonical 審查狀態另行回報，並不因此接受或合併候選。`effects=[]` 只描述此次讀回沒有任務操作；執行中的 Host 仍可獨立更新狀態，來源在讀回期間變更會被拒絕。候選檢查不執行 Git hooks／filters，不更新 index；正常 Publish 仍執行 hooks。

### 原 Host 的已保存進度與明確新嘗試

```sh
noodle --project-dir /path/to/control host task interruption-inspect \
  --request-file /path/to/original-attach.json \
  --order-id EXACT_ORDER --session-id STOPPED_SESSION

noodle --project-dir /path/to/control host task new-attempt \
  --request-file /path/to/original-attach.json \
  --order-id EXACT_ORDER --session-id STOPPED_SESSION \
  --expected-version ORIGINAL_INSPECTION_VERSION
```

這兩個入口只接受仍持有原 context 的 Host、原 binary／attach request／config，以及最新 failed 或 cancelled attempt 的已保存進度。`interruption-inspect` 的外層 `status=observed` 與內層 `inspection.status=retryable` 表示原 Loop 已核對原生停止與進度證據；`inspection.version` 綁定此次明確授權的來源。原生 session resume 仍不支援。

`new-attempt` 將該版本送入原 Loop control queue。`status=queued` 僅表示排隊，並不表示 worker 已開始、候選已接受或資源條件已允許派工；後續仍由原 Loop 重新驗證版本並經既有 admission。ACK 遺失、可能寫入後失敗或來源漂移會回報 `unknown` 並保留已有 command／effects，不可自行重送。缺失、變更或已關閉的 owner 不會被啟動或接替；舊 retained VM 也不能藉此遷移到新 Host。`host task review-capabilities` 的 `openshell_saved_progress_attempt=1` 表示此入口存在。

### 精確的 retained 初始化誤判恢復

```sh
noodle openshell recover-retained-init \
  --profile /absolute/path/to/original-profile.json \
  --expected-owner-sha256 REVIEWED_OWNER_SHA256
```

預設只檢查。僅在原 owner 尚無 worker 紀錄、完整 pending retained 清單與設定相同，且阻擋原因精確為 `retained initialization supplied for existing custody` 時，才回報可恢復。明確加入 `--apply` 會先保存原資料與前後摘要，再清除這一項本機誤判。此操作不呼叫原生 gateway、不刪除歷史、不啟動 VM；其他原因與未完成的恢復紀錄均拒絕。下一次正常取得 gateway 時仍需重新核對原生 VM。

---

## `noodle status`

Show compact runtime status. Prints active agent count, orders queue depth, and noodle loop state (running, paused, draining, or idle).

```
noodle status
```

---

## `noodle skills`

List resolved skills.

```
noodle skills
```

### `noodle skills list`

List all resolved skills.

```
noodle skills list
```

---

## `noodle schema`

Print generated schema docs for Noodle runtime contracts. Takes an optional target argument.

```
noodle schema [target]
```

### `noodle schema list`

List available schema targets.

```
noodle schema list
```

---

## `noodle worktree`

Manage linked git worktrees. Noodle uses worktrees to isolate concurrent agent sessions so they don't conflict on the working tree.

### `noodle worktree create`

Create a new linked worktree.

```
noodle worktree create <name> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--from` | string | `HEAD` | Branch or commit to base the new worktree on |

### `noodle worktree exec`

Run a command inside a worktree. Sets the working directory to the worktree path before executing.

```
noodle worktree exec <name> <command...>
```

### `noodle worktree merge`

Merge a worktree branch into a target branch.

```
noodle worktree merge <name> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--into` | string | integration branch | Target branch to merge into |

### `noodle worktree cleanup`

Remove a worktree without merging.

```
noodle worktree cleanup <name> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--force` | bool | `false` | Remove even when unmerged commits exist |

### `noodle worktree list`

List all worktrees with merge status.

```
noodle worktree list
```

### `noodle worktree prune`

Remove merged and patch-equivalent worktrees.

```
noodle worktree prune
```

### `noodle worktree hook`

Run worktree session hook. Used internally by agent sessions.

```
noodle worktree hook
```

---

## `noodle event`

Manage noodle loop events.

### `noodle event emit`

Emit an external event into the noodle loop or a specific session.

```
noodle event emit <type> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--payload` | string | — | Event payload as JSON |
| `--session` | string | — | Session ID. When set, writes to the session event log instead of the noodle loop event log |

---

## `noodle reset`

Clear all runtime state. Removes and recreates the runtime directory.

Refuses to run if Noodle is currently running (checks the lock file).

```
noodle reset
```


### Native fixture consumers

`noodle host capabilities` reports `native_consumer_leases=1`,
`native_consumer_worktrees=1` and `shared_service_adapter=stateless_fixtures_only`.
The original bound Host request is required for acquire, release and recovery.

```sh
noodle --project-dir PROJECT host resources residents inspect --request-file REQUEST
noodle --project-dir PROJECT host resources residents acquire --request-file REQUEST --native-consumer --spec-file SPEC
noodle --project-dir PROJECT host resources residents release --request-file REQUEST --native-consumer --id RESIDENT --consumer-lease LEASE
noodle --project-dir PROJECT host resources residents recover --request-file REQUEST --manifest-file MANIFEST
```

Native acquire/release must originate in the verified MCP carrier. Ordinary CLI
calls cannot invent a consumer identity. Inspect is read-only. Recover registers
only the selected historical fixture manifest; it does not adopt arbitrary PIDs.
An unknown effect returns to the same original inspect path, without resending
acquire, release or a recovery signal. This interface does not merge private
stdio workers or certify service I/O isolation.

### Review-only recovery and selected execution profile

`noodle host rebuild --request-file /original/host-request.json` can restore an
existing pending-review controller when original worker inputs are missing.
The original owner records `controller_only=true`. The loop remains manual and
rejects dispatch and non-manual mode controls. It preserves the missing-input
reason and does not capture controller environment as original worker inputs.

`noodle host rebuild --request-file /original/host-request.json --adopt-current-profile`
selects the caller's explicit current profile after checking terminal attempts,
original thread metadata and worktree history. It records
`execution_profile.kind=selected_current_profile`; the original request and
admission environment digest stay unchanged. The successor remains manual.
For `codex-app-server`, resume retains its original `NativeHome`; another home
is refused. This selection does not claim restoration of the old environment.

`noodle host task candidate-readback --request-file /original/host-request.json --order-id ORIGINAL_ORDER`
reads original worktree custody and Git-visible literal scope files. It validates
logical-to-physical storage, common/admin ownership and the Git backpointer.
It neither reviews a candidate nor dispatches a worker. Internal runtime config
references use the bound storage location. A missing alias target refuses;
external config sources still reject symbolic links.

A typed pre-context refusal can continue after its source is repaired. Original,
previous and current detached receipts remain history. New acceptance evidence
is reconciled through the original owner; unknown or contradictory effects do
not reset admission or authorize another attempt.

The project-local `.agents/skills/control-noodle/SKILL.md` provides the operator
and verification entry, with separate legacy, rolling, rebuild and release maps.

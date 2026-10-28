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

Host 讓不同 project 同時共用一個 Noodle 主進程。每個 project context 保留自己的設定、環境與 worktree。
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

### 共享資源額度（Swap Manager）

單一 Host 不代表單一 worker，也不代表可以無上限啟動 worker。
每個 worker 及其準備工作都可能使用記憶體與磁碟。因此 Host 在建立 worktree、安裝依賴或啟動 worker 前，先取得共享額度。
每個 Loop 原有的並行設定仍有效；共享額度是額外的全 Host 限制。

macOS Host 每 5 秒讀取系統 memory pressure 與累計 Swapouts bytes。每次採樣最多等待 1 秒。
Swapout 速率是相鄰有效樣本的差值除以時間，不是目前 Swap 使用量。
初次採樣、計數器重設、超過 15 秒沒有有效樣本、採樣失敗、warning／critical pressure，或正的 Swapout 速率，都會停止接納新 worker。
連續三個正常、零 Swapout 速率的樣本後，額度逐次恢復。每個正常採樣最多提供一次啟動機會，不累積突發啟動額度。
等待不計為 worker 失敗，也不增加重試次數。已啟動的 worker 繼續執行；完成處理與清理不等待新額度。
等待期間仍可處理 pause、drain、kill、stop 與 stop-all。原控制檔內容不變時，Loop 不重寫它，避免觸發新的檔案事件。

硬體上限為 `min(CPU 數量, max(1, floor((RAM − 4 GiB) / 3 GiB)))`。
16 GiB、10 CPU 的機器最多同時持有 4 個 worker 額度。這是保守的初始政策，並非實測最佳吞吐量或每個 worker 的記憶體配額。
非 macOS 平台會回報 `unsupported_monitor=true`，使用 1 個 worker 的固定上限；不宣稱已有系統壓力監控。

`noodle host resources` 不啟動 Host。它讀回目前 Host PID、context 數量、有效額度、使用中的額度、壓力與 Swapout 速率。
不能計算速率時，`swapout_bytes_per_second` 為 `null`。
額度涵蓋已接納的準備工作與尚未確認結束的 worker，所以 `active_workers` 不等於瞬間 OS 進程數量。
替換不可即時 steer 的 worker 時，Host 先確認資源條件，再停止並等待舊 worker 結束，沿用同一額度。
本機連線結束不能證明遠端 worker 已退出。共享 Host 的 OpenShell 路徑會保留額度，直到原 VM 的停止回覆與 fresh same-ID Stopped 觀察都成立；Sprites 與 Cursor 仍在啟動前拒絕。

這個機制控制啟動速度與同時執行數量。它不限制 worker 自行建立的子進程、單次寫入大小或 RSS，也不能保證 Swap 不增加。
它不修改 Spotlight、Time Machine、Gatekeeper 或系統 Swap 設定。Poteto playbook 與原有 Issue admission 保持原本的責任。

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

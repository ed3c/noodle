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

`noodle host resources` 不啟動 Host。它讀回目前 Host PID、context 數量、有效額度、使用中的額度、壓力與 Swapout 速率。
不能計算速率時，`swapout_bytes_per_second` 為 `null`。
額度涵蓋已接納的準備工作與尚未確認結束的 worker，所以 `active_workers` 不等於瞬間 OS 進程數量。
替換不可即時 steer 的 worker 時，Host 先確認資源條件，再停止並等待舊 worker 結束，沿用同一額度。
本機連線結束不能證明遠端 worker 已退出。因此這版共享 Host 在啟動前拒絕遠端 runtime，避免把未知退出誤認成可用額度。

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

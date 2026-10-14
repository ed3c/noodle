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

Host 讓不同 project 依序共用一個 Noodle 主進程。每次只接上一個 project context。
原 Loop 仍負責排程與 worktree。另一個 project 收到 `busy` 後，由原呼叫端稍後續行。
Host 不另外建立等待佇列。Context 必須設定 `server.enabled=false`。

```sh
noodle host capabilities
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
Host 等待 process、stream 與 watcher 結束，再還原環境與工作目錄。主進程繼續存在。

鎖定範圍是同一使用者的 cache directory 下 `noodle/host`。
新版 `start` 共用此鎖。`NOODLE_HOST_DIR` 可指定隔離的測試端點。
此鎖不能約束舊 binary 或另一個 OS 帳戶。歷史進程需要原 owner 確認結束後遷移。
Host 重啟若發現未解決的 active receipt，會回傳 `unknown_effect` 並拒絕新工作。
這不表示工作已失敗或可以重播；原 owner 必須先取得效果讀回。

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

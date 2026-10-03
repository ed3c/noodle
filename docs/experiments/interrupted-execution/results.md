# 中斷執行的保存與接續

這是 Noodle Issue #101 的設計與本機觀察。它不授予 provider 或 landing 權限。
本次 source base 是 `4e8da58e343e4de43245acd49dea30b394bcddcb`。
原 Soodles #229 的 live state 未變更，writer 未重新啟動。

## 問題與選擇

原 writer 結束時沒有 terminal outcome。Canonical attempt 仍是 running。
既有 startup recovery 會把 stale attempt 取消，然後容許新派發。
`spawnCook` 對後續 attempt 執行 reset 和 clean。
因此，直接 restart 會刪除未提交的 tracked 和 untracked 成果。

需要保留的是原 order、admission、worktree、成果與失敗紀錄。
需求沒有規定同一 provider thread。選擇同 order 的新 attempt。
這樣可以沿用現有 dispatcher、事件處理和 completion owner。
同 Codex thread resume 還需要程序世代和 log 分段，本次不採用。

## 已實作的邊界

`interruption inspect` 讀取原 snapshot、session、process group 和 candidate。
Subject 必須匹配 stage prompt 的 repository、Issue 和 envelope digest。
Candidate manifest 記錄 Git index entries，以及 tracked 和未忽略的 untracked 檔案。
每個檔案記錄 mode、內容 SHA-256 或缺失狀態。Symlink 記錄連結文字。
Ignored 檔案不在此 manifest 的保證範圍。

`interruption prepare` 在 native lock 下保存 before、after 和 custody。
它先持久化 intent，再更新 canonical checkpoint 和既有 projection。
原 attempt 的 owner 狀態變為 cancelled。Exit code 保持 unknown。
原 session events、raw output、process receipt 和 candidate bytes 不變。
這個操作不宣告 writer completed，也不派發模型。

Pending stage 保留一次性的 interruption binding。
原 Noodle edit-item owner可在 manual hold 時更新 prompt。
新 prompt 必須仍指向同 subject。Carrier、原 attempts 和 candidate 保持固定。
現有 loop 在派發前重驗 custody，並保存 dispatch-offered receipt。
只有這次 attempt 會跳過 reset 和 clean。
成功派發後，owner 保存新的 attempt 和 session 身分。
缺少 dispatch-result 時，readback 明確回報 unknown，不重新派發。
Startup 不能把這個 offered attempt 當普通 stale stage 重設。

Prepared 和 dispatched readback 可以在 loop 持鎖時讀取。
Readback 比對前後 snapshot bytes，拒絕讀取中的狀態變動。
`successor` 固定新 attempt 和 session。
`candidate_unchanged` 是本次 manifest 觀察，不是永久保證。
模型啟動前的 consumer 可要求它為 true。
模型工作後的合法 candidate 變更不會取消原 successor 身分。
原 session 證據若變動，readback 仍拒絕。

## 執行紀錄

第一個 CLI 測試先因缺少 UI embed 產物而無法編譯。
本機使用原 source 已建置的 ignored `ui/dist/client` 產物。
沒有修改 UI source 或使用假 embed 取代產品。

CLI 原始失敗：

```text
--- FAIL: TestInterruptionCommandsExposeStoppedOwner (0.00s)
    cmd_interruption_test.go:14: missing stopped interruption inspect: command=noodle error=unknown command "interruption" for "noodle"
```

首輪 owner fixture 漏建 foreign session 目錄，已修正 fixture。
這不是產品拒絕失敗。其餘最初的 owner 控制通過。

局部命令使用 canonical TMPDIR，避免 macOS `/var` 路徑別名：

```sh
TASK_TMPDIR=$(python3 -c 'import os,tempfile; print(os.path.realpath(tempfile.gettempdir()))')
TMPDIR="$TASK_TMPDIR" go test -race ./loop . -run '^TestInterruption' -count=1
```

已觀察一次結果：

```text
ok  github.com/poteto/noodle/loop 20.312s
ok  github.com/poteto/noodle 1.957s
```

控制覆蓋 dirty bytes 保留、old attempt、new attempt lineage、held prompt 更新、live lock、live process、foreign session、terminal 拒絕、changed bytes、unknown ledger 和兩個 prepare 故障點。
它還覆蓋 lost launch result、啟動前缺少 result 的讀回，以及不重播派發。

Fixture 使用 disposable Git repositories。
大部分控制使用 mock runtime。
另有真實程序控制通過，單次執行為 3.327 秒。
它走 manual hold、reconcile、Cycle planner 和解除 hold 的既有路徑。
ProcessDispatcher 啟動固定 shell child，child 讀取並核對 dirty 檔案。
這個 child 不呼叫 Codex 或任何模型。
它證明 Noodle 控制與檔案保存，不證明真實 Codex writer 已恢復。
獨立只讀審查找到 stage_yield 漏判、ticket outcome 誤判及原 prompt 身分未核對。
三項均已修正並加入正向或拒絕控制。
inspect 不取得會改寫 PID 的 lock，prepare 才取得 native lock。
Fixture 核對 inspect 保持原 lock bytes。

未執行 live GitHub、模型或 Soodles publication。
完整恢復仍需要 Soodles 選定 immutable owner、更新原 prompt、啟動原 loop、消費 successor，再完成原任務。

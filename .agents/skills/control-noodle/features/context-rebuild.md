# Context 交接重建

操作者在 context 消失後沿原 request 接回控制器，保留候選與 worker 歷史，並明確選定可用的執行 profile。

## Sub-features

- C1：保存 task 必要來源；缺來源只阻擋該 task，不建立替代任務。
- C2：缺原環境但已有 pending review 時，恢復 manual 且禁止派工的控制器。
- C3：新 profile 核對原 thread metadata 與原 worktree；原 request／環境摘要不改。
- C4：原生 candidate-readback 驗證 durable alias、Git index owner 與目錄內 scope files。

## How to get to it (user POV)

`host rebuild --request-file`、`host rebuild --request-file ... --adopt-current-profile`、`host task candidate-readback --request-file ... --order-id ...`；Soodles 原 loop inspect／candidate review continuation。

## Driving it with original CLI

Preconditions: 原 accepted request、完整 snapshot、terminal attempts、原 config／Git 與 worktree custody。profile adoption 另需明確選定 HOME／CODEX_HOME、工具、必要環境來源及原 thread rollout metadata。process runtime 可採用這個已核對的新 profile；codex-app-server resume 仍要求原 NativeHome，選定不同 home 必須拒絕。

- 首先使用普通 rebuild。缺 worker profile 且有原 pending review 時，核對 owner.controller_only、manual、原 pause 及 worker=0；supervised 與直接派工均須拒絕。
- 使用 candidate-readback，核對原 request/order/session、logical→physical custody、原 Git common/admin 與 scope_files。Soodles 以原 logical 身分保存 candidate，以 physical 讀 bytes。
- 採用現行 profile 時才加 `--adopt-current-profile`。CLI 記錄 `selected_current_profile` 和原 thread metadata 引用；保持 manual，由原 task review 決定修訂。
- 重讀同一 request，核對無新 attempt；profile 的 thread/worktree 不符、含糊來源及缺 custody 都拒絕。
- `native_rolling_probe.py` 覆蓋正常交接、同 thread resume 與 pending review；缺 profile 控制器的 live 驗收使用原恢復任務或隔離的 process fixture。

## Gotchas

新 profile 不等於原環境還原。process session 不要求不存在的 native.json；沿該 runtime 原讀回。不可只因資料 owner 名稱不同而要求歷史 Session 接手。審查可恢復不代表業務候選已通過，也不授權自動 request-changes。

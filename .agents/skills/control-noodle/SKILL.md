---
name: control-noodle
description: Operate and verify Noodle Host maintenance, rolling versions, context handoff and rebuild, and automatic release with Soodles SwapManager. Use original CLI owners and requests; does not authorize task dispatch or publication.
---

# Control Noodle

用此入口管理 Host 換版、context 交接與資源退出。Noodle 持有原 request、Host 選版、worker、租約與配額。Soodles 持有原 task review 及 App SwapManager 事件。Skill 不保存另一份任務狀態。

## Launch

先選實際 binary、Host root、原 request 與外部 evidence 目錄。每個命令都明確指定 `NOODLE_HOST_DIR`；涉及專案時另給 `--project-dir`。從原 `host switch --status` 讀取選定版本，不能從 shell 的預設路徑推斷服務版本。

一般操作使用既有 Host。只有已授權的驗證才建立獨立 Host、repository、profile 和本機回應服務。原任務缺少輸入或因壓力等待時，保留同一 request；不能另開 Host 繞過 admission。

需要編譯時，在選定 Noodle source 執行 `pnpm build`。保存 binary 與來源證據。換版目標使用獨立且不覆寫的 binary 路徑。不要改寫仍被 Host 或 worker 使用的檔案。

## Doctor

從 repository root 執行：

```text
python3 .agents/skills/control-noodle/scripts/host_doctor.py --binary "$NOODLE_BIN" --host-root "$NOODLE_HOST_DIR"
```

需要觀察既有任務時加 `--request-file "$NOODLE_REQUEST"`。成功只表示 Host 身分已觀測。壓力、原 task 狀態、缺件與 admission 結果分別讀回。原 CLI 讀取可能維護目錄權限及 metadata lock，不宣稱零檔案效果。

每次獨立 live drive 先 Doctor。發生非預期失敗後，再 Doctor 並讀原效果；不能用第二次寫入測試第一次是否成功。未載入新 binary、資料漂移與效果未知要分開處理。

## Drive

一般操作只讀對應 feature：

- [身分與選版](features/identity.md)：前端、Host 與原 task 版本的關係。
- [舊版首次維護](features/legacy-maintenance.md)：原控制器停派、等待、保存與單次正常退出。
- [Rolling 與共用配額](features/rolling.md)：兩代共存、第三代排隊與跨版本前端。
- [Context 交接重建](features/context-rebuild.md)：保留原候選、審查控制器及明確選定的新 profile。
- [自動釋放與 SwapManager](features/release.md)：原生 MCP 退出、配額歸還及 App 事件的獨立驗收。

已有合法 owner next 時直接消費。只有會改變 Scope、風險或 Oracle 的未決語義才使用現有 Thinking Router；可查事實先讀回。使用者已接受的決策不重問。缺少原環境只能如實記錄；明確選定現行 profile 不等於還原舊環境。

## Evidence

保存原命令、exit、stdout/stderr、原 owner 前後讀回與可辨識的程序觀測。證據放在 checkout 外的選定目錄。原 request 與原環境摘要保持不變；successor 與新 profile 由原 CLI 記錄。

ACK、worker=0、空 loaded 清單及程序退出各自只能支持其觀測。自動釋放的通過需要原生卸載、已觀測子程序／generation 退出、租約及配額歸還；仍需保留的共用 Host 可以繼續存在。局部 PASS 不關閉 task acceptance、App SwapManager 或 Parent Goal。

本機回應服務是隔離外部模型的 runtime fixture。它不證明模型遵循此 Skill。P-class 指引的行為驗證沿選定 repository 的 review-writing、evals 與 Schema feedback，保留真實 consumer trace。

## Cleanup

由原 Host 控制清理本次測試 context。失敗時先讀原效果並保留證據。不要用 PID 或 process group 強制終止 worker、恢復中的程序或未證實空閒的 Host。只在本次建立、身分相符且原資源讀回全空的 fixture Host 上，使用其正常 SIGTERM handler；觀測退出。

不要為清理測試終止有用途的共用 Host。不要刪原候選、dirty bytes、Git index、原歷史或 evidence。清理後再次確認證據可讀；未清理完成要記錄原 owner 與具體殘留。

## Helpers

以下命令從 repository root 執行。變數由本次原選擇提供，不是新的產品設定：

```text
python3 .agents/skills/control-noodle/scripts/auto_release_probe.py --binary "$NOODLE_BIN" --host-root "$NOODLE_HOST_DIR" --codex "$CODEX_BIN" --evidence "$NOODLE_EVIDENCE"
python3 .agents/skills/control-noodle/scripts/legacy_maintenance_probe.py --old "$NOODLE_LEGACY_BIN" --new "$NOODLE_BIN" --evidence "$NOODLE_EVIDENCE" --paused --queue
python3 .agents/skills/control-noodle/scripts/native_rolling_probe.py "$NOODLE_EVIDENCE" "$NOODLE_VARIANTS" "$CODEX_BIN" local-task
```

auto-release helper 需要現有 rolling Host。legacy helper 建立隔離舊 Host；缺來源反例使用新的 evidence 目錄及 `--missing-source`，不加 paused／queue。rolling helper 建立隔離 Host，並要求 variants 目錄內 `noodle-rolling-a`、`noodle-rolling-b`、`noodle-rolling-c` 具有三個不同 SHA；B 的版本標記為 `control-noodle-b`。兩者在 macOS 使用 `/usr/bin/sandbox-exec` 將 fixture 的外連限制為本機回應服務。`mcp_release_fixture.py` 僅由前者的私人 Codex profile 啟動；不要當成共用服務。

## Maintenance

[Feature Map](features/README.md) 是完整維護範圍。首次建立沿原 pstack create-verification-skill；後續沿已安裝的原 maintain-verification-skill。保留其單一來源，不複製上游 Skill。

完整 maintain 必須逐 feature 取得獨立唯讀 source review 與 live 覆蓋。單一協調 Session 驅動實際效果。維護回合只改此 Skill 目錄；產品缺陷先交原產品修復工作，修正後再續行受影響 drive。缺少真實 App 事件或其他必要能力時，完整維護為 `blocked`，不能把 CLI 子集稱為全部通過。

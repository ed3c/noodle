# Control Noodle Feature Map

一般操作只選相關 feature。完整 maintain 涵蓋五項；每項保留 source、live、拒絕例與清理證據。歷史測試只有在相同來源與主張仍適用時才能重用。

| Feature | 操作入口 | 需要的真實能力 |
| --- | --- | --- |
| [Identity](identity.md) | Doctor、switch status、task status | 選定 binary、既有 Host、需要時的原 request |
| [Legacy maintenance](legacy-maintenance.md) | switch initial-maintenance | 支援的舊版 binary、原 requests、macOS 程序／socket 觀測 |
| [Rolling](rolling.md) | switch、跨版本 CLI | 不同 SHA 的 binaries、隔離 native fixture |
| [Context rebuild](context-rebuild.md) | rebuild、candidate-readback | 原 snapshot、history、worktree custody；profile adoption 另需原 thread history |
| [Release](release.md) | 原生 completion、Interrupt／SubagentStop | 真實 Codex/MCP；App 路徑另需原 carrier、binding 與真實事件 |

新 feature 需要新入口、Consumer／Acceptance 邊界或無法由現有 control 區分的實際故障。新反例與來源漂移維護原 feature，不按每個 command 或未知值建立檔案。

`clean` 需要完整 source+live。`changed` 沿原 pstack shipping 規則。缺必要 prerequisite 或不能安全完成 shipping 為 `blocked`；已覆蓋的子集單獨列出。

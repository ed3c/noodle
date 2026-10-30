---
name: verify-neo-loop-statechart
description: 修改 Soodles outer／Noodle inner 的 continuation、unknown effect、Frozen feedback 或獨立驗收時，用原 statechart/TLC 檢查有界模型並以 Pele 繪製路徑。
---

# 內外閉環的有界驗證

先讀 [來源與範圍](source-map.md)，把本次變更對回真實函數。
使用原 `/statechart` 讀取每份 TLA+ 結果。使用原
`/create-formal-verification-skill` 方法維護模型與真實程式的對照。
沿原 admission 選定這些 supporting skills；名稱可見不代表 worker 已載入。

將 [模型](models/NeoLoop.tla)、選定 config 與 view 複製到 Repo 外的 scratch。
依目前安裝的 `/statechart` 原指令執行，不複製其他版本的工具命令。
`historical.cfg` 保留已重現的歷史 archive gate。
`current.cfg` 對應本工作樹的 custody 修正；未部署不代表 live Host 已採用。
`optimized.cfg` 描述移除該錯誤門檻後的目標，不代替產品修復驗收。
`broken.cfg` 刻意加入重規劃、未知效果重播及跳過驗收，必須被檢出。
不要為了 PASS 弱化原需求或把未檢查規則寫成成立。

每條反例按動作順序在真實程式或原測試重現。不能重現時先質疑模型。
未知效果只讀回；有效 next 直接續行；Frozen feedback 等原合法 revision。
目前要求不因局部 commit、模型 PASS 或 SVG 產生而完成。

使用原 `pele-render` 支援技能繪製每份展示的 Mermaid，保留 SVG 與原文字。
模型檢查與渲染是不同能力。這份 skill 不另建 admission、scheduler 或效果權限。
模型省略 OS/process lifetime、RPC 實作、credentials、worker 排程、任務內容與時間成本。
它不證明全產品、語義分類、實際最佳速度或完整 Goal 已交付。

covered source 改變後先重讀對照，再更新模型；確認兩者一致才前移 sync commit。
依原 `/maintain-formal-verification-skill` 維護，不把舊圖當成新程式的驗收。

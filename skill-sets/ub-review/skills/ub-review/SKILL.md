---
name: ub-review
description: >-
  Review a completed candidate with ub-mode only after its original required
  functional acceptance passes. Not for implementation, ordinary outer loops,
  or Poteto correction iterations. Use the original review admission and results.
---

# ub-mode 成品審查

本 skill 是 Noodle 可選 set 的成品審查入口，沒有 schedule trigger。
先讀本契約，再將 [ub-mode 原文](upstream/ub-mode/SKILL.md)作為審查參考。
上游保持原文；其實作、TDD、修改、清理、commit／push 指示不在此審查角色內執行。

## 啟用與效果邊界

原 task 當前完整交付範圍已形成候選，且原必要功能驗收通過，才使用本 skill。
若仍在設計、實作或修正，保留原 Poteto；不要載入上游或啟動本 set 工具。
原本必要的 Formal／Test Manager 驗證仍執行，不能以保持速度為由取消。
消費原 owner 返回的 candidate、base、diff、原要求、驗收證據及 continuation。
輸入缺失先原讀回，不把 worker exit 0 當成品，不猜 candidate digest 或建立新 Stage。
角色切換沿原 admission，不能覆寫 frozen Stage 或與原 Poteto 共用已載入的 thread。
NeoWeaver 必須使用原 Noodle OpenShell，缺能力保持缺口，不退回 process。

審查者只提出 findings，不修改產品、Oracle、測試期待、原授權或 Provider。
必要工具在明列的隔離副本與結果區寫 coverage／metrics／mutation 暫存。
原 candidate 不可作變異實驗的工作目錄。不得自行 push、merge 或清理原 worktree。
工具、runtime 及必要語言依賴由原 Host 在選定審查環境提供；Host 路徑不證明 guest 可用。

## Formal 與 Test Manager

Session 依原 Formal 方法核對獨立需求、Oracle、反例、合法鄰例與替代覆蓋。
不以品質分數、綠色 CI、另一個 Agent 或目前實作輸出反推正確答案。
只有狀態、並發、恢復或性質的具名未知需要時才使用模型／solver。
原 Test Manager 選定必要 controls、target test command、coverage command 及資源範圍。
先讀已有有效結果，只補缺失／失效部分。不要自行執行工具的預設全套測試。
缺 scope 回原 owner；不能以 full suite 補猜，也不能刪掉已接受品質義務。
Tool raw output、語義判定、功能驗收與 Provider 結果分開保存。

## 品質判定

檢查本次修改函式與相關重複片段，保留既有基線。架構發現對照當前完整 Goal，
不擴成全 Repo 整改。Dryer 需要比較修改片段與相關未修改片段，不能只做 changed 對 changed。

| 工具 | 判定與執行限制 |
| --- | --- |
| Crapper | 修改範圍 CRAP < 10。依原選定命令或已核對 coverage 執行；nil／缺 coverage 不能通過。其測試失敗後的零 coverage 不能混成有效觀察。 |
| Dryer | 明確選 0.8 門檻。exit 0 不代表沒有重複；語言、最小片段長度與掃描缺失需揭露。有獨立證據的誤報可排除。 |
| Mutator | 處理未覆蓋與存活變異；等價變異需獨立證據才能排除。timeout、不能編譯與工具失敗不能當有效 killed。上游 snapshot 不能區分原因時，該充分性主張為 unknown。 |
| UML Viewer | 保留原 IR／metrics 及桌面，使用專用本機 Grok companion 入口。Grok 只讀 outer 提供的圖形、摘要與必要片段；不使用上游掃描 Repo、自行改碼／跑測試的預設提示。 |

保留原始結果、來源版本、適用範圍、排除理由及缺件；數字不等於完整行為證明。
無法證明的必要項目仍未通過。無適用程式的文件變更可說明不適用，不能捏造工具 PASS。
工具資料的程序退出碼只描述該程序；不可直接映射成候選接受。

## 回送與恢復

將有來源的 findings 與必要修正結果交原 candidate review／feedback。
目前 Session 在既有授權內交 Poteto 修正。修正迭代不載入本 skill；
新候選功能驗收通過後，再審受影響部分，保留仍有效的原證據。
Oracle 有誤時先查原需求；只有獨立契約支持才能更改，不能配合 candidate 改答案。
新反例推翻架構前提時回 Thinking／More；機械缺件回原 producer，不重新跑所有 routes。

相同 request 讀回原操作，running／wait_for 等原變化；unknown 先查效果，不重播。
來源改變保留舊結果為歷史。部分輸出不等於完整審查，三次修正失敗依原契約重估。
審查結論交原 Schema／驗收 owner；本 skill 不授權交付、發布或 Parent Goal 關閉。
Factory 更新受影響 Cards；Medium 只展示經審查的來源快照、差異、排除與 unknown。
本機模型觀察不冒充 OpenShell guest 執行。

## 本機圖形 companion

NeoWeaver 使用 [companion 契約](companion-rules.md) 與 set 的專用啟動入口。
Grok CLI 在本機執行，不新增 Noodle provider；工程與品質工具仍走原 OpenShell。
執行邊界須拒絕產品 Repo 的直接讀寫，允許選定摘要及圖形 policy／提案操作。
必要限制無法生效時保留拒絕，不以提示詞取代隔離，不退回無限制執行。

Grok 可提出缺失證據、指標刷新與跨 Repo 修改請求。Outer 核對來源與既有授權，
補證據交原 Reader，選測交 Test Manager，產品修正交 Poteto，再把結果回送同候選。
沒有原接口時保存請求與缺口，不能假稱自動交接完成。模型一致不代替獨立驗收。
既有授權內自動往返；擴大範圍或真正未決產品取捨才問人。
無有據缺陷可 NO_CHANGE；必要證據缺失時仍未完成。
Session 中斷保存請求與結果，原入口接續；不建立背景排程，不重播未知效果。

Go 分析資料保留來源檔案摘要、結構、直接靜態呼叫及未知目標，供 UML EDN、
Noodle 工程頁與 Medium 本機頁共用。靜態圖不等於 trace，格式轉接不等於上游內建 Go scanner。
原始品質 metrics 與圖形分開保留；缺 coverage／mutation 不能以空欄或零值通過。

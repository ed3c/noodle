# Grok companion 與來源圖

ub-review 的本機 companion 與產品 OpenShell runtime 是不同責任。
原 UML Viewer 透過 GROK_BIN 啟動本機入口。入口限制產品目錄讀寫，
保留原桌面、mailbox 與 Grok CLI；它沒有新增 Noodle provider。

本次原生 Grok 檔案工具三次回傳 workspace session 未綁定。
改成 outer 提供提示詞資料後，原生回覆成功。這不證明工具綁定已修復，
也不證明 policy／metrics／Poteto 往返已完成。原失敗與成功 export 分開保存。

reviewgraph 的 Go 資料包含靜態直接呼叫與具名未知。目標 GOARCH 決定型別大小，
不能用宿主的預設大小。選定來源摘要不是完整 module／依賴建置快照。
原 UML EDN、Noodle artifact 展示與 Medium 頁面可消費同一份分析。
上游 Go scanner、原 guest 品質數據及原 Host 恢復仍需各自驗收。

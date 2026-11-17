# 固定 OpenShell Go SDK 來源

本目錄是原 Go module 的固定來源副本。Noodle 用相對路徑 replace 載入它。
公開原 module 仍固定於原基準；此副本另外綁定必要的條件刪除實作。
這讓正常建置不依賴本機絕對路徑、go.work 或尚不存在的遠端 fork。

原 repository 為 https://github.com/NVIDIA/OpenShell 。
基準為 bdb956f2cd65a635f1294fccf1d138d263bf650f。
本次原生實作 commit 為 7193561b598cf30a7cf9958d43c77e02ca05f278。
該 commit 目前是本機交付候選；此檔不宣告它已公開、合併或部署。
原 Apache 2.0 LICENSE 保存在本目錄，各檔原版權標示保留。

PROVENANCE.json 記錄逐檔 SHA-256、原 proto 輸入與原生成工具版本。
原 Go module 檔案沿原 sdk/go 相對路徑完整保存；額外的 upstream/proto
只提供生成來源，不是第二份 runtime 協定。
重新生成須在上述原生 commit 的原 repository 根目錄執行原
sdk/go/buf.gen.yaml。使用 buf 1.72.0、protoc-gen-go 1.36.11 和
protoc-gen-go-grpc 1.6.2。不得手改產生的 bindings。

Noodle 只呼叫 DeleteStoppedSandboxIfMatches，綁定同一個 Stopped 觀察的
原身分與 resource_version。原服務在生命週期鎖與每次 storage CAS 重試核對。舊 server 回 UNIMPLEMENTED 時保留
原退休 intent 與資源；不降回按名稱刪除。此 module 不提供 Main 驗收授權。

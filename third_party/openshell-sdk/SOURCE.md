# 固定 OpenShell Go SDK 來源

本目錄是原 Go module 的固定來源副本。Noodle 用相對路徑 replace 載入它。
Go module 路徑仍為 github.com/NVIDIA/OpenShell/sdk/go。
此副本綁定必要的條件刪除實作。Noodle 建置不依賴本機絕對路徑或 go.work。

固定來源 repository 為 https://github.com/ed3c/OpenShell 。
固定 commit 為 7c130f3605b18bd09753fb0257ae63f13d487510。
原上游為 https://github.com/NVIDIA/OpenShell 。
基準為 bdb956f2cd65a635f1294fccf1d138d263bf650f。
原本機實作 commit 為 7193561b598cf30a7cf9958d43c77e02ca05f278。
固定 commit 與原實作的完整 tree 同為 4dda257a54a0869d38bfa72cf38d1b319be2f9a2。
PROVENANCE.json 保留兩者的來源映射。固定來源不表示上游 main、fork main 或部署已採用。
原 Apache 2.0 LICENSE 保存在本目錄，各檔原版權標示保留。

在獨立目錄淺層取回固定 commit 的完整 tree。不使用 filter 或本機 alternates：

```sh
git init openshell-sdk-source
git -C openshell-sdk-source fetch --depth 1 https://github.com/ed3c/OpenShell.git 7c130f3605b18bd09753fb0257ae63f13d487510
git -C openshell-sdk-source checkout --detach 7c130f3605b18bd09753fb0257ae63f13d487510
```

PROVENANCE.json 記錄逐檔 SHA-256、原 proto 輸入與原生成工具版本。
原 Go module 檔案沿原 sdk/go 相對路徑完整保存；額外的 upstream/proto
只提供生成來源，不是第二份 runtime 協定。
重新生成時，在上述固定來源的 repository 根目錄執行原入口：

```sh
buf generate --template sdk/go/buf.gen.yaml
```

使用 buf 1.72.0、protoc-gen-go 1.36.11 和 protoc-gen-go-grpc 1.6.2。
不得手改產生的 bindings。

Noodle 只呼叫 DeleteStoppedSandboxIfMatches，綁定同一個 Stopped 觀察的
原身分與 resource_version。原服務在生命週期鎖與每次 storage CAS 重試核對。舊 server 回 UNIMPLEMENTED 時保留
原退休 intent 與資源；不降回按名稱刪除。此 module 不提供 Main 驗收授權。

# 來源與驗證範圍

模型是 Session 建立的有限抽象，工具不從產品程式自動推導規格。
TLC 1.7.4 與 pstack 0.15.36 產生 Mermaid；Pele 1.9.0 只負責 SVG。
ReadRejectedAdmission 是已查證未接納後的恢復分支；其真實資料檢查由原 Owner 與測試保護，模型不假設任意 unknown 都可重試。

| 模型動作／規則 | 真實 producer／consumer |
| --- | --- |
| UseOriginalNext／AskMaterialQuestion | Soodles neo-route 的當前 Goal 與原 Owner next；真正未決語義由 Session 提出。這是指引抽象，不證明模型判斷正確。 |
| Dispatch／WaitForQuota | Soodles `task_host_admission.retry`／`ready` 與 Noodle 原 Host／worker admission。Session 只讀 native worker 額度。 |
| LoseEffectReadback／ReadOriginalEffect | Soodles `task_host_admission.retry` 的 durable intent、attach 後 status-only 分支。 |
| RetiredArchiveGate | Noodle `OpenShellCustody.observeRetirement`、`initialize` 與 `NewOpenShellGateway`。已完成退役的舊 closure 消失曾阻擋新工作。 |
| FindCounterexample／OriginalLegalRevision | 原 Session 核實反例後，沿原 task review/revision；模型不建立 Native Stage。 |
| IntegrateCommit／OwnerAcceptance／Deliver | NeoWeaver `TaskAdapter` 保留原 candidate、commit、acceptance 與 Provider 結果的獨立邊界。 |

同步讀回來源為 Soodles `5233a45e75570ee56ac594e475b777bd96e2b236`、
NeoWeaver `7ace72b6101755601706347020da2662a293a836`。
Noodle archive 反例對照 base `655edfc3ddb34de4c53b769c555c0f1f26df62e2`；
custody 修正為 `7f409e188585ce676c7b4f2f95e7dae9c0b9a9bd`，
Native 試跑再揭露 not_started subtype 的 archive 依賴，已由
`ded66f606c5d2a4a2f70d34069301c50ba667f0c` 補正。
新增真實 Retire→移除 archive→重開的正反測試先失敗再通過，相關 race 與完整 pnpm check 第三輪通過。
原 Host 已採用補正 binary；原 Soodles Owner 已採納其手動 context 為同一原任務的 runtime successor。
這只證明本次 admission 恢復，原 Stage／acceptance／A 真實產品交付仍各自需要證據。
上游 pstack 相關五檔與 cursor/plugins `4dd1d05818d19792e7cd6e818f1667f95f7e1dbe` bytes 相同。

## 已執行的控制

historical config 由 TLC 探索55狀態，發現三步 archive-gate 反例。
current／optimized config 加入已查證未接納的恢復分支後探索91狀態，六項規則及 deadlock 檢查未發現違反。
broken config 探索136狀態，分別檢出多餘重規劃、archive gate、未知效果重播、跳過驗收。
圖合併相同標籤並省略留在同一標籤的步驟；沒有省略不同標籤的路徑。
這些狀態數是各 config 的實際檢查母體，不是覆蓋率或速度指標。

真實 Soodles mutation control 將原 retry 的 status-only 分支改成 attach。
原 `test_unknown_response_reads_same_request_without_second_attach` 在原碼 PASS，
scratch 實碼 mutant FAIL，移除 mutant 後原碼再次 PASS；沒有修改工作樹。
Noodle 的 archive 反例另有真實 public mTLS open/reopen 回歸與負向控制。
目前 repo-level full check 與 Native 部署結果沿原工程報告；此 skill 不代替它們。

GoalPreserved 在這份模型中是建構不變量；它不證明 Session 沒有遺漏外部需求。
quota=0/1、單一當前任務、未知效果、commit/acceptance 等布林狀態是界限。
無公平性或牆鐘時間模型，不能宣稱所有等待必然終止、全域最優或加速百分比。

已查證未接納分支另由原 Test Manager request `14eeb373087d8be166438c96f1dd664bd2251461f318b465c2a4142b56caeb31` 的44個專用控制保護；完整程式來源已提交於上述 Soodles source sync；整合後136 controls在提交前原程式通過，最終HEAD另由原 Test Manager 保留必要讀回。原 unknown response 不能僅因 not_found 就變成可重播。

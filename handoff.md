# Soodles／Noodle：OpenShell、模組組合與 Harness² 任務交接

交接日期：2026-10-29（Asia/Taipei）。本任務尚未完成。
使用者已確認：提交本檔後，建立全新對話及 App-managed worktree，從本次交接提交開始；新對話讀取後立即依既有授權續作。
這是任務資料、既有決策與證據索引，不是新的執行 owner、資源例外或驗收紀錄。

## 先讀與來源位置

- 本檔提交前的 Noodle source：`77828e690e7a73bc361bdfed1dcd048948b1a802`，分支 `codex/openshell-startup-repair`。
- 原 App worktree：`/Users/neon/.codex/worktrees/openshell-startup-repair/noodle`。新對話使用自己的 App worktree；不要刪除或改寫原路徑，既有證據仍引用它。
- Soodles 修正來源：`/Users/neon/worktrees/soodles-openshell-config-admission`，commit `955e3906097170325c0e92e04c78b9ff70b58d2e`。此標準 Git worktree 的例外已獲使用者授權。
- 下文 `BASE` = `/Users/neon/Documents/Codex/2026-10-25/soodles-noodle-noodle-llm-openshell-https-3`。
- `PD` = `BASE/work/openshell-progress-deployment`；`P` = `BASE/work/harness2-live-preparation`；`E` = `P/evaluation-preparation`。
- 完整需求覆蓋與操作歷史：`BASE/outputs/noodle-current-checklist.md`。先讀頂部及末尾；中間含已被新證據取代的歷史狀態。
- Repo 分類與原生能力來源：`BASE/outputs/noodle-module-compatibility-current.md`。其中舊進度不能取代本檔的新讀回。
- 上一輪續接索引：`PD/host-v3/resume-state.json`。它的 memory-warning 是歷史觀察，已被下列新快照更新。

## 完整任務定義與驗收範圍

目標是讓 Soodles outer loop 與 Noodle Poteto 小迴圈，以小介面、薄轉接器和明確任務設定組合不同 runtime／sandbox、harness、模型、技能 Repo 及呈現 Repo。Soodles 保有 admission、審查與 acceptance；Noodle 保有工作目錄、派工、停止、程序與資源。Repo 是來源，MicroVM 是執行邊界，不把兩者當成通用模組介面，也不新增第二個排程 owner。

以下範圍全部保留，不因交接或局部 PASS 刪減：

| 需求索引 | 完整交付內容 | 已知狀態與剩餘工作 |
| --- | --- | --- |
| R01–R05、R07 | 正式 Soodles → Noodle Host → Mac 原生 OpenShell MicroVM → Codex 訂閱／Pi＋OpenRouter；指令素材、capture、停止證據、成果安全回寫與原 owner 接受 | 兩條正常真模型路徑已通過。證據在 `BASE/work/openshell-refresh-drain-deployment/maintenance-20261028/{codex-v2,pi-v2}/`。這不代表異常情境均完成。 |
| R06、R14 | 中斷後保存 repo／session／日誌／效果，查明狀態，分開原 session 續接與明確的新嘗試；驗收取消、拒絕、失聯與人工恢復 | runtime／Loop／Host／UI 與 provider-free MicroVM 控制已實作及測試；真模型停止→保存→新嘗試尚待公開示範完成。 |
| R08–R09 | Noodle 按需管理專用 gateway、刷新憑證、安全停機、最後租約釋放；每任務必要網域政策 | refresh-drain 修補、原 private Host 安全停機、cleanup 修正與靜態政策驗證已通過。最後自動關閉、故障面及兩任務相反網域政策的真實證據仍待完成。 |
| M01–M05 | 任務設定選固定組合、多技能來源別名、版本與完整素材；跨 harness 已接受成果自動交接；成果預覽與控制頁；Pele 原狀態圖 | 已有實作、來源驗證、控制頁與局部呈現證據。完整 Codex 元件→Pi 圖解、同卡 active_stage、停止／新嘗試、完整關係圖與成果驗收仍待完成。 |
| O01–O05 | Harness² 原生有界優化；Codex 與 Pi 各自完成；獨立保留案例、整輪實帳／時間與品質比較；結果交回 Soodles，跨 Repo 改良先經目標驗證 | Pi 完整訓練已收尾但品質 FAIL。Codex 舊輪不完整，新完整輪已獲明確授權並準備。保留案例、完整比較與 outer feedback 未完成。 |
| R10、D01 | Thinking／Ambitious 的必要 Prompt Delta 沿原 outer／inner 入口傳遞；查清第三方原生角色及相容性 | 已有原生歷史證據與 Repo 分類。SoL-Pi、LoopX、DeepSeek Harness、Colorfigures、Cursor pstack 是候選／分類，不是本次都必須安裝或啟用。 |
| R11–R13 | Soodles 使用可追溯、含 host 修訂號的 Noodle 版本；保留未合併修正；最後只保留一個 canonical 來源；私密憑證與 US$5 上限 | 金鑰與帳務已設置。正式發版／入口更新與舊 root 退役尚未完成；不得以刪除仍有 linked worktree 的舊 `.git` 達成表面上的單一目錄。 |

驗收採原公開 case／rubric、原 owner 與 native readback。安全交付 PASS 不等於圖解品質 PASS；離線測試、設定已保存、UI 卡片或檔案存在，都不代表模型已執行、原生續接成功或優化有效。

## 已確認的決策與持續授權

- 模組、harness、模型於任務／階段開始前明確選定；不搬移執行中對話，不自動換模型、付費來源或隔離環境。
- Mac 使用 OpenShell 原生 MicroVM；第一版只做 Linux 可執行的開發／測試任務。僅隔離 Noodle 派出的 worker。
- Codex 用專用 ChatGPT 訂閱登入，模型 `gpt-5.6-sol`、effort `low`；Pi 用 `openrouter/deepseek/deepseek-v4.1-flash-1017`、effort `low`。
- OpenRouter 實帳 canonical model 是 `deepseek/deepseek-v4.1-flash-20261017`。不得因名稱相近自行替換。
- 本次 OpenRouter 總上限 US$5。最後已核對的 key usage 為 US$0.388148325，remaining US$4.611851675；下一個付費呼叫前由原程式重新核對。Pi 訓練七次呼叫實帳共 US$0.296527962，已全部結清。訂閱可歸屬美元成本未知時保持 null，不能記零。
- 私密檔 `/Users/neon/.config/openshell-noodle/.env` 已設為 0600。只讓既有程式供應器讀取；LLM 不讀檔案、不輸出金鑰、不複製進 prompt、Repo、handoff 或 guest 素材。
- 專用 Codex grant 由原 gateway 管理；不重匯／重用舊 refresh grant。不要求重新登入，除非新 native readback 證明確有必要。
- gateway 由 Noodle 按需管理、專用且獨佔 VM lifecycle；VM 名稱不重用。OpenShell 名稱 API 沒有 expected-ID 原子條件；觀察外部變動就停止相應派工並查明。
- 網路依任務明確允許必要網域。未允許的連線需原拒絕證據；DNS、TLS、timeout、reset 只能 UNKNOWN，不能冒稱 policy deny 或自動重試 probe。
- 同 worker 可載入多組技能。跨 harness 只交接上一階段已接受的固定版本檔案；通過原審查後依設定自動交接，原流程要求人工審查時仍等待。
- 視覺化分成果預覽與控制介面。第一組示範為 dimensional-drawing＋Pele；沿既有 Noodle 頁面選組合、查狀態、執行、停止及明確新嘗試，安裝／組合定義仍用設定檔。
- 中斷不自動續跑模型。原生 session 不支援續接時如實拒絕，先查明舊效果，再以已保存成果明確開始新嘗試。公開示範已包含一次原 UI 停止與 version-bound 新嘗試；不能將它泛化為任意失敗的自動 retry。
- 優化只改任務專用提示詞、技能內容及 harness 設定。憑證、資源、隔離政策、驗收規則固定；品質不退步後才比較含優化器及失敗成本的整輪時間／費用。沒有改善就不採用候選。
- 使用者於本對話已明確授權「啟動新的 Codex 完整優化輪」。授權有效，接手不用重問；不包含重跑 Pi 訓練或無界增加實驗。
- 單一 worker 的既有資源許可不涵蓋 gateway 啟動，也不涵蓋嚴重壓力或持續 swap。不能擴大成新的資源例外。
- 持續遵守「跳過 Swap」：不切換／停止共用 Host、不動其任務。先前維護及跨 chat 通知授權是歷史；不據此向其他 chat 發送新訊息。

## 交接時的新鮮狀態

2026-10-29 02:33:10 +08:00 原生讀回：

- 原專用 Host 仍在：dir `/Users/neon/.local/state/nrd-host-progress-v3`，instance `b17c7b37f3a471c2c6be84e190f275da`，當時 PID `38952`。
- `pressure=normal`、`reason=ready`、`effective_limit=4`、`start_credit=1`；0 contexts、0 workers、0 residents、0 allocations，shutdown 未請求。
- 這推翻上一輪「目前仍被記憶體警告擋住」的現況描述。它不是永久的啟動許可；新對話在效果前仍讀回原 Host。
- 快照：`BASE/work/handoff-preparation-20261029-hjg5iwut/snapshot-summary.json` 及 `resources.stdout.json`；後者 SHA `b20c71353905e1aba15e834dd3d1c280e6d11d07fc04f3f11ab2da94e80bccf8`。
- 原 UI URL：`http://127.0.0.1:52904/contexts`；原 resources 回傳 `/loops`。先核同 origin／identity 再使用，舊瀏覽器 tab ID 不可沿用。
- 舊 private Host `e0d2451b807ec6d3cf9334942e48a151` 與 gateway 已安全關閉；18 個原 VM／證據保留。不要重啟舊 Host。closure 在 `PD/shutdown-stop-01/closure.json`，SHA `d76866b5b7312b238e9883c8dba471068eb107d671f520defd8fac21aadfeef6`。
- 現行 private Host binary 是 `PD/noodle-eadc56ab`，source `eadc56ab09014a7285aa4b70f13db94ecd3068a6`，SHA `44f4270a16820b107addc3180bb083358b0c343fa6ebe5bfdc4befdcbf8738b4`。handoff／Python 文件提交不代表它已換版。
- patched gateway：`BASE/work/native-gateway-versions/0.1.5+noodle.refresh-drain.1/openshell-gateway`，SHA `375d1330904e41edc980ce7ecde5a55758938d78eb70fe33f47cc5ce20455c49`。不可換回未驗證的原版停機流程。
- 保留原 credential DB `/Users/neon/.local/state/nrd-gateway/gateway.db`、TLS、gateway endpoint `https://127.0.0.1:17672` 與18個 retained 身分。新 custody root `/Users/neon/.local/state/noodle-progress-custody-v3` 由原 runtime 管理，不手建／清空。
- 新 profiles：`PD/profiles-eadc56ab/profile.json`（優化）、`profile-r09.json`（公開網路示範），靜態驗證已 PASS。原生 dependency digest 與 raw JSON FileRef SHA 不同，不得互相比較或改檔配合錯誤假設。

這次交接只讀取狀態及寫文件，沒有新增模型呼叫、啟動 gateway 或重試 admission。工具 session `23348` 是舊對話 launcher 紀錄，不是新對話可接管程序的權限；優先使用原 Host API，不以歷史 PID 操作程序。

讀回原 Host 時，使用 `PD/host-v3/source-migration-955e3906/intent.json` 的明確環境及 `PD/noodle-eadc56ab host resources`，將 `PD` 展開為絕對路徑。不可省略 `NOODLE_HOST_DIR` 而讀到預設共用 Host。

## 最短合法續作順序

### 1. 接回同一公開示範，完成 R06／R09／M01–M05

讀 `PD/soodles-deferred-fix-955e3906/migration-v2.md`、`PD/host-v3/source-migration-955e3906/intent.json` 與 `PD/composition-preparation/revision-02/README.md`。
原根 request：`PD/composition-preparation/revision-02/run-01/component/request.json`，SHA `dd2a4a2d9ffb87fec63d3990181a14b91d8c7d320ee6d1a91c88213cc2b0ab93`。
第一次 UI start 在 VM 前因 memory warning deferred，未 offer 模型；原 native request `7e76ed2440930bb0bf78bf77a32cf59d`、context `971a95bc7911890de8796ba995b20123` 的收據保留為 detached。

Soodles `955e3906` 已修好首次 known-deferred、失敗回覆保留已驗 composition、retry 綁原 Host。新 frozen CLI 在 `E/cli-source-955e3906/cli.json`，SHA `b27732f15c5d5180ce43a2b30dce9314aea17edf494395adfe3024871bef9403`；完整 source receipt 在同目錄 `receipt.json`。
新 CLI child 固定 Python 3.14.6；Harness² runner 仍是 Python 3.13，兩者不可混為同一測試環境。

資源及原 Host 身分允許時，沿已準備的 source-migration intent，以 frozen CLI 原 `manager-request loop_retry_host_admission` 消費同 request；stdin 在該 intent 同目錄 `retry-input.json`。此入口先核原 deferred／generation／資源，再保留新 native admission 身分鏈。不要重新 prepare、重建 request、刪 task-owner 或 generic requeue。後續只消費實際回傳的原 next。

舊 UI registration 仍固定 c01 bundle。Host 不允許同 ID 換 launcher，故沒有熱改 registry／bundle，也沒有第二張卡。新來源可接同 request；舊卡只沿其已支援的 admitted-stage inspect／stop／new_attempt 使用，不宣稱三項修正已部署到舊 UI。

公開示範順序：Codex 先寫 partial／marker，在 `checkpoint_wait_started` 後由原 UI 停止；核模型 drain、repo/session archive、ProgressHead、Stop ACK/fresh Stopped；再明確按原 version-bound 新嘗試。相同 case／partial／marker 與不同 session 身分需有證據。最終 component 原審查及 integrated PASS 後，原 accepted handoff 自動進 Pi drawing。180 秒自然等待結束不算停止成功。
兩階段都用完整 network-proof 素材。Codex 只允許 example.com，Pi 只允許 example.org；固定 Node 每 host 一次 GET，驗 native Create policy 與原拒絕日誌。原 OpenShell exec 已注入 Node CA，不能停用 TLS 驗證。
最後以真瀏覽器、原圖形投影與固定 rubric 驗收 component、drawing、Pele 完整關係圖及同卡操作。量測／截圖協定在 `PD/composition-activation-tools-v3/browser-evaluator-protocol.md`。

### 2. 執行已授權的 Codex 完整新輪

使用 `P/codex-complete-round-02/revision02/README.md` 與 `delivery.json`（SHA `7336824ce37696a7f148bdc359399271720534f4e7ebfb49b62c29dcd6a57c3a`）。revision01 保留為不可啟動的歷史。
七個全新 App controls 是 `/Users/neon/.codex/worktrees/codex-round02-solver-{01,02,03,04}/noodle` 與 `codex-round02-proposer-{01,02,03}/noodle`；base `c5b4cdef`，named branch 及 repo/cwd/clean doctor 已核對。它們和 source worktree 是不同用途。

先執行原 launcher 的預設唯讀 preflight。確認未有另一模型工作、run／handoffs 未存在，再以 `/opt/homebrew/bin/python3.13 -B P/codex-complete-round-02/revision02/launch.py --run` 啟動；將 `P` 展開成上述絕對路徑。這是已明確授權的新輪，不需舊 chat 的額外批准。
原 native core `1cfe254e8d25b15f37048c3730134686abe60335`，k=1／seq，4 solver／3 proposer，原 SharedGate 序列化。callback 等原 candidate review／integrated acceptance，不自批。需 Session 消費 `handoffs` 內原 review input；不知道原效果時只讀回，不重跑 launcher。
此 runner 沒有綁定舊 chat 身分；使用者已授權的新 Session 可續作，原 Runtime 身分與所有來源驗證仍須成立。CLI timeout 為120秒；route 的3600秒是 metadata，不能當作 Host 強制 deadline。
新輪 runner 固定 `77828e69`、CLI 固定 `955e3906`、Host／binary／profile 明確固定。Codex-only 不建立 OpenRouter client。worker 僅見公開 case、同 accepted component／geometry，evaluator rubric 留外部；與舊輪的 input 不同，不能直接作跨輪效率比較。

舊 Codex 輪有兩次派工前拒絕，後續 c1 已基於 fallback 產生；不能補兩行後冒稱舊輪完整。保留 `E/codex-incomplete-review-01/REPORT.md` 及原 journal，勿改 fingerprint／cache／既有候選。

### 3. 凍結候選後才開保留案例，完成 O01–O05

Pi 訓練已完整完成，不重跑。四次 solver＋三次 proposer 已 safe-delivery accepted、逐 generation 結算；最後一次費用 US$0.087921158。原 native core 在七筆 exact cache 上收尾，無新增模型呼叫。
Pi final freeze：`E/pi-final-freeze/freeze.json`，SHA `078bf5afd751a65bbc8109175b09f7e641d1a87de09aa56a35cd01e2f4e528d1`。圖解 c1 頁首文字重疊；native judge 在 `E/pi-training-independent-final-03/native-pair-result.json`，`quality_non_regression=FAIL`、`adoption_authorized=false`，缺證據項為 UNKNOWN。不要把 native 產生候選當作優化收益。

原 final 同 call 的停止後對帳、接受與實帳皆完成；證據分別在 `P/pi-final-landed-reconciliation/`、`P/pi-final-stopped-integration-01/`、`P/pi-final-billing-stopped-01/`。原四筆 pending effects 是當下取消，不是重寫為歷史成功，不得再 merge 或重派該 call。

私有保留案例目前未開啟。先固定新 Codex final，再由 evaluator 讀私有 oracle，worker／optimizer 不讀。十二個 App controls 已備：`/Users/neon/.codex/worktrees/h2-eval-{codex,pi}-h{1,2}-{stage-a,baseline-b,final-b}/noodle`；詳見 `E/cli-doctor-20261028T114038Z/completion-receipt.json`、`E/caller-preparation-v1/`。
每案例先接受 A，baseline B 與 final B 各自直接消費同一 accepted A，不串成 A→baseline B→final B。固定模型／環境／素材／timeout，候選只改允許的 harness 內容；未知費用／時長保持未知，計入 optimizer、失敗與 evaluation 成本。圖解逐項真像素／幾何驗收，不以字串或 worker 自評代替。
Pi derived module 已固定 `/Users/neon/.codex/worktrees/harness2-pi-eval-modules/noodle` commit `e4d16e9f9b4be42bf08b3327ce4688aa29073fac`；索引 `E/pi-derived-module-source.json`。Codex 舊 module／catalog 不能當新輪 final；依新 freeze 重建明確候選來源。
比較結果交回原 Soodles owner 審查。品質不退步和完整實帳／時間不足時不得宣稱 Pareto 改善；跨組合建議先經目標驗證，不自動寫入其他 harness／skills Repo。

### 4. 完成生命週期與可追溯發版

完成 R08 最後租約自動關閉、R14 必要故障面與原效果讀回，再收尾 R11／R12。
`PD/validation-eadc56ab.json` 是 Go build／全 Go tests／vet／architecture／fixtures 完整 `pnpm check` PASS，source 是 eadc56ab；後續 Python 單組支援另通過 `BASE/work/harness2-native-contract-control-03/controls-1793205299941124000.json`。兩者不冒稱為同一 HEAD 的整套檢查。
Soodles 955e3906 的31個功能／負例與48個 CLI 契約測試 PASS；新 CLI 唯讀 inspect 原 request 通過、effects=[]。詳見 `PD/soodles-deferred-fix-955e3906/`。這些測試不代替真模型示範。

保留 `/Users/neon/noodle` 為 canonical。交接時本機 main 是 `40d32da1a441056caf27f34323800b5a45a39f01`，不是本次未合併功能的 HEAD；遠端最新 main 在發版前另做新鮮比對。
舊 `/Users/neon/github_projects/noodle` main 是 `55b2042e0e17ac02053ffaab1f724dd3ffebbb88`，仍登記40個 worktree；兩 root 是不同 common-dir。先前盤點含9處未釐清 runtime custody 與一份他人 `.gitignore` 修改。安全收尾方案在 `BASE/work/r11-r12-retirement-and-release-plan.md`；先備份、核原 owner、解除 linked consumers，再退休。不能手改 `.git`／commondir、reset 他人修改或用 symlink 冒充合併。被 skip-Swap 排除的 owner 仍有責任時保留相應路徑並報告缺口。
Noodle 版本必須含 host 修訂號、source commit／binary SHA；PID 不是版本號。全域入口與已載入的共用 Host 不因文件、source commit 或 symlink 而自動升級，不能擅自切換。

## 工程與證據紀律

用中文回覆。先讀新 worktree 的 AGENTS 及相關 brain；增量提交，沿既有實作與 library。App 隔離用 App 正式入口，不在主 tree 切 branch、不冒充 managed worktree、不停用 hooks／tests、不用 `--no-verify`。
同一問題最多三種失敗嘗試後記錄原因、質疑前提、換方法；不以新 request／Host／模型掩蓋未知效果。原 request、candidate、ACK、失敗、archive、帳單與 custody 不覆寫。
建置用 `pnpm build`，驗證用 `pnpm check`。Go 在 linked checkout 曾誤判 VCS：只對 build 綁定經驗證的 GIT_DIR／GIT_WORK_TREE，freeze 前核 `go version -m` 的 exact revision／clean。全測、Host、worker 與 task env 必須移除這些 selectors。細節在 `brain/codebase/worktree-gotchas.md`。
不要整份輸出 CLI 的兩千多個 source refs、巨大 native summary／paired environment／intent。只讀所需欄位，其餘留原檔。不要讀 `.env`、gateway DB 或 grant 來找「方便」的恢復路徑。
本輪只刪除了可重建的本任務 Rust／Go 編譯 cache；所有 source、frozen binary、VM、憑證、原證據保留。最新 cache receipt 是 `BASE/work/rust-dependency-cache-cleanup-20261029-completed.json`。

## 建議 skills 與交接完成條件

- 原 Soodles `handoff`、`thinking-router`：`/Users/neon/worktrees/soodles-openshell-config-admission/.agents/skills/`。本介面沒有原生 Skill tool；本交接依完整文件執行，沒有冒稱 native Skill invocation。使用者明示 root `handoff.md` 並 commit，取代 upstream 預設存暫存目錄的路徑建議。
- 新 worktree 的 `noodle`、`execute`、`worktree`、`commit`；遇到具體失敗用 `debugging`，Go 變更用 `go-best-practices`，測試依影響選 `testing`。使用者的 App 隔離規則優先於 skill 中舊的 CLI worktree 範例。
- 需要新的實質語義決策才用 Thinking／Ambitious 雙向檢視；已確認的模型、預算、順序、原 owner next 不重問。雙腦是同一 Session 的方法，並不要求增加兩個 agent 或新 gate。
- 本交接只要求文件完整、來源可讀、狀態／授權／下一步無矛盾並提交；它不證明接手 Agent 已遵循、原任務已完成或 runtime ownership 已轉移。

下一個 Session 先讀本檔與索引，核新 worktree 來源及原 Host 身分，沿上述原 request／原 next 繼續。不要重新訪談已定案的整體架構，也不要只完成單一增量後遺漏其餘 Parent Goal。

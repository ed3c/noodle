# 唯讀關係圖與靜態成果預覽

在既有 Loop Graph 選定任務後，展開「Pele 關係圖」。圖形僅使用原投影的 owner、occurrence、交接及 input/output artifact 關係；unknown 保留，沒有新排程者。超過節點／邊／文字上限時拒絕此視圖，原圖與資料不截斷。Pele 延後至展開時載入。

選擇 `format: text/html` 的已登記 artifact 後，可開啟靜態預覽。API 僅接受 registry 中的 artifact id，加上選定 graph version 與原 artifact SHA256；沿既有 ArtifactContent 檢查實際檔案、大小、來源雜湊與 symlink。它不接受瀏覽器提供的主機檔案路徑。

預覽不代表已接受。此版本回覆 `acceptance: unknown`、`effects: []`；原生驗收仍由原 Soodles owner 保存的 task envelope、candidate、head、integrated acceptance record 與 FileRef 決定。後續 composition owner 必須明確接入這些來源，不能用 node.status 字串或可顯示檔案推導驗收。本切片未加入 M05 派工／控制功能，也未變更獨立 `/loops` 的唯讀邊界。

## 靜態交付契約

HTML 最大 2 MiB；結構經 HTML parser allowlist 驗證。允許基本排版、CSS 與靜態 inline SVG primitives。腳本、事件處理器、導航連結、meta refresh、SVG animation、foreignObject、表單與外部嵌入元素一律明確拒絕，沒有靜默清洗後冒充原成果。顯示用 HTML 加入最先執行的 CSP，拒絕所有網路資源；CSS 可 inline。iframe 沒有任何 sandbox 額外權限，且使用 no-referrer。

回覆 `document` 是顯示衍生文件，不是原 artifact bytes；metadata SHA256 始終識別原來源。原始內容仍可用既有文字檢視比較。完整動態 HTML 不支援此預覽。

dimensional-drawing 的固定上游範例使用 inline JS 產生 SVG，也載入 Google Fonts。示範任務須明確要求：可在生成過程使用工具／腳本，但交付前將圖形序列化成靜態 SVG＋HTML/CSS，使用系統或已授權本機字型，不留腳本或網路依賴。這保留原技能的繪圖方法與完整素材，驗收實際圖解內容，不要求執行上游範例腳本。此切片沒有產生真實模型圖解，也不把測試 fixture 當成果。

## Pele 來源

固定 `pele@1.10.0`，lockfile 記錄 tarball SHA512。官方 registry 的 SLSA provenance 對應 `obsidianmd/pele@4eff76f98c73b6817800a05773517f35832e4e92`；來源聲明與比對範圍保存在 `ui/src/client/pele/provenance.json`，完整 MIT／Lucide ISC notice 保存在 `ui/public/third-party/pele-LICENSE.txt`，建置時一併分發。沒有宣稱獨立驗證 attestation 簽章。

只註冊原生 flowchart renderer，關閉 links/images，不提供可插入 markup 的 icons/math resolver。標籤經 Mermaid entity 編碼，語法 ID 由本地索引產生；結果 SVG 僅來自此固定 renderer。大圖拒絕，不增加全域 cap。

## 驗證

聚焦 Go tests：`GOMAXPROCS=2 go test -p 1 ./internal/loopview ./server -run 'TestStaticPreview|TestLoopStaticPreview'`。涵蓋來源漂移、未知 ID、錯誤圖版本、唯讀 HTTP、主動 HTML 拒絕與 CSP。`server/testdata/static-preview.html` 是實際 API 輸出的 golden，含用來驗證 CSP 的外部 CSS URL。

UI tests：`pnpm --filter noodle-ui test src/client/pele-projection.test.ts src/client/loop-preview.test.ts src/components/LoopArtifactPreview.test.tsx src/components/LoopGraph.test.tsx`。使用真 Pele package，確認語法注入只顯示為文字、原狀態不變、來源漂移與虛假接受回覆拒絕，且預覽不預先載入。

瀏覽器驗收：`node e2e/ui/loop-preview-proof.mjs` 僅啟動隔離的 localhost Vite fixture，輸出 `/proof` URL；透過 CUA 新開測試 tab，確認真 Pele 圖與「開啟靜態預覽」後的 S1=24 SVG。此 fixture 使用 mock transport，但 HTML document 與真 Go endpoint 的 golden 相同；不是 live Host／模型驗收。結束後正常停止該測試程序。

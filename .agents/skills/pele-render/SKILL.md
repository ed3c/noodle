---
name: pele-render
description: 用 Pele 將 Mermaid 圖表繪製為可自訂 CSS 的 SVG。Noodle Poteto 需要展示 statechart 或其他 Mermaid 圖表時使用。
---

# Pele 繪圖

使用隨 skill 保存、未修改的 Pele 1.9.0。來源與摘要見 [source.json](source.json)。
呼叫目前 Host 或 worker payload 已提供的 Node，不在執行中下載套件。

```text
node /absolute/skill/path/scripts/render.mjs /absolute/chart.mmd /absolute/chart.svg
node /absolute/skill/path/scripts/render.mjs /absolute/chart.mmd /absolute/chart.svg --css /absolute/theme.css
```

把範例中的 skill 路徑換成原 instruction manifest 提供的實際位置。
輸出目錄須已存在。相同輸入與輸出再次執行會保留相同 SVG，不重寫檔案。
修改圖表時沿同一輸出原子更新。輸入與輸出不能是同一個檔案。

Pele 的 CSS variables 設在 `.pele`，例如 `--pele-bg`、`--pele-fg`。
statechart 的 `.breaks`、`.mayBreak`、`.bugPath`、`.repeats` 可用 CSS 標示。
Pele 忽略 Mermaid `theme` 與 `themeVariables`。使用 `--css` 提供主題。

展示產生的 SVG，並保留原 Mermaid 與 TLC 結果。Pele 渲染成功只證明圖表可繪製，
不證明模型、程式、Native worker 或產品驗收正確。缺 Node 或渲染失敗時保留原圖與錯誤，
不宣稱 SVG 已產生，也不改用其他 renderer。

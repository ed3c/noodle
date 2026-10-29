import { createRoot } from "react-dom/client";
import { ContextControls } from "../../../ui/src/components/ContextControls";
import "../../../ui/src/app.css";
createRoot(document.getElementById("root")!).render(<><p role="note">隔離介面驗收：模擬 owner 傳輸，沒有模型或 Host 操作。</p><ContextControls /></>);

import { useEffect, useState } from "react";
import {
  actOnContext,
  fetchContextChoices,
  observeContext,
  previewContextArtifact,
} from "~/client/context-controls";
import type {
  ContextAction,
  ContextChoice,
  ContextFileRef,
  ContextObservation,
  ContextAcceptedArtifact,
} from "~/client/context-controls";

const actionLabels: Record<ContextAction, string> = {
  start: "執行已準備任務",
  stop: "停止原任務",
  new_attempt: "依原審查開始新嘗試",
};

const reasonLabels: Record<string, string> = {
  prepared: "任務已準備，可以執行。",
  prepared_original_owner: "任務已準備，可以執行。",
  original_entry: "由原任務流程開始執行。",
  original_dispatch_next_required: "需要原任務流程允許這次派工。",
  original_interrupt_custody: "停止交由原任務管理者確認，保留現有成果。",
  original_task_active: "原任務正在執行，可以要求停止。",
  not_started: "任務尚未開始。",
  no_active_worker: "目前沒有執行中的工作。",
  saved_request_changes_required: "先在原審查流程提出修改，再開始新嘗試。",
  review_required: "先完成原審查流程。",
  original_review_required: "先完成原審查流程。",
  original_saved_revision_next: "原審查已允許修改，可開始新嘗試。",
  original_revision_next_unavailable: "原審查尚未允許新的嘗試。",
  original_attempt_unavailable: "尚無可銜接的前次嘗試。",
  original_attempt_not_terminal: "前次工作尚未結束。",
  native_stopped_completion_unavailable: "尚未確認前次工作已安全停止。",
  original_active_owner_required: "需要原任務管理者查明狀態。",
  original_owner_readback_required: "請先查明原任務狀態。",
  existing_instance_admission_missing: "目前的任務服務尚未支援這個啟動入口。",
  original_interrupt_capability_missing: "目前的任務服務尚未支援此停止入口。",
  native_session_resume_unsupported: "目前不支援續接原模型對話。",
  unsupported: "目前不支援此操作。",
};
const acceptanceLabels: Record<string, string> = {
  PASS: "通過",
  FAIL: "未通過",
  UNKNOWN: "未確認",
};
const statusLabels: Record<string, string> = {
  prepared: "已準備",
  running: "執行中",
  active: "執行中",
  pending: "等待執行",
  stopped: "已停止",
  completed: "已完成",
  failed: "執行失敗",
  refused: "操作未獲允許",
  unknown: "狀態未確認",
  candidate: "等待審查",
  local_task: "原任務處理中",
};
function reasonLabel(reason: string, available: boolean) {
  if (
    !available &&
    [
      "prepared",
      "prepared_original_owner",
      "original_task_active",
      "original_entry",
      "original_saved_revision_next",
    ].includes(reason)
  ) {
    return "目前無法執行，請查明狀態。";
  }
  return reasonLabels[reason] ?? "需要查看原任務管理者的診斷說明。";
}

function Evidence({ label, source }: { label: string; source: ContextFileRef | null }) {
  return (
    <p className="break-all text-sm">
      <strong>{label}：</strong>
      {source ? (
        <>
          <span>{source.path}</span>
          <br />
          <code>{source.sha256}</code>
        </>
      ) : (
        "尚無已確認來源"
      )}
    </p>
  );
}
function AcceptedPreview({
  choice,
  observation,
  artifact,
}: {
  choice: ContextChoice;
  observation: ContextObservation;
  artifact: ContextAcceptedArtifact;
}) {
  const [document, setDocument] = useState<string | null>(null);
  const [message, setError] = useState("");
  const [busy, setBusy] = useState(false);
  async function open() {
    setBusy(true);
    setError("");
    setDocument(null);
    try {
      setDocument(await previewContextArtifact(choice, observation, artifact));
    } catch (error) {
      setError(error instanceof Error ? error.message : "成果版本未確認");
    } finally {
      setBusy(false);
    }
  }
  return (
    <article className="space-y-2">
      <h3>{artifact.label}</h3>
      <details>
        <summary>成果來源細節</summary>
        <Evidence label="已接受成果" source={artifact.source} />
      </details>
      {artifact.format === "html" && (
        <button
          type="button"
          disabled={busy}
          onClick={() => void open()}
          className="border border-border-subtle px-3 py-2"
        >
          {busy ? "核對原驗收來源…" : "預覽已接受圖解"}
        </button>
      )}
      {message && <p role="alert">{message}</p>}
      {document && (
        <iframe
          title={`已接受圖解：${artifact.label}`}
          sandbox=""
          referrerPolicy="no-referrer"
          srcDoc={document}
          style={{
            width: "100%",
            height: 640,
            border: "1px solid currentColor",
            background: "white",
          }}
        />
      )}
    </article>
  );
}
export function ContextControls() {
  const [choices, setChoices] = useState<ContextChoice[]>([]);
  const [selected, setSelected] = useState("");
  const [observation, setObservation] = useState<ContextObservation | null>(null);
  const [busy, setBusy] = useState(false);
  const [message, setError] = useState("");
  const [loading, setLoading] = useState(true);
  useEffect(() => {
    let current = true;
    async function load() {
      try {
        const items = await fetchContextChoices();
        if (current) {
          setChoices(items);
        }
      } catch (error) {
        if (current) {
          setError(error instanceof Error ? error.message : "組合登記讀取失敗");
        }
      } finally {
        if (current) {
          setLoading(false);
        }
      }
    }
    void load();
    return () => {
      current = false;
    };
  }, []);
  const choice = choices.find((item) => item.id === selected);
  async function invoke(action?: ContextAction) {
    if (!choice || busy || (action && !observation)) {
      return;
    }
    setBusy(true);
    setError("");
    try {
      const result =
        action && observation
          ? await actOnContext(choice, observation, action)
          : await observeContext(choice);
      setObservation(result);
    } catch (error) {
      // An uncertain response never leaves an old enabled action on screen.
      setObservation(null);
      setError(error instanceof Error ? error.message : "操作未確認，請查明原任務狀態。");
    } finally {
      setBusy(false);
    }
  }
  return (
    <main className="mx-auto max-w-5xl space-y-6 p-8 text-text-primary">
      <header className="space-y-2">
        <a href="/loops" className="underline">
          查看任務關係與成果
        </a>
        <h1 className="text-2xl font-semibold">固定組合與任務控制</h1>
        <p>選擇 Session 已準備的組合。執行與停止由原任務管理者處理；頁面不更換模型或隔離環境。</p>
      </header>
      {loading && <p role="status">讀取已登記組合…</p>}
      {!loading && choices.length === 0 && (
        <p>目前沒有屬於此 Host 的已登記任務。請先由 Session 準備並登記固定組合。</p>
      )}
      <section className="space-y-3" aria-label="已準備的固定組合">
        <label htmlFor="context-choice">選擇組合與任務</label>
        <select
          id="context-choice"
          disabled={busy || loading}
          value={selected}
          onChange={(event) => {
            setSelected(event.target.value);
            setObservation(null);
            setError("");
          }}
          className="ml-3 border border-border-subtle bg-bg-surface p-2"
        >
          <option value="">請選擇</option>
          {choices.map((item) => (
            <option key={item.id} value={item.id}>
              {item.label}
            </option>
          ))}
        </select>
        {choice && <p>已選擇：{choice.label}</p>}
        <button
          type="button"
          className="border border-border-subtle px-3 py-2"
          disabled={!choice || busy}
          onClick={() => void invoke()}
        >
          查明原任務狀態
        </button>
        <p className="text-sm">查明狀態可能保存原任務的驗收證據。操作結果不明時，不會自動重試。</p>
      </section>
      {busy && <p role="status">等候原任務管理者確認…</p>}
      {message && <p role="alert">{message}</p>}
      {observation && (
        <>
          <section aria-label="原任務狀態" className="space-y-2">
            <h2 className="text-lg font-semibold">
              原任務狀態：{statusLabels[observation.status] ?? "需要查明"}
            </h2>
            <p>可用操作由原任務管理者判定，請依下方說明繼續。</p>
          </section>
          <section aria-label="原任務操作" className="space-y-3">
            {(Object.keys(actionLabels) as ContextAction[]).map((action) => (
              <div key={action}>
                <button
                  type="button"
                  disabled={busy || !observation.actions[action].available}
                  className="border border-border-subtle px-3 py-2 disabled:opacity-50"
                  onClick={() => void invoke(action)}
                >
                  {actionLabels[action]}
                </button>
                <p className="text-sm">
                  {reasonLabel(
                    observation.actions[action].reason,
                    observation.actions[action].available,
                  )}
                </p>
              </div>
            ))}
            <p>原模型對話續接：不支援。</p>
            <p>新嘗試需要原任務已安全停止，且原審查流程允許；保留前次歷史，不代表續接原對話。</p>
          </section>
          <section aria-label="原任務成果與驗收" className="space-y-3">
            <h2 className="text-lg font-semibold">成果與驗收</h2>
            <p>
              驗收：
              {acceptanceLabels[observation.acceptance.status] ?? "未確認"} ·{" "}
              {observation.acceptance.scope === "integrated" ? "整合後成果" : "候選成果"}
            </p>
            <p>以下已接受成果必須由原整合驗收紀錄綁定；一般圖形頁的預覽仍不代表驗收通過。</p>
            {choice &&
              observation.acceptance.artifacts.map((artifact) => (
                <AcceptedPreview
                  key={`${observation.version}-${artifact.id}`}
                  choice={choice}
                  observation={observation}
                  artifact={artifact}
                />
              ))}
          </section>
          <details className="space-y-3">
            <summary>診斷與固定來源</summary>
            <p>
              原始狀態：{observation.status} · {observation.summary}
            </p>
            <p className="break-all">觀察版本：{observation.version}</p>
            <p className="break-all">
              任務：{observation.identity.order_id ?? "尚未派發"} · 對話：
              {observation.identity.session_id ?? "尚無模型對話"}
            </p>
            <p>
              組合：{choice?.composition_id} · 階段：{choice?.stage_id}
            </p>
            <p className="break-all">成果版本：{observation.acceptance.head ?? "尚無已確認版本"}</p>
            <Evidence label="原請求" source={observation.request} />
            <Evidence label="候選成果" source={observation.candidate} />
            <Evidence label="原驗收紀錄" source={observation.acceptance.record} />
            <ul>
              {Object.entries(observation.actions).map(([action, availability]) => (
                <li key={action}>
                  {action}：{availability.reason}
                </li>
              ))}
            </ul>
            <p>
              原驗收：{observation.acceptance.status} · {observation.acceptance.scope}
            </p>
            {observation.effects.length > 0 && (
              <section aria-label="原操作效果">
                <h3>原操作回報</h3>
                <ul>
                  {[...new Set(observation.effects)].map((effect) => (
                    <li key={effect}>{effect}</li>
                  ))}
                </ul>
              </section>
            )}
          </details>
        </>
      )}
    </main>
  );
}

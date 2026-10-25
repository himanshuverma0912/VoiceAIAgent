import { problemText, type Flow } from "../spec";
import { Check, Field, VarText } from "./ui";

const LANGUAGES = [
  { code: "hi", label: "Hindi (hi)" },
  { code: "en", label: "English (en)" },
  { code: "hi-IN", label: "Hindi, India (hi-IN)" },
  { code: "en-IN", label: "English, India (en-IN)" },
  { code: "multi", label: "Several languages (multi)" },
];

export function SettingsEditor(props: {
  flow: Flow;
  editable: boolean;
  problems: string[];
  vars: { name: string; about: string }[];
  change: (fn: (f: Flow) => unknown) => void;
  onSelectNode: (id: string) => void;
}) {
  const { flow, editable: ed, change, vars } = props;
  const s = flow.settings ?? {};
  const setSetting = (fn: (s: NonNullable<Flow["settings"]>) => void) =>
    change((f) => {
      f.settings ??= {};
      fn(f.settings);
    });
  const guardrails = flow.guardrails ?? [];
  const nodeIds = Object.keys(flow.nodes);

  return (
    <div>
      <div className="inspector-head">
        <h3 className="grow">Flow settings</h3>
      </div>
      <p className="hint" style={{ marginTop: -8, marginBottom: 14 }}>
        Apply to every step. Click a node on the canvas to edit it.
      </p>

      {props.problems.length > 0 && (
        <ul className="problems">
          {props.problems.map((p, i) => (
            <li key={i}>
              <strong>{problemText(p).where || "flow"}</strong>: {problemText(p).what}
            </li>
          ))}
        </ul>
      )}

      <Field label="Name">
        <input value={flow.name} disabled={!ed} onChange={(e) => change((f) => (f.name = e.target.value))} />
      </Field>
      <Field label="Flow id" hint="Fixed when the flow was created.">
        <input className="mono" value={flow.flow_id} disabled />
      </Field>
      <Field label="Persona" hint="Who the agent is and how it speaks, shared by every step.">
        <VarText value={flow.persona} rows={6} vars={vars} disabled={!ed} onChange={(v) => change((f) => (f.persona = v))} />
      </Field>

      <div className="field">
        <div className="row" style={{ marginBottom: 4 }}>
          <span className="field-label grow" style={{ margin: 0 }}>
            Guardrails (rules every step follows)
          </span>
          {ed && (
            <button className="small" onClick={() => change((f) => (f.guardrails = [...guardrails, ""]))}>
              Add
            </button>
          )}
        </div>
        {guardrails.length === 0 && <p className="hint">None yet.</p>}
        {guardrails.map((g, i) => (
          <div key={i} className="row" style={{ marginBottom: 6, alignItems: "flex-start" }}>
            <div className="grow">
              <VarText value={g} rows={2} vars={vars} disabled={!ed} onChange={(v) => change((f) => (f.guardrails![i] = v))} />
            </div>
            {ed && (
              <button className="icon danger" title="Remove" onClick={() => change((f) => f.guardrails!.splice(i, 1))}>
                ✕
              </button>
            )}
          </div>
        ))}
      </div>

      <Field label="Greeting" hint="Said word for word when the call starts (the caller can't interrupt it). Leave empty to let the first step speak.">
        <VarText
          value={flow.greeting ?? ""}
          rows={2}
          vars={vars}
          disabled={!ed}
          onChange={(v) => change((f) => (v ? (f.greeting = v) : delete f.greeting))}
        />
      </Field>

      <Field label="Calls start at">
        <select value={flow.start_node} disabled={!ed} onChange={(e) => change((f) => (f.start_node = e.target.value))}>
          {!nodeIds.includes(flow.start_node) && <option value={flow.start_node}>{flow.start_node || "—"} (missing)</option>}
          {nodeIds.map((id) => (
            <option key={id}>{id}</option>
          ))}
        </select>
      </Field>
      <Check
        label="The agent may end the call from any step when the caller clearly wants to"
        checked={flow.allow_end_call_anywhere ?? true}
        disabled={!ed}
        onChange={(v) => change((f) => (f.allow_end_call_anywhere = v))}
      />

      <div className="section">
        <h3 style={{ marginBottom: 10 }}>Voice and model</h3>
        <Field label="Language">
          <select value={s.language ?? "hi"} disabled={!ed} onChange={(e) => setSetting((x) => (x.language = e.target.value))}>
            {!LANGUAGES.some((l) => l.code === (s.language ?? "hi")) && <option>{s.language}</option>}
            {LANGUAGES.map((l) => (
              <option key={l.code} value={l.code}>
                {l.label}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Model" hint="A model name set up in LiteLLM, e.g. voice-fast.">
          <input
            list="model-aliases"
            value={s.llm_model ?? ""}
            disabled={!ed}
            onChange={(e) => setSetting((x) => (e.target.value ? (x.llm_model = e.target.value) : delete x.llm_model))}
          />
          <datalist id="model-aliases">
            <option value="voice-fast" />
          </datalist>
        </Field>
        <Field label="Creativity (temperature)" hint="0 = most predictable. Keep it low for banking flows.">
          <input
            type="number"
            min={0}
            max={2}
            step={0.1}
            value={s.llm_temperature ?? 0}
            disabled={!ed}
            onChange={(e) => setSetting((x) => (x.llm_temperature = e.target.value === "" ? 0 : Number(e.target.value)))}
          />
        </Field>
        <Field label="Cartesia voice id (optional)" hint="Empty = the agent's default voice.">
          <input
            className="mono"
            value={s.tts_voice_id ?? ""}
            disabled={!ed}
            onChange={(e) => setSetting((x) => (e.target.value ? (x.tts_voice_id = e.target.value) : delete x.tts_voice_id))}
          />
        </Field>
      </div>

      {nodeIds.length > 0 && (
        <div className="section">
          <h3 style={{ marginBottom: 8 }}>Steps</h3>
          {nodeIds.map((id) => (
            <div key={id}>
              <button className="link" onClick={() => props.onSelectNode(id)}>
                {id}
              </button>
              <span className="faint"> · {flow.nodes[id].type}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

import { offeredTo, type CatalogEntry, type CatalogKind, type OfferedTool } from "../api";
import { BUILTIN_VARIABLES, problemText, type Flow, type FlowSettings } from "../spec";
import { Listeners } from "./Listeners";
import { ReplyChecks } from "./ReplyChecks";
import { ToolUses } from "./Tools";
import { Check, Field, VarText } from "./ui";

const LANGUAGES = [
  { code: "hi", label: "Hindi (hi)" },
  { code: "en", label: "English (en)" },
  { code: "hi-IN", label: "Hindi, India (hi-IN)" },
  { code: "en-IN", label: "English, India (en-IN)" },
  { code: "multi", label: "Several languages (multi)" },
];

type ModelSetting = "llm_model" | "stt_model" | "tts_voice_id";

const MODEL_FIELDS: { kind: CatalogKind; setting: ModelSetting; label: string; fallback: string }[] = [
  { kind: "llm", setting: "llm_model", label: "LLM", fallback: "the agent's default (LLM_MODEL, normally voice-fast)" },
  { kind: "stt", setting: "stt_model", label: "Speech-to-text model", fallback: "Deepgram Nova-3" },
  { kind: "tts_voice", setting: "tts_voice_id", label: "Voice", fallback: "the agent's default voice" },
];

/** A dropdown of the catalog's entries of one kind that this flow's workspace may use. */
function ModelSelect(props: {
  kind: CatalogKind;
  setting: ModelSetting;
  fallback: string;
  value: string;
  workspace: string;
  language: string;
  catalog: CatalogEntry[] | null;
  disabled: boolean;
  onChange: (v: string) => void;
}) {
  const all = (props.catalog ?? []).filter((e) => e.kind === props.kind);
  const offered = all.filter((e) => offeredTo(e, props.workspace));
  const current = all.find((e) => e.model_id === props.value);
  const listed = offered.some((e) => e.model_id === props.value);
  const langNote = (e: CatalogEntry) => (e.languages.length ? ` · ${e.languages.join(", ")}` : "");
  return (
    <>
      <select value={props.value} disabled={props.disabled || props.catalog === null} onChange={(e) => props.onChange(e.target.value)}>
        <option value="">Default: {props.fallback}</option>
        {props.value && !listed && (
          <option value={props.value}>
            {current ? `${current.label} (${current.enabled ? "not offered to this workspace" : "switched off"})` : `${props.value} (not in the catalog)`}
          </option>
        )}
        {offered.map((e) => (
          <option key={e.model_id} value={e.model_id}>
            {e.label}
            {props.kind === "tts_voice" ? langNote(e) : ` (${e.model_id})${langNote(e)}`}
          </option>
        ))}
      </select>
      {current?.notes && <div className="hint">{current.notes}</div>}
      {props.catalog !== null && offered.length === 0 && <div className="hint">Nothing in the model catalog for this workspace yet; ask a platform admin.</div>}
    </>
  );
}

export function SettingsEditor(props: {
  flow: Flow;
  workspace: string;
  catalog: CatalogEntry[] | null;
  tools: OfferedTool[] | null;
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
  const modelField = (m: (typeof MODEL_FIELDS)[number]) => (
    <Field key={m.setting} label={m.label}>
      <ModelSelect
        kind={m.kind}
        setting={m.setting}
        fallback={m.fallback}
        value={(s[m.setting] as string | null | undefined) ?? ""}
        workspace={props.workspace}
        language={s.language ?? "hi"}
        catalog={props.catalog}
        disabled={!ed}
        onChange={(v) => setSetting((x: FlowSettings) => (v ? (x[m.setting] = v) : delete x[m.setting]))}
      />
    </Field>
  );
  const nodeIds = Object.keys(flow.nodes);
  // Values the flow defines (captured, collected, saved): the ones a resumed call can keep.
  const builtins = new Set(BUILTIN_VARIABLES.map((v) => v.name));
  const flowValues = vars.filter((v) => !builtins.has(v.name));

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
        <h3 style={{ marginBottom: 6 }}>Tools on every step ({flow.tools?.length ?? 0})</h3>
        <p className="hint" style={{ marginTop: 0 }}>
          Lookups any caller may need at any moment, like help-centre answers or branches. Tools that need a verified caller
          go on the steps after verification instead.
        </p>
        <ToolUses
          uses={flow.tools ?? []}
          offered={props.tools}
          vars={vars}
          editable={ed}
          everyStep
          change={(uses) => change((f) => (uses.length ? (f.tools = uses) : delete f.tools))}
        />
      </div>

      <div className="section">
        <h3 style={{ marginBottom: 6 }}>Resume dropped calls</h3>
        <Check
          label="If a caller hangs up mid-call and rings back within 15 minutes, offer to carry on from the same step"
          checked={s.resume?.enabled ?? false}
          disabled={!ed}
          onChange={(v) => setSetting((x) => (v ? (x.resume = { ...x.resume, enabled: true }) : delete x.resume))}
        />
        {s.resume?.enabled && (
          <>
            <p className="hint" style={{ marginTop: 0 }}>
              The agent asks “carry on or start over?”. A caller who had been verified verifies again, then goes straight back to
              where they were. Phone calls only; kept encrypted for 15 minutes and used once.
            </p>
            <div className="field-label">Keep these answers for the call back (everything else is asked again)</div>
            {flowValues.length === 0 && <p className="hint">This flow doesn't capture, collect or save any values.</p>}
            <div className="keep-list">
              {flowValues.map((v) => (
                <Check
                  key={v.name}
                  label={v.name}
                  checked={(s.resume?.keep ?? []).includes(v.name)}
                  disabled={!ed}
                  onChange={(on) =>
                    setSetting((x) => {
                      const keep = (x.resume?.keep ?? []).filter((k) => k !== v.name);
                      x.resume = { ...x.resume, enabled: true, keep: on ? [...keep, v.name] : keep };
                      if (!x.resume.keep?.length) delete x.resume.keep;
                    })
                  }
                />
              ))}
            </div>
            <p className="hint">
              Keep what saves the caller repeating themselves (the issue, a merchant). Verification details are asked again
              anyway, so don't keep them.
            </p>
          </>
        )}
      </div>

      <div className="section">
        <h3 style={{ marginBottom: 6 }}>Reply checks ({s.reply_checks?.length ?? 0})</h3>
        <p className="hint" style={{ marginTop: 0 }}>
          Every sentence the agent says is checked before the caller hears it, on every step. Steps can add more.
        </p>
        <ReplyChecks
          selected={s.reply_checks ?? []}
          editable={ed}
          change={(checks) => setSetting((x) => (checks.length ? (x.reply_checks = checks) : delete x.reply_checks))}
        />
        <Field
          label="Said instead when a check stops a sentence (optional)"
          hint={
            (s.language ?? "hi").startsWith("hi")
              ? "Empty: a Hindi line that fits the check, e.g. “कृपया एक पल रुकिए, मुझे वह राशि दोबारा देखनी होगी।”"
              : "Empty: a line that fits the check, e.g. “Let me just double-check that figure for you.”"
          }
        >
          <VarText
            value={s.reply_check_line ?? ""}
            rows={2}
            vars={vars}
            disabled={!ed}
            onChange={(v) => setSetting((x) => (v ? (x.reply_check_line = v) : delete x.reply_check_line))}
          />
        </Field>
      </div>

      <div className="section">
        <h3 style={{ marginBottom: 6 }}>Side listeners ({s.listeners?.length ?? 0})</h3>
        <p className="hint" style={{ marginTop: 0 }}>
          They follow what the caller says, alongside the conversation, and act when they hear something: warn about secret codes, jump to a
          step, keep a promise to pay. None run unless you add them.
        </p>
        <Listeners
          flow={flow}
          editable={ed}
          vars={vars}
          change={(listeners) => setSetting((x) => (listeners.length ? (x.listeners = listeners) : delete x.listeners))}
        />
      </div>

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
        {MODEL_FIELDS.filter((m) => m.kind === "llm").map((m) => modelField(m))}
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
        {MODEL_FIELDS.filter((m) => m.kind !== "llm").map((m) => modelField(m))}
        <p className="hint">The lists come from the model catalog, set up by platform admins.</p>
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

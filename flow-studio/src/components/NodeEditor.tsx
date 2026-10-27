import { useEffect, useRef, useState } from "react";
import type { OfferedTool } from "../api";
import {
  BUILTINS,
  IDENTIFIER_HINT,
  IDENTIFIER_RE,
  NODE_TYPE_LABEL,
  VALUE_TYPES,
  problemText,
  toolName,
  uniqueId,
  type AgentNode,
  type ApiNode,
  type CaptureField,
  type CollectField,
  type CollectNode,
  type EndNode,
  type Flow,
  type FlowNode,
  type Transition,
  type ValueType,
} from "../spec";
import { ToolInputs, ToolLabel, ToolSelect, ToolUses } from "./Tools";
import { Check, Field, ListInput, VarText } from "./ui";

type Vars = { name: string; about: string }[];

interface Props {
  flow: Flow;
  id: string;
  node: FlowNode;
  editable: boolean;
  problems: string[];
  vars: Vars;
  focusTransition?: number;
  /** The integration tools this flow's workspace may use (null while loading). */
  tools: OfferedTool[] | null;
  /** Change this node (the callback gets a copy to modify). */
  change: (fn: (node: FlowNode) => unknown) => void;
  rename: (newId: string) => void;
  remove: () => void;
  makeStart: () => void;
}

export function NodeEditor(p: Props) {
  const otherIds = Object.keys(p.flow.nodes).filter((x) => x !== p.id);
  return (
    <div>
      <div className="inspector-head">
        <h3 className="grow">{NODE_TYPE_LABEL[p.node.type]}</h3>
        {p.flow.start_node === p.id ? (
          <span className="chip">start</span>
        ) : (
          p.editable && (
            <button className="small" onClick={p.makeStart} title="Calls begin at this node">
              Make start
            </button>
          )
        )}
        {p.editable && (
          <button className="small danger" onClick={() => confirm(`Delete “${p.id}” and the transitions into it?`) && p.remove()}>
            Delete
          </button>
        )}
      </div>

      {p.problems.length > 0 && (
        <ul className="problems">
          {p.problems.map((x, i) => (
            <li key={i}>
              <strong>{problemText(x).where.replace(`nodes.${p.id}`, "") || "this node"}</strong>: {problemText(x).what}
            </li>
          ))}
        </ul>
      )}

      <RenameField id={p.id} taken={otherIds} editable={p.editable} rename={p.rename} />

      {p.node.type === "agent" && <AgentForm {...p} node={p.node} otherIds={otherIds} />}
      {p.node.type === "collect" && <CollectForm {...p} node={p.node} otherIds={otherIds} />}
      {p.node.type === "api" && <ApiForm {...p} node={p.node} otherIds={otherIds} />}
      {p.node.type === "end" && <EndForm {...p} node={p.node} />}
    </div>
  );
}

function RenameField(props: { id: string; taken: string[]; editable: boolean; rename: (id: string) => void }) {
  const [value, setValue] = useState(props.id);
  useEffect(() => {
    setValue(props.id);
  }, [props.id]);
  const error = !IDENTIFIER_RE.test(value) ? IDENTIFIER_HINT : props.taken.includes(value) ? "another node already has this id" : null;
  const apply = () => {
    if (value !== props.id && !error) props.rename(value);
    else setValue(props.id);
  };
  return (
    <Field label="Node id" hint={error && value !== props.id ? <span className="bad">{error}</span> : "Used in transitions and the logs."}>
      <input
        className="mono"
        value={value}
        disabled={!props.editable}
        onChange={(e) => setValue(e.target.value)}
        onBlur={apply}
        onKeyDown={(e) => e.key === "Enter" && (e.target as HTMLInputElement).blur()}
      />
    </Field>
  );
}

function NodeSelect(props: { value: string; options: string[]; onChange: (v: string) => void; disabled: boolean }) {
  const known = props.options.includes(props.value);
  return (
    <select value={props.value} disabled={props.disabled} onChange={(e) => props.onChange(e.target.value)}>
      {!known && <option value={props.value}>{props.value ? `${props.value} (missing)` : "— choose a node —"}</option>}
      {props.options.map((id) => (
        <option key={id} value={id}>
          {id}
        </option>
      ))}
    </select>
  );
}

// ── Conversation step ─────────────────────────────────────────────────────────

function AgentForm(p: Props & { node: AgentNode; otherIds: string[] }) {
  const ed = p.editable;
  const transitions = p.node.transitions ?? [];
  const focused = useRef<HTMLDivElement>(null);
  useEffect(() => {
    focused.current?.scrollIntoView({ block: "nearest", behavior: "smooth" });
  }, [p.focusTransition, p.id]);
  const changeT = (i: number, fn: (t: Transition) => void) =>
    p.change((n) => {
      const t = (n as AgentNode).transitions![i];
      fn(t);
    });

  return (
    <>
      <Field label="Instructions" hint="What the agent does in this step. Type {{ to insert a variable.">
        <VarText value={p.node.instructions} rows={6} vars={p.vars} disabled={!ed} onChange={(v) => p.change((n) => ((n as AgentNode).instructions = v))} />
      </Field>
      <Field label="When this step starts">
        <select
          value={p.node.on_enter ?? "speak"}
          disabled={!ed}
          onChange={(e) => p.change((n) => ((n as AgentNode).on_enter = e.target.value as "speak" | "wait"))}
        >
          <option value="speak">The agent speaks first</option>
          <option value="wait">The agent waits for the caller</option>
        </select>
      </Field>

      <div className="section">
        <h3 style={{ marginBottom: 6 }}>Tools on this step ({p.node.tools?.length ?? 0})</h3>
        <p className="hint" style={{ marginTop: 0 }}>
          Lookups the agent may do here when the caller needs them, like the balance. Inputs set here are filled from variables
          and the AI can't change them.
        </p>
        <ToolUses
          uses={p.node.tools ?? []}
          offered={p.tools}
          vars={p.vars}
          editable={ed}
          change={(uses) => p.change((n) => (uses.length ? ((n as AgentNode).tools = uses) : delete (n as AgentNode).tools))}
        />
      </div>

      <div className="section">
        <div className="row" style={{ marginBottom: 8 }}>
          <h3 className="grow">Ways out ({transitions.length})</h3>
          {ed && (
            <button
              className="small"
              onClick={() => p.change((n) => ((n as AgentNode).transitions = [...transitions, { to: p.otherIds[0] ?? "", when: "" }]))}
            >
              Add
            </button>
          )}
        </div>
        {transitions.length === 0 && (
          <p className="hint">No transitions: the call stays here until it ends. Drag from this node's right edge to another node to add one.</p>
        )}
        {transitions.map((t, i) => (
          <div key={i} className="sub-card" ref={i === p.focusTransition ? focused : undefined} style={i === p.focusTransition ? { borderColor: "var(--accent)" } : undefined}>
            <div className="row">
              <span className="field-label" style={{ margin: 0 }}>Go to</span>
              <div className="grow">
                <NodeSelect value={t.to} options={p.otherIds} disabled={!ed} onChange={(v) => changeT(i, (x) => (x.to = v))} />
              </div>
              {ed && (
                <button className="icon danger" title="Remove" onClick={() => p.change((n) => (n as AgentNode).transitions!.splice(i, 1))}>
                  ✕
                </button>
              )}
            </div>
            <div style={{ marginTop: 8 }}>
              <span className="field-label">When</span>
              <VarText value={t.when} rows={2} vars={p.vars} disabled={!ed} placeholder="e.g. The caller has described their issue." onChange={(v) => changeT(i, (x) => (x.when = v))} />
            </div>
            <details style={{ marginTop: 8 }}>
              <summary className="muted" style={{ fontSize: 12.5, cursor: "pointer" }}>
                Tool name <code>{toolName(t)}</code> · captures {Object.keys(t.capture ?? {}).length}
              </summary>
              <div style={{ marginTop: 8 }}>
                <Field label="Tool name (optional)" hint="Only needed when two ways out go to the same node.">
                  <input
                    className="mono"
                    value={t.name ?? ""}
                    disabled={!ed}
                    placeholder={`go_to_${t.to}`}
                    onChange={(e) => changeT(i, (x) => (e.target.value ? (x.name = e.target.value) : delete x.name))}
                  />
                </Field>
                <Captures transition={t} editable={ed} change={(fn) => changeT(i, fn)} />
              </div>
            </details>
          </div>
        ))}
      </div>
    </>
  );
}

function Captures(props: { transition: Transition; editable: boolean; change: (fn: (t: Transition) => void) => void }) {
  const capture = props.transition.capture ?? {};
  const entries = Object.entries(capture);
  const ed = props.editable;
  const setField = (name: string, fn: (c: CaptureField) => void) => props.change((t) => fn(t.capture![name]));
  const renameKey = (oldName: string, newName: string) =>
    props.change((t) => {
      const next: Record<string, CaptureField> = {};
      for (const [k, v] of Object.entries(t.capture ?? {})) next[k === oldName ? newName : k] = v;
      t.capture = next;
    });
  return (
    <div>
      <div className="row" style={{ marginBottom: 6 }}>
        <span className="field-label grow" style={{ margin: 0 }}>
          Values to capture when taking this way out
        </span>
        {ed && (
          <button
            className="small"
            onClick={() =>
              props.change((t) => {
                t.capture = { ...(t.capture ?? {}), [uniqueId("value", Object.keys(t.capture ?? {}))]: { type: "string", description: "" } };
              })
            }
          >
            Add value
          </button>
        )}
      </div>
      {entries.map(([name, c]) => (
        <div key={name} className="sub-card" style={{ background: "var(--panel)" }}>
          <div className="row">
            <KeyInput value={name} editable={ed} onCommit={(v) => renameKey(name, v)} taken={entries.map(([k]) => k).filter((k) => k !== name)} />
            <select style={{ width: 110 }} value={c.type ?? "string"} disabled={!ed} onChange={(e) => setField(name, (x) => (x.type = e.target.value as ValueType))}>
              {VALUE_TYPES.map((v) => (
                <option key={v}>{v}</option>
              ))}
            </select>
            {ed && (
              <button className="icon danger" title="Remove" onClick={() => props.change((t) => delete t.capture![name])}>
                ✕
              </button>
            )}
          </div>
          <div className="row">
            <input placeholder="What it is, for the LLM" value={c.description} disabled={!ed} onChange={(e) => setField(name, (x) => (x.description = e.target.value))} />
          </div>
          <div className="row">
            <ListInput value={c.enum} disabled={!ed} placeholder="Allowed values, comma-separated (optional)" onChange={(v) => setField(name, (x) => (v ? (x.enum = v) : delete x.enum))} />
          </div>
          <div className="row" style={{ marginTop: 6 }}>
            <Check label="Required" checked={c.required ?? true} disabled={!ed} onChange={(v) => setField(name, (x) => (v ? delete x.required : (x.required = false)))} />
            <span className="hint" style={{ margin: "0 0 8px" }}>
              Use as <code>{`{{${name}}}`}</code>
            </span>
          </div>
        </div>
      ))}
    </div>
  );
}

/** An id-like name edited in place and applied on blur. */
function KeyInput(props: { value: string; taken: string[]; editable: boolean; onCommit: (v: string) => void }) {
  const [v, setV] = useState(props.value);
  useEffect(() => {
    setV(props.value);
  }, [props.value]);
  const ok = IDENTIFIER_RE.test(v) && !props.taken.includes(v);
  return (
    <input
      className="mono grow"
      value={v}
      disabled={!props.editable}
      title={ok ? "" : IDENTIFIER_HINT}
      style={ok ? undefined : { borderColor: "var(--bad)" }}
      onChange={(e) => setV(e.target.value)}
      onBlur={() => (ok && v !== props.value ? props.onCommit(v) : setV(props.value))}
      onKeyDown={(e) => e.key === "Enter" && (e.target as HTMLInputElement).blur()}
    />
  );
}

// ── API call ──────────────────────────────────────────────────────────────────

function ApiForm(p: Props & { node: ApiNode; otherIds: string[] }) {
  const ed = p.editable;
  const offered = p.tools ?? [];
  const t = offered.find((x) => x.ref === p.node.tool);
  const save = p.node.save ?? [];
  const set = (fn: (n: ApiNode) => void) => p.change((n) => fn(n as ApiNode));
  return (
    <>
      <p className="hint" style={{ marginTop: -4 }}>
        Calls the tool by itself (the AI doesn't decide), then goes on. Use it for lookups that must always happen, like
        checking who the caller is.
      </p>
      <Field label="Tool">
        <ToolSelect
          value={p.node.tool}
          offered={offered}
          disabled={!ed}
          onChange={(ref) =>
            set((n) => {
              n.tool = ref;
              n.save = [];
              const next = offered.find((x) => x.ref === ref);
              n.inputs = Object.fromEntries((next?.tool.inputs ?? []).filter((i) => i.required).map((i) => [i.name, `{{${i.name}}}`]));
            })
          }
        />
      </Field>
      {t && (
        <div style={{ marginTop: -6, marginBottom: 12 }}>
          <ToolLabel t={t} />
          <p className="hint">{t.tool.description}</p>
        </div>
      )}
      <div className="section">
        <h3 style={{ marginBottom: 8 }}>Inputs</h3>
        <ToolInputs use={p.node} offered={t} vars={p.vars} editable={ed} allInputs change={(inputs) => set((n) => (n.inputs = inputs))} />
      </div>
      <div className="section">
        <h3 style={{ marginBottom: 6 }}>Keep from the answer</h3>
        {!t || t.tool.outputs.length === 0 ? (
          <p className="hint">{t ? "This tool returns nothing to keep." : "Choose a tool first."}</p>
        ) : (
          <div className="row wrap" style={{ gap: "4px 14px" }}>
            {t.tool.outputs.map((o) => (
              <Check
                key={o}
                label={`{{${o}}}`}
                checked={save.includes(o)}
                disabled={!ed}
                onChange={(v) => set((n) => (n.save = v ? [...save, o] : save.filter((x) => x !== o)))}
              />
            ))}
          </div>
        )}
        <p className="hint">Kept values become variables for the next steps; the agent also sees them.</p>
      </div>
      <div className="section">
        <Field label="Say while it runs (optional)" hint="Covers the wait, e.g. “One moment while I check.”">
          <VarText single value={p.node.say ?? ""} vars={p.vars} disabled={!ed} onChange={(v) => set((n) => (v ? (n.say = v) : delete n.say))} />
        </Field>
        <Check
          label="This step verifies the caller"
          checked={!!p.node.verifies}
          disabled={!ed}
          onChange={(v) => set((n) => (v ? (n.verifies = true) : delete n.verifies))}
        />
        <p className="hint" style={{ marginTop: -4 }}>
          Tools that need a verified caller can only be used after this step works. The bank's tool must fail when the details
          don't match.
        </p>
        <Field label={p.node.verifies ? "When the caller is verified, go to" : "When it works, go to"}>
          <NodeSelect value={p.node.next} options={p.otherIds} disabled={!ed} onChange={(v) => set((n) => (n.next = v))} />
        </Field>
        <Field label={p.node.verifies ? "When the details don't match (or it fails), go to" : "If it fails, go to"}>
          <NodeSelect value={p.node.on_error} options={p.otherIds} disabled={!ed} onChange={(v) => set((n) => (n.on_error = v))} />
        </Field>
      </div>
    </>
  );
}

// ── Collect details ───────────────────────────────────────────────────────────

function CollectForm(p: Props & { node: CollectNode; otherIds: string[] }) {
  const ed = p.editable;
  const fields = p.node.fields ?? [];
  const changeF = (i: number, fn: (f: CollectField) => void) => p.change((n) => fn((n as CollectNode).fields[i]));
  return (
    <>
      <Field label="Instructions for the whole step (optional)">
        <VarText value={p.node.instructions ?? ""} rows={3} vars={p.vars} disabled={!ed} onChange={(v) => p.change((n) => ((n as CollectNode).instructions = v))} />
      </Field>
      <div className="section">
        <div className="row" style={{ marginBottom: 8 }}>
          <h3 className="grow">Fields, asked in order ({fields.length})</h3>
          {ed && (
            <button
              className="small"
              onClick={() =>
                p.change((n) => (n as CollectNode).fields.push({ name: uniqueId("field", fields.map((f) => f.name)), type: "string", description: "" }))
              }
            >
              Add
            </button>
          )}
        </div>
        {fields.map((f, i) => (
          <div key={i} className="sub-card">
            <div className="row">
              <KeyInput value={f.name} editable={ed} taken={fields.filter((_, j) => j !== i).map((x) => x.name)} onCommit={(v) => changeF(i, (x) => (x.name = v))} />
              <select style={{ width: 150 }} value={f.builtin ?? "custom"} disabled={!ed} onChange={(e) => changeF(i, (x) => setKind(x, e.target.value))}>
                {BUILTINS.map((b) => (
                  <option key={b.kind} value={b.kind}>
                    {b.label}
                  </option>
                ))}
                <option value="custom">Custom (describe it)</option>
              </select>
              {ed && (
                <>
                  <button className="icon" title="Move up" disabled={i === 0} onClick={() => p.change((n) => move((n as CollectNode).fields, i, -1))}>
                    ↑
                  </button>
                  <button className="icon danger" title="Remove" onClick={() => p.change((n) => (n as CollectNode).fields.splice(i, 1))}>
                    ✕
                  </button>
                </>
              )}
            </div>
            {!f.builtin && (
              <>
                <div className="row">
                  <input className="grow" placeholder="What to ask for, for the LLM" value={f.description ?? ""} disabled={!ed} onChange={(e) => changeF(i, (x) => (x.description = e.target.value))} />
                  <select style={{ width: 100 }} value={f.type ?? "string"} disabled={!ed} onChange={(e) => changeF(i, (x) => (x.type = e.target.value as ValueType))}>
                    {VALUE_TYPES.map((v) => (
                      <option key={v}>{v}</option>
                    ))}
                  </select>
                </div>
                <div className="row">
                  <ListInput value={f.enum} disabled={!ed} placeholder="Allowed values, comma-separated (optional)" onChange={(v) => changeF(i, (x) => (v ? (x.enum = v) : delete x.enum))} />
                </div>
              </>
            )}
            {f.builtin === "name" && (
              <div className="row wrap" style={{ marginTop: 8 }}>
                <Check label="Ask for last name too" checked={f.options?.last_name ?? true} disabled={!ed} onChange={(v) => changeF(i, (x) => (x.options = { ...x.options, last_name: v }))} />
                <Check label="Confirm spelling" checked={f.options?.verify_spelling ?? false} disabled={!ed} onChange={(v) => changeF(i, (x) => (x.options = { ...x.options, verify_spelling: v }))} />
              </div>
            )}
            {f.builtin === "digits" && (
              <div className="row" style={{ marginTop: 8 }}>
                <span className="field-label" style={{ margin: 0 }}>How many digits</span>
                <input
                  type="number"
                  min={1}
                  max={32}
                  style={{ width: 80 }}
                  value={f.options?.num_digits ?? ""}
                  disabled={!ed}
                  onChange={(e) => changeF(i, (x) => (x.options = { ...x.options, num_digits: e.target.value === "" ? undefined : Number(e.target.value) }))}
                />
                <Check label="Read back" checked={f.options?.ask_for_confirmation ?? false} disabled={!ed} onChange={(v) => changeF(i, (x) => (x.options = { ...x.options, ask_for_confirmation: v }))} />
              </div>
            )}
            <div style={{ marginTop: 8 }}>
              <span className="field-label">Extra instructions (optional)</span>
              <VarText value={f.instructions ?? ""} rows={2} vars={p.vars} disabled={!ed} onChange={(v) => changeF(i, (x) => (v ? (x.instructions = v) : delete x.instructions))} />
            </div>
          </div>
        ))}
      </div>
      <div className="section">
        <Field label="When every field is collected, go to">
          <NodeSelect value={p.node.next} options={p.otherIds} disabled={!ed} onChange={(v) => p.change((n) => ((n as CollectNode).next = v))} />
        </Field>
      </div>
    </>
  );
}

function setKind(f: CollectField, kind: string): void {
  delete f.options;
  if (kind === "custom") {
    delete f.builtin;
    f.type ??= "string";
    f.description ??= "";
    return;
  }
  delete f.type;
  delete f.description;
  delete f.enum;
  f.builtin = kind as CollectField["builtin"];
  if (kind === "digits") f.options = { num_digits: 4 };
}

function move<T>(list: T[], i: number, by: number): void {
  const j = i + by;
  if (j < 0 || j >= list.length) return;
  [list[i], list[j]] = [list[j], list[i]];
}

// ── End call ──────────────────────────────────────────────────────────────────

function EndForm(p: Props & { node: EndNode }) {
  const ed = p.editable;
  return (
    <>
      <Field label="Final words" hint="Said just before hanging up. The caller can't reply, so ask “anything else?” in the step before.">
        <VarText value={p.node.instructions} rows={4} vars={p.vars} disabled={!ed} onChange={(v) => p.change((n) => ((n as EndNode).instructions = v))} />
      </Field>
      <Field label="Goodbye (optional)" hint="Leave empty for a short thank-you in the caller's language.">
        <VarText value={p.node.goodbye ?? ""} rows={2} vars={p.vars} disabled={!ed} onChange={(v) => p.change((n) => (v ? ((n as EndNode).goodbye = v) : delete (n as EndNode).goodbye))} />
      </Field>
    </>
  );
}

// Picking integration tools for a flow: on every step (flow settings), on one
// conversation step, or for an API step. The config service checks the rules
// (verified caller, locked inputs); these forms just make them visible.
import { type OfferedTool } from "../api";
import { splitTool, type ToolUse } from "../spec";
import { Check, VarText } from "./ui";

type Vars = { name: string; about: string }[];

export function ToolLabel({ t }: { t: OfferedTool | undefined }) {
  if (!t) return null;
  return (
    <span className="row wrap" style={{ gap: 6 }}>
      <span className="muted" style={{ fontSize: 12 }}>
        {t.integration.label}
      </span>
      {t.tool.sensitive && (
        <span className="chip warn" title="Only after a step that verifies the caller">
          needs a verified caller
        </span>
      )}
    </span>
  );
}

/** Choose a tool; tools already used elsewhere are still listed (the server reports clashes). */
export function ToolSelect(props: {
  value: string;
  offered: OfferedTool[];
  disabled: boolean;
  onChange: (ref: string) => void;
  allowSensitive?: boolean;
  placeholder?: string;
}) {
  const options = props.offered.filter((t) => props.allowSensitive !== false || !t.tool.sensitive);
  const known = options.some((t) => t.ref === props.value);
  return (
    <select value={props.value} disabled={props.disabled} onChange={(e) => props.onChange(e.target.value)} aria-label="Tool">
      {!known && <option value={props.value}>{props.value ? `${props.value} (not available)` : props.placeholder ?? "— choose a tool —"}</option>}
      {options.map((t) => (
        <option key={t.ref} value={t.ref}>
          {t.tool.name} · {t.integration.label}
          {t.tool.sensitive ? " (verified callers)" : ""}
        </option>
      ))}
    </select>
  );
}

/** The inputs of one use of a tool. allInputs: an API step (no AI), so every input is set here. */
export function ToolInputs(props: {
  use: { tool: string; inputs?: Record<string, string> };
  offered: OfferedTool | undefined;
  vars: Vars;
  editable: boolean;
  allInputs?: boolean;
  change: (inputs: Record<string, string>) => void;
}) {
  const t = props.offered?.tool;
  const inputs = props.use.inputs ?? {};
  if (!t) {
    const keys = Object.keys(inputs);
    return keys.length ? (
      <p className="hint">Inputs: {keys.join(", ")}</p>
    ) : null;
  }
  const set = (name: string, value: string | undefined) => {
    const next = { ...inputs };
    if (value === undefined) delete next[name];
    else next[name] = value;
    props.change(next);
  };
  if (t.inputs.length === 0) return <p className="hint">This tool takes no inputs.</p>;
  return (
    <div className="tool-inputs">
      {t.inputs.map((i) => {
        const locked = t.locked.includes(i.name);
        const isSet = i.name in inputs;
        const here = props.allInputs || locked || isSet;
        return (
          <div key={i.name} className="tool-input">
            <div className="row wrap" style={{ gap: 6 }}>
              <code>{i.name}</code>
              {locked && (
                <span className="chip" title="The AI may not choose this; set it from a collected value">
                  set by the flow
                </span>
              )}
              {i.required && <span className="muted" style={{ fontSize: 12 }}>required</span>}
              <span className="grow" />
              {!props.allInputs && !locked && (
                <Check
                  label="Set it here"
                  checked={isSet}
                  disabled={!props.editable}
                  onChange={(v) => set(i.name, v ? "" : undefined)}
                />
              )}
            </div>
            <div className="hint" style={{ marginTop: 0 }}>
              {i.description}
            </div>
            {here ? (
              <VarText
                single
                value={inputs[i.name] ?? ""}
                vars={props.vars}
                disabled={!props.editable}
                ariaLabel={i.name}
                placeholder={locked ? "e.g. {{card_last4}}" : "Text, or {{a variable}}"}
                onChange={(v) => set(i.name, v)}
              />
            ) : (
              <div className="hint">The AI fills this in from the conversation.</div>
            )}
          </div>
        );
      })}
    </div>
  );
}

/** A list of tools the AI may use (on every step, or on one step). */
export function ToolUses(props: {
  uses: ToolUse[];
  offered: OfferedTool[] | null;
  vars: Vars;
  editable: boolean;
  everyStep?: boolean;
  change: (uses: ToolUse[]) => void;
}) {
  const offered = props.offered ?? [];
  const byRef = new Map(offered.map((t) => [t.ref, t]));
  const ed = props.editable;
  const available = offered.filter((t) => !props.everyStep || !t.tool.sensitive);
  const unused = available.filter((t) => !props.uses.some((u) => u.tool === t.ref));
  const update = (i: number, fn: (u: ToolUse) => ToolUse) => props.change(props.uses.map((u, j) => (j === i ? fn(u) : u)));
  return (
    <div>
      {props.offered !== null && offered.length === 0 && (
        <p className="hint">No integrations are offered to this workspace yet; a platform admin adds them.</p>
      )}
      {props.uses.map((u, i) => {
        const t = byRef.get(u.tool);
        return (
          <div key={i} className="sub-card">
            <div className="row">
              <div className="grow">
                <ToolSelect
                  value={u.tool}
                  offered={offered}
                  allowSensitive={!props.everyStep}
                  disabled={!ed}
                  onChange={(ref) => update(i, () => ({ tool: ref }))}
                />
              </div>
              {ed && (
                <button className="icon danger" title="Remove" onClick={() => props.change(props.uses.filter((_, j) => j !== i))}>
                  ✕
                </button>
              )}
            </div>
            {t ? (
              <>
                <div style={{ marginTop: 6 }}>
                  <ToolLabel t={t} />
                </div>
                <p className="hint" style={{ margin: "4px 0 8px" }}>
                  {t.tool.description}
                </p>
              </>
            ) : (
              <p className="hint bad">
                {u.tool ? `${splitTool(u.tool)[1] || u.tool} isn't available to this workspace.` : "Choose a tool."}
              </p>
            )}
            <ToolInputs
              use={u}
              offered={t}
              vars={props.vars}
              editable={ed}
              change={(inputs) => update(i, (x) => (Object.keys(inputs).length ? { ...x, inputs } : { tool: x.tool }))}
            />
          </div>
        );
      })}
      {ed && unused.length > 0 && (
        <ToolSelect
          value=""
          offered={unused}
          allowSensitive={!props.everyStep}
          disabled={false}
          placeholder="+ Add a tool"
          onChange={(ref) => {
            const t = byRef.get(ref);
            const inputs: Record<string, string> = {};
            for (const name of t?.tool.locked ?? []) inputs[name] = `{{${name}}}`;
            props.change([...props.uses, Object.keys(inputs).length ? { tool: ref, inputs } : { tool: ref }]);
          }}
        />
      )}
    </div>
  );
}

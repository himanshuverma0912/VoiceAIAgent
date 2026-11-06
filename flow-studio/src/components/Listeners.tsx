// Side listeners: they follow what the caller says, alongside the conversation,
// and act when they hear something (flow_engine/listeners.py). Off unless
// someone adds them here; each change goes through review like any other.
import { useEffect, useState } from "react";
import {
  IDENTIFIER_HINT,
  LISTENER_ACTIONS,
  LISTENER_KIND_LABEL,
  LISTENER_PRESETS,
  ONE_PER_FLOW,
  listenerJumpWarnings,
  uniqueId,
  type Flow,
  type Listener,
  type ListenerAction,
} from "../spec";
import { Check, Field, VarText } from "./ui";

const DEFAULT_WARNING = {
  hi: "कृपया अपना ओटीपी, पिन, सीवीवी या कार्ड नंबर किसी के साथ साझा न करें, हमारे साथ भी नहीं।…",
  en: "Please don't share your OTP, PIN, CVV or card number with anyone, not even with us…",
};

export function Listeners(props: {
  flow: Flow;
  editable: boolean;
  vars: { name: string; about: string }[];
  change: (listeners: Listener[]) => void;
}) {
  const listeners = props.flow.settings?.listeners ?? [];
  const nodeIds = Object.keys(props.flow.nodes);
  const hindi = (props.flow.settings?.language ?? "hi").startsWith("hi");
  const warnings = listenerJumpWarnings(props.flow);
  const set = (i: number, next: Listener) => props.change(listeners.map((l, j) => (j === i ? next : l)));
  const remove = (i: number) => props.change(listeners.filter((_, j) => j !== i));
  const add = (presetId: string) => {
    const preset = LISTENER_PRESETS.find((p) => p.id === presetId);
    if (!preset) return;
    const made = preset.make(props.flow);
    made.name = uniqueId(made.name, listeners.map((l) => l.name));
    props.change([...listeners, made]);
  };
  // One of each kind per flow, keyword watches apart.
  const offered = LISTENER_PRESETS.filter((p) => !(ONE_PER_FLOW.includes(p.id as never) && listeners.some((l) => l.kind === p.id)));

  return (
    <div className="listeners">
      {listeners.map((ls, i) => (
        <div key={i} className="listener-card">
          <div className="listener-head">
            <strong className="grow">{LISTENER_KIND_LABEL[ls.kind]}</strong>
            <span className="mono muted">{ls.name}</span>
            {props.editable && (
              <button type="button" className="small danger" aria-label={`Remove ${ls.name}`} onClick={() => remove(i)}>
                Remove
              </button>
            )}
          </div>
          <Field label="Name" hint={`Shown in test calls and call history; ${IDENTIFIER_HINT}.`}>
            <input className="mono" value={ls.name} disabled={!props.editable} onChange={(e) => set(i, { ...ls, name: e.target.value.trim() })} />
          </Field>
          {ls.kind !== "time_limit" && <Steps listener={ls} nodeIds={nodeIds} editable={props.editable} change={(next) => set(i, next)} />}
          {ls.kind === "secret_guard" && (
            <Field label="Warning (optional)" hint={`Empty: “${hindi ? DEFAULT_WARNING.hi : DEFAULT_WARNING.en}”`}>
              <VarText
                value={ls.say ?? ""}
                rows={2}
                vars={props.vars}
                disabled={!props.editable}
                onChange={(v) => set(i, withText(ls, "say", v))}
              />
            </Field>
          )}
          {ls.kind === "keyword_watch" && (
            <KeywordWatch listener={ls} nodeIds={nodeIds} vars={props.vars} editable={props.editable} change={(next) => set(i, next)} />
          )}
          {ls.kind === "time_limit" && (
            <TimeLimitForm listener={ls} nodeIds={nodeIds} vars={props.vars} editable={props.editable} hindi={hindi} change={(next) => set(i, next)} />
          )}
          {ls.kind === "off_topic" && (
            <OffTopicForm listener={ls} nodeIds={nodeIds} vars={props.vars} editable={props.editable} hindi={hindi} change={(next) => set(i, next)} />
          )}
          {ls.kind === "promise_to_pay" && (
            <>
              <div className="row2">
                <Field label="Keep the date as">
                  <input
                    className="mono"
                    value={ls.save_date ?? "promised_date"}
                    disabled={!props.editable}
                    onChange={(e) => set(i, withText(ls, "save_date", e.target.value.trim(), "promised_date"))}
                  />
                </Field>
                <Field label="Keep the amount as">
                  <input
                    className="mono"
                    value={ls.save_amount ?? "promised_amount"}
                    disabled={!props.editable}
                    onChange={(e) => set(i, withText(ls, "save_amount", e.target.value.trim(), "promised_amount"))}
                  />
                </Field>
              </div>
              <p className="hint">
                Later steps can use {`{{${ls.save_date ?? "promised_date"}}}`} and {`{{${ls.save_amount ?? "promised_amount"}}}`}. A small AI check runs
                after each thing the caller says on these steps; the agent's reply doesn't wait for it.
              </p>
            </>
          )}
          {warnings
            .filter((w) => w.listener === i)
            .map((w) => (
              <div key={w.to} className="notice warn" role="note">
                {w.text}
              </div>
            ))}
        </div>
      ))}
      {props.editable && offered.length > 0 && (
        <select
          className="add-listener"
          value=""
          aria-label="Add a side listener"
          onChange={(e) => {
            add(e.target.value);
            e.target.value = "";
          }}
        >
          <option value="">+ Add listener</option>
          {offered.map((p) => (
            <option key={p.id} value={p.id} title={p.about}>
              {p.label}: {p.about}
            </option>
          ))}
        </select>
      )}
    </div>
  );
}

/** Set an optional text key, leaving it out when empty (or at its default). */
function withText<L extends Listener>(ls: L, key: string, value: string, fallback = ""): L {
  const next = { ...ls } as Record<string, unknown>;
  if (value && value !== fallback) next[key] = value;
  else delete next[key];
  return next as L;
}

function Steps(props: { listener: Listener; nodeIds: string[]; editable: boolean; change: (l: Listener) => void }) {
  const ls = props.listener;
  const steps = ls.steps ?? [];
  const every = steps.length === 0;
  const setSteps = (next: string[]) => {
    const out = { ...ls } as Listener;
    if (next.length) out.steps = next;
    else delete out.steps;
    props.change(out);
  };
  return (
    <div className="field">
      <span>Listens on</span>
      <div className="tabs" role="group" aria-label="Listens on">
        <button type="button" aria-pressed={every} disabled={!props.editable} onClick={() => setSteps([])}>
          Every step
        </button>
        <button
          type="button"
          aria-pressed={!every}
          disabled={!props.editable}
          onClick={() => every && props.nodeIds.length && setSteps([props.nodeIds[0]])}
        >
          Only some steps
        </button>
      </div>
      {!every && (
        <div className="step-ticks">
          {props.nodeIds.map((id) => (
            <Check
              key={id}
              label={id}
              checked={steps.includes(id)}
              disabled={!props.editable || (steps.length === 1 && steps[0] === id)}
              onChange={(on) => setSteps(on ? props.nodeIds.filter((n) => n === id || steps.includes(n)) : steps.filter((s) => s !== id))}
            />
          ))}
          {steps
            .filter((s) => !props.nodeIds.includes(s))
            .map((s) => (
              <span key={s} className="chip bad">
                {s} (not a step)
              </span>
            ))}
        </div>
      )}
    </div>
  );
}

function KeywordWatch(props: {
  listener: Extract<Listener, { kind: "keyword_watch" }>;
  nodeIds: string[];
  vars: { name: string; about: string }[];
  editable: boolean;
  change: (l: Listener) => void;
}) {
  const ls = props.listener;
  // One word or phrase per line; blank lines are kept while typing and dropped on leaving the box.
  const [text, setText] = useState(ls.words.join("\n"));
  useEffect(() => {
    if (text.split("\n").map((w) => w.trim()).filter(Boolean).join("\n") !== ls.words.join("\n")) setText(ls.words.join("\n"));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ls.words.join("\n")]);
  const setThen = (then: ListenerAction) => {
    const next = { ...ls, then };
    if (then === "go_to") next.to = ls.to || props.nodeIds[0] || "";
    else delete next.to;
    if (then === "flag") delete next.say;
    props.change(next);
  };
  const sayLabel = ls.then === "say" ? "Line to say" : ls.then === "end_call" ? "Goodbye (optional)" : "Say first (optional)";
  return (
    <>
      <Field label="Listen for" hint="One word or phrase per line, in every language callers use. Whole words, any case.">
        <textarea
          rows={4}
          value={text}
          disabled={!props.editable}
          onChange={(e) => {
            setText(e.target.value);
            props.change({ ...ls, words: e.target.value.split("\n").map((w) => w.trim()).filter(Boolean) });
          }}
        />
      </Field>
      <div className="row2">
        <Field label="Then">
          <select value={ls.then} disabled={!props.editable} onChange={(e) => setThen(e.target.value as ListenerAction)}>
            {LISTENER_ACTIONS.map((a) => (
              <option key={a.id} value={a.id}>
                {a.label}
              </option>
            ))}
          </select>
        </Field>
        {ls.then === "go_to" && (
          <Field label="Step">
            <select value={ls.to ?? ""} disabled={!props.editable} onChange={(e) => props.change({ ...ls, to: e.target.value })}>
              {!ls.to && <option value="">Choose a step</option>}
              {ls.to && !props.nodeIds.includes(ls.to) && <option value={ls.to}>{ls.to} (not a step)</option>}
              {props.nodeIds.map((id) => (
                <option key={id} value={id}>
                  {id}
                </option>
              ))}
            </select>
          </Field>
        )}
      </div>
      {ls.then !== "flag" && (
        <Field label={sayLabel} hint={ls.then === "go_to" ? "The step you go to answers the caller; this line comes first." : undefined}>
          <VarText value={ls.say ?? ""} rows={2} vars={props.vars} disabled={!props.editable} onChange={(v) => props.change(withText(ls, "say", v))} />
        </Field>
      )}
      <Check
        label="Only the first time in a call"
        checked={ls.once ?? true}
        disabled={!props.editable}
        onChange={(on) => {
          const next = { ...ls };
          if (on) delete next.once;
          else next.once = false;
          props.change(next);
        }}
      />
    </>
  );
}

/** A whole number field; empty leaves the key out (its default applies). */
function Count(props: { label: string; hint?: string; value: number | undefined; fallback: number; editable: boolean; set: (n: number | undefined) => void }) {
  return (
    <Field label={props.label} hint={props.hint}>
      <input
        type="number"
        min={0}
        step={1}
        inputMode="numeric"
        value={props.value ?? props.fallback}
        disabled={!props.editable}
        onChange={(e) => {
          const raw = e.target.value.trim();
          props.set(raw === "" ? undefined : Number(raw));
        }}
      />
    </Field>
  );
}

function withNumber<L extends Listener>(ls: L, key: string, value: number | undefined): L {
  const next = { ...ls } as Record<string, unknown>;
  if (value === undefined || Number.isNaN(value)) delete next[key];
  else next[key] = value;
  return next as L;
}

/** "then" and its step, shared by the time limit and the off-topic guard. */
function ThenStep<L extends Extract<Listener, { kind: "time_limit" | "off_topic" }>>(props: {
  label: string;
  listener: L;
  actions: { id: string; label: string }[];
  nodeIds: string[];
  editable: boolean;
  change: (l: L) => void;
}) {
  const ls = props.listener;
  const then = ls.then ?? "end_call";
  return (
    <div className="row2">
      <Field label={props.label}>
        <select
          value={then}
          disabled={!props.editable}
          onChange={(e) => {
            const next = { ...ls, then: e.target.value } as L;
            if (e.target.value === "go_to") next.to = ls.to || props.nodeIds[0] || "";
            else delete next.to;
            if (e.target.value === "flag") delete next.say;
            props.change(next);
          }}
        >
          {props.actions.map((a) => (
            <option key={a.id} value={a.id}>
              {a.label}
            </option>
          ))}
        </select>
      </Field>
      {then === "go_to" && (
        <Field label="Step">
          <select value={ls.to ?? ""} disabled={!props.editable} onChange={(e) => props.change({ ...ls, to: e.target.value })}>
            {!ls.to && <option value="">Choose a step</option>}
            {ls.to && !props.nodeIds.includes(ls.to) && <option value={ls.to}>{ls.to} (not a step)</option>}
            {props.nodeIds.map((id) => (
              <option key={id} value={id}>
                {id}
              </option>
            ))}
          </select>
        </Field>
      )}
    </div>
  );
}

const TIME_UP = {
  hi: "इस कॉल का समय पूरा हो गया है। कॉल करने के लिए धन्यवाद…",
  en: "We've reached the time limit for this call. Thank you for calling…",
};
const OFF_TOPIC_END = {
  hi: "माफ़ कीजिए, इस कॉल पर हम सिर्फ़ इसी काम में मदद कर सकते हैं…",
  en: "I'm sorry, on this call we can only help with what it's for…",
};

function TimeLimitForm(props: {
  listener: Extract<Listener, { kind: "time_limit" }>;
  nodeIds: string[];
  vars: { name: string; about: string }[];
  editable: boolean;
  hindi: boolean;
  change: (l: Listener) => void;
}) {
  const ls = props.listener;
  const goTo = ls.then === "go_to";
  const warn = ls.warn_minutes ?? 2;
  return (
    <>
      <p className="hint" style={{ marginTop: 0 }}>
        Applies to the whole call, from the moment it's answered.
      </p>
      <div className="row2">
        <Count label="Longest call (minutes)" value={ls.minutes} fallback={30} editable={props.editable} set={(n) => props.change(withNumber(ls, "minutes", n))} />
        <Count
          label="Warning before (minutes)"
          hint="0: no warning."
          value={ls.warn_minutes}
          fallback={2}
          editable={props.editable}
          set={(n) => props.change(withNumber(ls, "warn_minutes", n))}
        />
      </div>
      {warn > 0 && (
        <Field label="Warning (optional)" hint="Empty: the agent says it in its own words, in the caller's language.">
          <VarText value={ls.warn_say ?? ""} rows={2} vars={props.vars} disabled={!props.editable} onChange={(v) => props.change(withText(ls, "warn_say", v))} />
        </Field>
      )}
      <ThenStep
        label="At the limit"
        listener={ls}
        actions={[
          { id: "end_call", label: "End the call" },
          { id: "go_to", label: "Go to a step (the call ends 3 minutes later)" },
        ]}
        nodeIds={props.nodeIds}
        editable={props.editable}
        change={props.change}
      />
      <Field
        label={goTo ? "Say first (optional)" : "Last line (optional)"}
        hint={goTo ? "For example: “Let me book a call back for you.”" : `Empty: “${props.hindi ? TIME_UP.hi : TIME_UP.en}”`}
      >
        <VarText value={ls.say ?? ""} rows={2} vars={props.vars} disabled={!props.editable} onChange={(v) => props.change(withText(ls, "say", v))} />
      </Field>
    </>
  );
}

function OffTopicForm(props: {
  listener: Extract<Listener, { kind: "off_topic" }>;
  nodeIds: string[];
  vars: { name: string; about: string }[];
  editable: boolean;
  hindi: boolean;
  change: (l: Listener) => void;
}) {
  const ls = props.listener;
  const then = ls.then ?? "end_call";
  const thenAfter = ls.then_after ?? 4;
  return (
    <>
      <Field label="What the call is about (optional)" hint="Empty: the flow's name, persona and the step the call is on.">
        <textarea rows={2} value={ls.topic ?? ""} disabled={!props.editable} onChange={(e) => props.change(withText(ls, "topic", e.target.value))} />
      </Field>
      <div className="row2">
        <Count
          label="Bring them back after"
          hint="Off-topic things said in a row."
          value={ls.steer_after}
          fallback={2}
          editable={props.editable}
          set={(n) => props.change(withNumber(ls, "steer_after", n))}
        />
        <Count
          label="Then act after"
          hint="In a row; 0: never."
          value={ls.then_after}
          fallback={4}
          editable={props.editable}
          set={(n) => props.change(withNumber(ls, "then_after", n))}
        />
      </div>
      {thenAfter > 0 && (
        <>
          <ThenStep
            label="Then"
            listener={ls}
            actions={[
              { id: "end_call", label: "End the call" },
              { id: "go_to", label: "Go to a step" },
              { id: "flag", label: "Flag the call" },
            ]}
            nodeIds={props.nodeIds}
            editable={props.editable}
            change={props.change}
          />
          {then !== "flag" && (
            <Field
              label={then === "go_to" ? "Say first (optional)" : "Last line (optional)"}
              hint={then === "go_to" ? undefined : `Empty: “${props.hindi ? OFF_TOPIC_END.hi : OFF_TOPIC_END.en}”`}
            >
              <VarText value={ls.say ?? ""} rows={2} vars={props.vars} disabled={!props.editable} onChange={(v) => props.change(withText(ls, "say", v))} />
            </Field>
          )}
        </>
      )}
      <p className="hint">
        A small AI check after each thing the caller says on these steps; replies of a word or two count as on topic. The agent's reply doesn't
        wait for it.
      </p>
    </>
  );
}

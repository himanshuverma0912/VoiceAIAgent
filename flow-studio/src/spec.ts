// The flow format (schema version 1), as the voice agent runs it.
// flow_engine/spec.py and config-service/internal/flowspec define and check it;
// this file only gives the editor types and helpers. The server is the judge of
// what's valid (POST /v1/validate), so nothing here duplicates those rules.

export type ValueType = "string" | "number" | "integer" | "boolean";
export const VALUE_TYPES: ValueType[] = ["string", "number", "integer", "boolean"];

export interface CaptureField {
  type?: ValueType;
  description: string;
  enum?: string[] | null;
  required?: boolean;
}

export interface Transition {
  to: string;
  when: string;
  name?: string | null;
  capture?: Record<string, CaptureField>;
}

export type BuiltinKind = "name" | "phone" | "email" | "address" | "date_of_birth" | "digits";
export const BUILTINS: { kind: BuiltinKind; label: string }[] = [
  { kind: "name", label: "Name" },
  { kind: "phone", label: "Phone number" },
  { kind: "email", label: "Email" },
  { kind: "address", label: "Address" },
  { kind: "date_of_birth", label: "Date of birth" },
  { kind: "digits", label: "Digits (e.g. last 4 of a card)" },
];

export interface CollectField {
  name: string;
  builtin?: BuiltinKind;
  instructions?: string;
  options?: { last_name?: boolean; verify_spelling?: boolean; num_digits?: number; ask_for_confirmation?: boolean };
  type?: ValueType;
  description?: string;
  enum?: string[] | null;
}

export interface AgentNode {
  type: "agent";
  instructions: string;
  transitions?: Transition[];
  on_enter?: "speak" | "wait";
}

export interface CollectNode {
  type: "collect";
  instructions?: string;
  fields: CollectField[];
  next: string;
}

export interface EndNode {
  type: "end";
  instructions: string;
  goodbye?: string;
}

export type FlowNode = AgentNode | CollectNode | EndNode;
export type NodeType = FlowNode["type"];

export interface FlowSettings {
  language?: string;
  llm_model?: string | null;
  llm_temperature?: number;
  stt_model?: string | null;
  tts_voice_id?: string | null;
}

export interface Flow {
  schema_version: 1;
  flow_id: string;
  name: string;
  persona: string;
  guardrails?: string[];
  greeting?: string | null;
  allow_end_call_anywhere?: boolean;
  settings?: FlowSettings;
  start_node: string;
  nodes: Record<string, FlowNode>;
}

export const BUILTIN_VARIABLES: { name: string; about: string }[] = [
  { name: "caller_number", about: "The caller's phone number" },
  { name: "dialed_number", about: "The number the caller dialled" },
  { name: "now_ist", about: "Current date and time in IST" },
  { name: "today_ist", about: "Today's date in IST" },
];

export const IDENTIFIER_RE = /^[a-z][a-z0-9_]{0,47}$/;
export const IDENTIFIER_HINT = "lower-case letters, digits and _, starting with a letter";

export const NODE_TYPE_LABEL: Record<NodeType, string> = {
  agent: "Conversation step",
  collect: "Collect details",
  end: "End call",
};

/** True when the value has the overall shape the editor needs (nodes object etc). */
export function isEditable(value: unknown): value is Flow {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const nodes = (value as Flow).nodes;
  if (!nodes || typeof nodes !== "object" || Array.isArray(nodes)) return false;
  return Object.values(nodes).every(
    (n) => n && typeof n === "object" && ["agent", "collect", "end"].includes((n as FlowNode).type),
  );
}

export function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

/** The flow as saved: 2-space JSON. Fields set to undefined disappear. */
export function serialize(flow: unknown): string {
  return JSON.stringify(flow, null, 2) + "\n";
}

export function toolName(t: Transition): string {
  return t.name || `go_to_${t.to}`;
}

/** A small, valid flow to start from. */
export function newFlowTemplate(flowId: string, name: string): Flow {
  return {
    schema_version: 1,
    flow_id: flowId,
    name,
    persona:
      "You are a friendly, concise phone assistant. Speak in the caller's language and keep each answer to one or two short sentences.",
    guardrails: ["Never ask for a full card number, PIN, OTP or password."],
    greeting: "Hello! How can I help you today?",
    allow_end_call_anywhere: true,
    settings: { language: "hi", llm_model: "voice-fast", llm_temperature: 0 },
    start_node: "listen",
    nodes: {
      listen: {
        type: "agent",
        on_enter: "wait",
        instructions: "Listen to what the caller needs and answer briefly.",
        transitions: [{ to: "goodbye", when: "The caller has nothing more to ask." }],
      },
      goodbye: { type: "end", instructions: "Thank the caller for calling." },
    },
  };
}

export function newNode(type: NodeType, otherIds: string[]): FlowNode {
  const target = otherIds[0] ?? "";
  switch (type) {
    case "agent":
      return { type: "agent", instructions: "Describe what the agent does in this step.", transitions: [], on_enter: "speak" };
    case "collect":
      return {
        type: "collect",
        instructions: "",
        fields: [{ name: "full_name", builtin: "name" }],
        next: target,
      };
    case "end":
      return { type: "end", instructions: "Thank the caller and say what happens next." };
  }
}

export function uniqueId(base: string, taken: Iterable<string>): string {
  const used = new Set(taken);
  if (!used.has(base)) return base;
  for (let i = 2; ; i++) if (!used.has(`${base}_${i}`)) return `${base}_${i}`;
}

/** Variables usable as {{name}}: built-ins plus every captured or collected value. */
export function variablesOf(flow: Flow): { name: string; about: string }[] {
  const out = [...BUILTIN_VARIABLES];
  const seen = new Set(out.map((v) => v.name));
  for (const [id, node] of Object.entries(flow.nodes)) {
    const add = (name: string, about: string) => {
      if (name && !seen.has(name)) {
        seen.add(name);
        out.push({ name, about });
      }
    };
    if (node.type === "agent") {
      for (const t of node.transitions ?? [])
        for (const [name, c] of Object.entries(t.capture ?? {})) add(name, c.description || `captured in ${id}`);
    } else if (node.type === "collect") {
      for (const f of node.fields ?? []) add(f.name, f.description || `collected in ${id}`);
    }
  }
  return out;
}

export interface Edge {
  id: string;
  from: string;
  to: string;
  label: string;
  transition?: number; // index in the agent node's transitions
}

export function edgesOf(flow: Flow): Edge[] {
  const edges: Edge[] = [];
  for (const [id, node] of Object.entries(flow.nodes)) {
    if (node.type === "agent") {
      (node.transitions ?? []).forEach((t, i) => {
        edges.push({ id: `t:${id}:${i}`, from: id, to: t.to, label: t.when || toolName(t), transition: i });
      });
    } else if (node.type === "collect" && node.next) {
      edges.push({ id: `n:${id}`, from: id, to: node.next, label: "when all details are collected" });
    }
  }
  return edges;
}

/** Rename a node, keeping its place in the file and fixing every reference. */
export function renameNode(flow: Flow, oldId: string, newId: string): Flow {
  const nodes: Record<string, FlowNode> = {};
  for (const [id, node] of Object.entries(flow.nodes)) nodes[id === oldId ? newId : id] = node;
  for (const node of Object.values(nodes)) {
    if (node.type === "agent") for (const t of node.transitions ?? []) if (t.to === oldId) t.to = newId;
    if (node.type === "collect" && node.next === oldId) node.next = newId;
  }
  return { ...flow, nodes, start_node: flow.start_node === oldId ? newId : flow.start_node };
}

/** Delete a node and the transitions into it. A collect step that led to it is
 * left pointing nowhere, so validation asks the editor where it should go. */
export function deleteNode(flow: Flow, id: string): Flow {
  const nodes: Record<string, FlowNode> = {};
  for (const [nid, node] of Object.entries(flow.nodes)) {
    if (nid === id) continue;
    if (node.type === "agent") nodes[nid] = { ...node, transitions: (node.transitions ?? []).filter((t) => t.to !== id) };
    else if (node.type === "collect" && node.next === id) nodes[nid] = { ...node, next: "" };
    else nodes[nid] = node;
  }
  return { ...flow, nodes };
}

export interface ProblemsByPlace {
  byNode: Record<string, string[]>;
  general: string[];
}

/** "nodes.register.transitions[0].to: ..." belongs to node "register". */
export function groupProblems(problems: string[], nodeIds: string[]): ProblemsByPlace {
  const ids = new Set(nodeIds);
  const out: ProblemsByPlace = { byNode: {}, general: [] };
  for (const p of problems) {
    const m = /^nodes\.([^.[:\s]+)/.exec(p);
    if (m && ids.has(m[1])) (out.byNode[m[1]] ??= []).push(p);
    else out.general.push(p);
  }
  return out;
}

/** The part of a problem after its location, for showing next to a form field. */
export function problemText(problem: string): { where: string; what: string } {
  const i = problem.indexOf(": ");
  return i < 0 ? { where: "", what: problem } : { where: problem.slice(0, i), what: problem.slice(i + 2) };
}

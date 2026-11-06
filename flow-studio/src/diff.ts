// What changed between two flows, as a list a reviewer can read: one entry per
// changed field, and one per added or removed node.
import { LISTENER_KIND_LABEL, NODE_TYPE_LABEL, type FlowNode, type Listener } from "./spec";

export interface Change {
  path: string; // e.g. "nodes.register.instructions", "guardrails[1]"
  kind: "added" | "removed" | "changed";
  before?: string;
  after?: string;
}

type Leaves = Map<string, string>;

function flatten(value: unknown, path: string, out: Leaves): void {
  if (Array.isArray(value)) {
    if (value.length === 0) out.set(path, "[]");
    value.forEach((v, i) => flatten(v, `${path}[${i}]`, out));
  } else if (value && typeof value === "object") {
    const entries = Object.entries(value as Record<string, unknown>);
    if (entries.length === 0) out.set(path, "{}");
    for (const [k, v] of entries) flatten(v, path ? `${path}.${k}` : k, out);
  } else {
    out.set(path, typeof value === "string" ? value : JSON.stringify(value));
  }
}

function listenersOf(flow: unknown): Record<string, Listener> {
  const list = (flow as { settings?: { listeners?: unknown } })?.settings?.listeners;
  const out: Record<string, Listener> = {};
  if (Array.isArray(list)) for (const l of list) if (l && typeof l === "object" && typeof l.name === "string") out[l.name] = l as Listener;
  return out;
}

/** A listener in one line, for a review: "Keyword watch fraud_watch: go to dispute". */
function describeListener(l: Listener): string {
  const kind = LISTENER_KIND_LABEL[l.kind] ?? l.kind;
  const steps = l.steps?.length ? ` on ${l.steps.join(", ")}` : " on every step";
  const thenText = (then?: string, to?: string | null) =>
    then === "go_to" ? `go to ${to}` : then === "say" ? "say a line" : then === "flag" ? "flag the call" : "end the call";
  if (l.kind === "keyword_watch") return `${kind} ${l.name}${steps}: “${(l.words ?? []).join("”, “")}” → ${thenText(l.then, l.to)}`;
  if (l.kind === "time_limit")
    return `${kind} ${l.name}: ${l.minutes ?? 30} minutes (warning ${l.warn_minutes ?? 2} before) → ${thenText(l.then, l.to)}`;
  if (l.kind === "off_topic") {
    const after = l.then_after ?? 4;
    return `${kind} ${l.name}${steps}: brings the caller back after ${l.steer_after ?? 2} off-topic turns${after ? `, after ${after} → ${thenText(l.then, l.to)}` : ""}`;
  }
  return `${kind} ${l.name}${steps}`;
}

function nodesOf(flow: unknown): Record<string, FlowNode> {
  const nodes = (flow as { nodes?: unknown })?.nodes;
  return nodes && typeof nodes === "object" && !Array.isArray(nodes) ? (nodes as Record<string, FlowNode>) : {};
}

export function changesBetween(before: unknown, after: unknown): Change[] {
  const changes: Change[] = [];
  const oldNodes = nodesOf(before);
  const newNodes = nodesOf(after);
  const label = (n: FlowNode) => NODE_TYPE_LABEL[n?.type] ?? "node";

  const oldListeners = listenersOf(before);
  const newListeners = listenersOf(after);

  // Whole nodes and listeners added or removed: one entry each, not one per
  // field. Listeners are compared by name, so reordering them changes nothing.
  const strip = (flow: unknown, others: Record<string, FlowNode>, otherListeners: Record<string, Listener>) => {
    if (!flow || typeof flow !== "object") return flow;
    const nodes: Record<string, FlowNode> = {};
    for (const [id, n] of Object.entries(nodesOf(flow))) if (id in others) nodes[id] = n;
    const out = { ...(flow as object), nodes } as { settings?: Record<string, unknown> };
    if (out.settings && "listeners" in out.settings) {
      const kept: Record<string, Listener> = {};
      for (const [name, l] of Object.entries(listenersOf(flow))) if (name in otherListeners) kept[name] = l;
      const settings = { ...out.settings };
      delete settings.listeners;
      if (Object.keys(kept).length) settings.listeners = kept;
      out.settings = settings;
    }
    return out;
  };
  for (const name of Object.keys(oldListeners))
    if (!(name in newListeners)) changes.push({ path: `settings.listeners.${name}`, kind: "removed", before: describeListener(oldListeners[name]) });
  for (const name of Object.keys(newListeners))
    if (!(name in oldListeners)) changes.push({ path: `settings.listeners.${name}`, kind: "added", after: describeListener(newListeners[name]) });
  for (const id of Object.keys(oldNodes))
    if (!(id in newNodes)) changes.push({ path: `nodes.${id}`, kind: "removed", before: label(oldNodes[id]) });
  for (const id of Object.keys(newNodes))
    if (!(id in oldNodes)) changes.push({ path: `nodes.${id}`, kind: "added", after: label(newNodes[id]) });

  const a: Leaves = new Map();
  const b: Leaves = new Map();
  if (before != null) flatten(strip(before, newNodes, newListeners), "", a);
  if (after != null) flatten(strip(after, oldNodes, oldListeners), "", b);
  for (const [path, value] of b) {
    if (!a.has(path)) changes.push({ path, kind: "added", after: value });
    else if (a.get(path) !== value) changes.push({ path, kind: "changed", before: a.get(path), after: value });
  }
  for (const [path, value] of a) if (!b.has(path)) changes.push({ path, kind: "removed", before: value });
  return changes.filter((c) => c.path !== "nodes" || c.kind === "changed");
}

/** "nodes.register.transitions[0].when" -> "register › transitions 1 › when" */
export function readablePath(path: string): string {
  return path
    .replace(/^nodes\./, "")
    .replace(/\[(\d+)\]/g, (_, i) => ` ${Number(i) + 1}`)
    .split(".")
    .join(" › ");
}

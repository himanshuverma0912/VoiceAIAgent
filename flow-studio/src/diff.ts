// What changed between two flows, as a list a reviewer can read: one entry per
// changed field, and one per added or removed node.
import { NODE_TYPE_LABEL, type FlowNode } from "./spec";

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

function nodesOf(flow: unknown): Record<string, FlowNode> {
  const nodes = (flow as { nodes?: unknown })?.nodes;
  return nodes && typeof nodes === "object" && !Array.isArray(nodes) ? (nodes as Record<string, FlowNode>) : {};
}

export function changesBetween(before: unknown, after: unknown): Change[] {
  const changes: Change[] = [];
  const oldNodes = nodesOf(before);
  const newNodes = nodesOf(after);
  const label = (n: FlowNode) => NODE_TYPE_LABEL[n?.type] ?? "node";

  // Whole nodes added or removed: one entry each, not one per field.
  const strip = (flow: unknown, others: Record<string, FlowNode>) => {
    if (!flow || typeof flow !== "object") return flow;
    const nodes: Record<string, FlowNode> = {};
    for (const [id, n] of Object.entries(nodesOf(flow))) if (id in others) nodes[id] = n;
    return { ...(flow as object), nodes };
  };
  for (const id of Object.keys(oldNodes))
    if (!(id in newNodes)) changes.push({ path: `nodes.${id}`, kind: "removed", before: label(oldNodes[id]) });
  for (const id of Object.keys(newNodes))
    if (!(id in oldNodes)) changes.push({ path: `nodes.${id}`, kind: "added", after: label(newNodes[id]) });

  const a: Leaves = new Map();
  const b: Leaves = new Map();
  if (before != null) flatten(strip(before, newNodes), "", a);
  if (after != null) flatten(strip(after, oldNodes), "", b);
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

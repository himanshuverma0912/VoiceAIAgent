// Where nodes sit on the canvas. Saved positions win; nodes without one are
// placed by dagre (left to right, following the transitions).
import dagre from "@dagrejs/dagre";
import { edgesOf, type Flow } from "./spec";

export type Positions = Record<string, { x: number; y: number }>;
export const NODE_WIDTH = 260;
export const NODE_HEIGHT = 112;

export function autoLayout(flow: Flow): Positions {
  const g = new dagre.graphlib.Graph();
  g.setGraph({ rankdir: "LR", nodesep: 48, ranksep: 110, marginx: 24, marginy: 24 });
  g.setDefaultEdgeLabel(() => ({}));
  for (const id of Object.keys(flow.nodes)) g.setNode(id, { width: NODE_WIDTH, height: NODE_HEIGHT });
  for (const e of edgesOf(flow)) if (flow.nodes[e.to] && e.to !== e.from) g.setEdge(e.from, e.to);
  dagre.layout(g);
  const out: Positions = {};
  for (const id of Object.keys(flow.nodes)) {
    const n = g.node(id);
    out[id] = { x: Math.round(n.x - NODE_WIDTH / 2), y: Math.round(n.y - NODE_HEIGHT / 2) };
  }
  return out;
}

/** Saved positions for known nodes; new nodes go to the right of everything. */
export function positionsFor(flow: Flow, saved: Positions | undefined): Positions {
  const ids = Object.keys(flow.nodes);
  const known = ids.filter((id) => saved?.[id]);
  if (!saved || known.length === 0) return autoLayout(flow);
  const out: Positions = {};
  for (const id of known) out[id] = saved[id];
  const maxX = Math.max(...known.map((id) => saved[id].x));
  let y = Math.min(...known.map((id) => saved[id].y));
  for (const id of ids) {
    if (out[id]) continue;
    out[id] = { x: maxX + NODE_WIDTH + 80, y };
    y += NODE_HEIGHT + 40;
  }
  return out;
}

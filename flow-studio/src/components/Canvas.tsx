import {
  Background,
  Controls,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  applyNodeChanges,
  type Connection,
  type Edge as RFEdge,
  type Node as RFNode,
  type NodeChange,
  type NodeProps,
} from "@xyflow/react";
import { memo, useEffect, useMemo, useState } from "react";
import type { Positions } from "../layout";
import { NODE_TYPE_LABEL, edgesOf, type Flow, type FlowNode } from "../spec";

export type Selection =
  | { kind: "settings" }
  | { kind: "node"; id: string; transition?: number };

type CardData = { id: string; node: FlowNode; isStart: boolean; problems: number; active: boolean };

const COLORS: Record<FlowNode["type"], string> = {
  agent: "var(--agent)",
  collect: "var(--collect)",
  api: "var(--api)",
  end: "var(--end)",
};

function summary(node: FlowNode): string {
  if (node.type === "collect") {
    const names = (node.fields ?? []).map((f) => f.name).filter(Boolean);
    return names.length ? `Asks for ${names.join(", ")}` : "No fields yet";
  }
  if (node.type === "api") {
    if (!node.tool) return "No tool chosen yet";
    const saves = node.save?.length ? ` → ${node.save.join(", ")}` : "";
    return `Calls ${node.tool}${saves}`;
  }
  return node.instructions || "No instructions yet";
}

function footer(node: FlowNode): string {
  if (node.type === "agent") {
    const n = node.transitions?.length ?? 0;
    const tools = node.tools?.length ? ` · ${node.tools.length} tool${node.tools.length === 1 ? "" : "s"}` : "";
    return `${node.on_enter === "wait" ? "listens first" : "speaks first"} · ${n} way${n === 1 ? "" : "s"} out${tools}`;
  }
  if (node.type === "api") return node.verifies ? "verifies the caller" : "runs by itself";
  if (node.type === "collect") return `${node.fields?.length ?? 0} field${node.fields?.length === 1 ? "" : "s"}`;
  return "hangs up after";
}

const Card = memo(function Card({ data, selected }: NodeProps<RFNode<CardData>>) {
  const { node, id, isStart, problems, active } = data;
  return (
    <div
      className={`fs-node${selected ? " selected" : ""}${problems ? " has-problems" : ""}${active ? " active" : ""}`}
      style={{ "--node-color": COLORS[node.type] } as React.CSSProperties}
    >
      <Handle type="target" position={Position.Left} />
      <div className="kind">
        {NODE_TYPE_LABEL[node.type]}
        {isStart && <span className="chip" style={{ textTransform: "none", letterSpacing: 0 }}>start</span>}
        {active && <span className="chip good" style={{ textTransform: "none", letterSpacing: 0 }}>● now</span>}
      </div>
      <div className="id">{id}</div>
      <div className="text">{summary(node)}</div>
      <div className="foot">
        <span className="grow">{footer(node)}</span>
        {problems > 0 && <span className="chip bad">{problems} problem{problems === 1 ? "" : "s"}</span>}
      </div>
      {node.type !== "end" && <Handle type="source" position={Position.Right} />}
    </div>
  );
});

const nodeTypes = { card: Card };

const short = (s: string) => (s.length > 34 ? s.slice(0, 33) + "…" : s);

export function Canvas(props: {
  flow: Flow;
  positions: Positions;
  problemsByNode: Record<string, string[]>;
  selection: Selection;
  activeNode?: string | null; // the step a test call is in
  editable: boolean;
  onSelect: (s: Selection) => void;
  onMove: (id: string, pos: { x: number; y: number }) => void;
  onConnect: (from: string, to: string) => void;
}) {
  const { flow, positions, problemsByNode, selection, editable } = props;
  const selectedNode = selection.kind === "node" ? selection.id : null;
  const [nodes, setNodes] = useState<RFNode<CardData>[]>([]);

  // Rebuild cards from the flow, keeping what React Flow measured.
  useEffect(() => {
    setNodes((prev) => {
      const old = new Map(prev.map((n) => [n.id, n]));
      return Object.entries(flow.nodes).map(([id, node]) => ({
        ...old.get(id),
        id,
        type: "card",
        position: positions[id] ?? old.get(id)?.position ?? { x: 0, y: 0 },
        selected: id === selectedNode,
        data: {
          id,
          node,
          isStart: id === flow.start_node,
          problems: problemsByNode[id]?.length ?? 0,
          active: id === props.activeNode,
        },
      }));
    });
  }, [flow, positions, problemsByNode, selectedNode, props.activeNode]);

  const edges = useMemo<RFEdge[]>(
    () =>
      edgesOf(flow)
        .filter((e) => flow.nodes[e.to])
        .map((e) => ({
          id: e.id,
          source: e.from,
          target: e.to,
          label: short(e.label),
          selected: selection.kind === "node" && selection.id === e.from && selection.transition === e.transition && e.transition !== undefined,
          markerEnd: { type: MarkerType.ArrowClosed, width: 18, height: 18 },
          style: e.kind === "error" ? { strokeWidth: 1.6, strokeDasharray: "5 4", stroke: "var(--bad)" } : { strokeWidth: 1.6 },
          data: { transition: e.transition },
        })),
    [flow, selection],
  );

  const onNodesChange = (changes: NodeChange<RFNode<CardData>>[]) => {
    setNodes((ns) => applyNodeChanges(changes, ns));
    for (const c of changes) {
      if (c.type === "position" && c.position && c.dragging === false) props.onMove(c.id, c.position);
    }
  };

  return (
    <ReactFlow
      nodes={nodes}
      edges={edges}
      nodeTypes={nodeTypes}
      onNodesChange={onNodesChange}
      onNodeDragStop={(_, n) => props.onMove(n.id, n.position)}
      onNodeClick={(_, n) => props.onSelect({ kind: "node", id: n.id })}
      onEdgeClick={(_, e) =>
        props.onSelect({ kind: "node", id: e.source, transition: (e.data as { transition?: number })?.transition })
      }
      onPaneClick={() => props.onSelect({ kind: "settings" })}
      onConnect={(c: Connection) => c.source && c.target && c.source !== c.target && props.onConnect(c.source, c.target)}
      nodesDraggable={editable}
      nodesConnectable={editable}
      deleteKeyCode={null}
      fitView
      fitViewOptions={{ padding: 0.2, maxZoom: 1 }}
      minZoom={0.25}
      proOptions={{ hideAttribution: true }}
    >
      <Background gap={20} size={1} />
      <Controls showInteractive={false} />
    </ReactFlow>
  );
}

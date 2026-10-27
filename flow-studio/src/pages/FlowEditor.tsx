import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ApiError, api, can, offeredTools, type CatalogEntry, type FlowSummary, type Integration, type Me } from "../api";
import { Canvas, type Selection } from "../components/Canvas";
import { NodeEditor } from "../components/NodeEditor";
import { ReviewDialog, SubmitDialog, VersionsDialog } from "../components/Review";
import { SettingsEditor } from "../components/SettingsEditor";
import { TestCall } from "../components/TestCall";
import { timeAgo, toast } from "../components/ui";
import { NODE_WIDTH, autoLayout, positionsFor, type Positions } from "../layout";
import { Link, navigate, setLeaveGuard } from "../router";
import { workspaceNames } from "./FlowList";
import {
  NODE_TYPE_LABEL,
  clone,
  deleteNode,
  groupProblems,
  isEditable,
  newNode,
  problemText,
  renameNode,
  serialize,
  uniqueId,
  variablesOf,
  type Flow,
  type FlowNode,
  type NodeType,
} from "../spec";

const NEW_ID: Record<NodeType, string> = { agent: "step", collect: "collect_details", api: "api_call", end: "end_call" };

export function FlowEditor({ me, flowId }: { me: Me; flowId: string }) {
  const [summary, setSummary] = useState<FlowSummary | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [spec, setSpec] = useState<unknown>(null);
  const [checksum, setChecksum] = useState("");
  const [positions, setPositions] = useState<Positions>({});
  const [dirty, setDirty] = useState(false);
  const [layoutDirty, setLayoutDirty] = useState(false);
  const [mode, setMode] = useState<"canvas" | "json">("canvas");
  const [jsonText, setJsonText] = useState("");
  const [jsonError, setJsonError] = useState<string | null>(null);
  const [problems, setProblems] = useState<string[]>([]);
  const [selection, setSelection] = useState<Selection>({ kind: "settings" });
  const [saving, setSaving] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [dialog, setDialog] = useState<null | "submit" | "review" | "versions">(null);
  const [testing, setTesting] = useState(false);
  const [activeNode, setActiveNode] = useState<string | null>(null);
  const [catalog, setCatalog] = useState<CatalogEntry[] | null>(null);
  const [integrations, setIntegrations] = useState<Integration[] | null>(null);

  useEffect(() => {
    api.catalog().then(setCatalog, () => setCatalog([]));
    api.integrations().then(setIntegrations, () => setIntegrations([]));
  }, []);

  // ── load ──
  useEffect(() => {
    Promise.all([api.flow(flowId), api.draft(flowId)]).then(
      ([s, d]) => {
        setSummary(s);
        setSpec(d.spec);
        setChecksum(d.checksum);
        setProblems(d.problems);
        if (isEditable(d.spec)) setPositions(positionsFor(d.spec, d.layout?.positions));
        else {
          setMode("json");
          setJsonText(serialize(d.spec));
        }
      },
      (e) => setLoadError(e instanceof ApiError && e.status === 404 ? "This flow doesn't exist, or it's in a workspace you aren't in." : (e as Error).message),
    );
  }, [flowId]);

  const editable = !!summary && can.edit(me, summary.workspace);
  const checker = !!summary && can.approve(me, summary.workspace);
  const pending = summary?.pending_review ?? null;
  const mySubmission = !!pending && pending.submitted_by === me.email;
  const canDecide = !!pending && checker && !mySubmission;
  const flow = isEditable(spec) ? spec : null;
  const unsaved = dirty || layoutDirty;

  // ── unsaved changes: warn before leaving ──
  useEffect(() => {
    setLeaveGuard(unsaved ? () => confirm("You have unsaved changes. Leave without saving?") : null);
    const onUnload = (e: BeforeUnloadEvent) => {
      if (unsaved) e.preventDefault();
    };
    window.addEventListener("beforeunload", onUnload);
    return () => {
      setLeaveGuard(null);
      window.removeEventListener("beforeunload", onUnload);
    };
  }, [unsaved]);

  // ── check as you type (the server's rules, debounced) ──
  const checkSeq = useRef(0);
  useEffect(() => {
    if (spec === null || !dirty) return;
    const text = mode === "json" ? jsonText : serialize(spec);
    const seq = ++checkSeq.current;
    const timer = setTimeout(async () => {
      try {
        const res = await api.validate(text, summary?.workspace || undefined);
        if (seq === checkSeq.current) setProblems(res.problems);
      } catch (e) {
        if (seq === checkSeq.current) setProblems([`(top level): ${(e as Error).message}`]);
      }
    }, 450);
    return () => clearTimeout(timer);
  }, [spec, jsonText, mode, dirty, summary?.workspace]);

  // ── editing ──
  /** Change the flow in place: fn gets a copy to modify (its return value is ignored). */
  const edit = useCallback(
    (fn: (f: Flow) => unknown) => {
      if (!editable) return;
      setSpec((prev: unknown) => {
        const draft = clone(prev as Flow);
        fn(draft);
        return draft;
      });
      setDirty(true);
    },
    [editable],
  );

  /** Replace the whole flow with what fn returns (rename, delete). */
  const replace = useCallback(
    (fn: (f: Flow) => Flow) => {
      if (!editable) return;
      setSpec((prev: unknown) => fn(clone(prev as Flow)));
      setDirty(true);
    },
    [editable],
  );

  const changeNode = (id: string) => (fn: (n: FlowNode) => unknown) =>
    edit((f) => {
      fn(f.nodes[id]);
    });

  const rename = (oldId: string, newId: string) => {
    replace((f) => renameNode(f, oldId, newId));
    setPositions((p) => {
      const { [oldId]: pos, ...rest } = p;
      return pos ? { ...rest, [newId]: pos } : rest;
    });
    setLayoutDirty(true);
    setSelection({ kind: "node", id: newId });
  };

  const remove = (id: string) => {
    replace((f) => deleteNode(f, id));
    setSelection({ kind: "settings" });
  };

  const addNode = (type: NodeType) => {
    if (!flow) return;
    const id = uniqueId(NEW_ID[type], Object.keys(flow.nodes));
    const near = selection.kind === "node" ? positions[selection.id] : undefined;
    const xs = Object.values(positions);
    const pos = near
      ? { x: near.x + NODE_WIDTH + 80, y: near.y + 30 }
      : { x: xs.length ? Math.max(...xs.map((p) => p.x)) + NODE_WIDTH + 80 : 40, y: xs.length ? Math.min(...xs.map((p) => p.y)) : 40 };
    edit((f) => {
      f.nodes[id] = newNode(type, Object.keys(f.nodes).filter((n) => f.nodes[n].type === "end"));
    });
    setPositions((p) => ({ ...p, [id]: pos }));
    setLayoutDirty(true);
    setSelection({ kind: "node", id });
  };

  const connect = (from: string, to: string) => {
    if (!flow) return;
    const node = flow.nodes[from];
    if (node.type === "agent") {
      const index = node.transitions?.length ?? 0;
      edit((f) => {
        const n = f.nodes[from];
        if (n.type === "agent") n.transitions = [...(n.transitions ?? []), { to, when: "" }];
      });
      setSelection({ kind: "node", id: from, transition: index });
      toast("Added a way out. Describe when to take it.");
    } else if (node.type === "collect") {
      edit((f) => {
        const n = f.nodes[from];
        if (n.type === "collect") n.next = to;
      });
      setSelection({ kind: "node", id: from });
    } else if (node.type === "api") {
      // The first link is "when it works"; the next one "if it fails".
      const failure = !!node.next && node.next !== to;
      edit((f) => {
        const n = f.nodes[from];
        if (n.type === "api") {
          if (failure) n.on_error = to;
          else n.next = to;
        }
      });
      setSelection({ kind: "node", id: from });
      toast(failure ? "Set where to go if the call fails." : "Set where to go when the call works.");
    }
  };

  const move = (id: string, pos: { x: number; y: number }) => {
    setPositions((p) => (p[id]?.x === pos.x && p[id]?.y === pos.y ? p : { ...p, [id]: pos }));
    if (editable) setLayoutDirty(true);
  };

  // ── canvas / JSON ──
  const toJson = () => {
    setJsonText(serialize(spec));
    setJsonError(null);
    setMode("json");
  };
  const toCanvas = () => {
    if (jsonError) return toast("Fix the JSON first: " + jsonError);
    if (!isEditable(spec)) return toast("The canvas needs a 'nodes' object whose nodes have type agent, collect or end.");
    setPositions((p) => positionsFor(spec, p));
    setMode("canvas");
  };
  const typeJson = (text: string) => {
    setJsonText(text);
    setDirty(true);
    try {
      setSpec(JSON.parse(text));
      setJsonError(null);
    } catch (e) {
      setJsonError((e as Error).message);
    }
  };

  // ── save ──
  const save = useCallback(async (): Promise<boolean> => {
    if (!editable || saving) return false;
    if (mode === "json" && jsonError) {
      toast("Fix the JSON first: " + jsonError);
      return false;
    }
    setSaving(true);
    try {
      if (dirty) {
        const res = await api.saveDraft(flowId, mode === "json" ? jsonText : serialize(spec), { ifMatch: checksum });
        setChecksum(res.checksum);
        setProblems(res.problems);
        setDirty(false);
        setSummary((s) => (s ? { ...s, draft_valid: res.valid, draft_updated_at: new Date().toISOString() } : s));
        toast(res.valid ? "Draft saved" : `Draft saved with ${res.problems.length} problem${res.problems.length === 1 ? "" : "s"}`);
      }
      if (layoutDirty && isEditable(spec)) {
        await api.saveLayout(flowId, positions);
        setLayoutDirty(false);
        if (!dirty) toast("Layout saved");
      }
      return true;
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) setConflict(true);
      else toast((e as Error).message);
      return false;
    } finally {
      setSaving(false);
    }
  }, [editable, saving, mode, jsonError, dirty, layoutDirty, flowId, jsonText, spec, checksum, positions]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "s") {
        e.preventDefault();
        save();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [save]);

  // "Talk to this draft": the agent runs the saved draft, so save first.
  const startTest = async () => {
    if (unsaved && !(await save())) return;
    if (problems.length) {
      toast("Fix the draft's problems first; the agent can't run it yet.");
      return;
    }
    setTesting(true);
  };

  const openSubmit = async () => {
    if (unsaved && !(await save())) return;
    setDialog("submit");
  };

  /** After a review is decided or withdrawn: fetch the flow's state again. */
  const refreshSummary = () => api.flow(flowId).then(setSummary, () => {});

  // ── derived ──
  const grouped = useMemo(() => groupProblems(problems, flow ? Object.keys(flow.nodes) : []), [problems, flow]);
  const vars = useMemo(() => (flow ? variablesOf(flow) : []), [flow]);
  const selectedNode = selection.kind === "node" && flow?.nodes[selection.id] ? selection : null;
  const tools = useMemo(
    () => (integrations === null || !summary ? null : offeredTools(integrations, summary.workspace)),
    [integrations, summary],
  );

  if (loadError)
    return (
      <main className="page">
        <div className="notice bad">{loadError}</div>
        <Link to="/">← Back to flows</Link>
      </main>
    );
  if (!summary || spec === null) return <div className="center muted">Loading flow…</div>;

  const firstProblemNode = Object.keys(grouped.byNode)[0];

  return (
    <div className="editor">
      <div className="editor-bar">
        <Link to="/" className="link">
          ← Flows
        </Link>
        <div className="title">
          <strong>{flow?.name || summary.name}</strong>
          <span className="faint mono">
            {flowId} · {workspaceNames(me)[summary.workspace] ?? summary.workspace}
            <span style={{ fontFamily: "system-ui, sans-serif" }}>
              {" · "}
              {saving ? "saving…" : unsaved ? "unsaved changes" : editable ? "saved" : "view only"}
            </span>
          </span>
        </div>
        {summary.latest_version ? <span className="chip good">v{summary.latest_version} live</span> : <span className="chip">not published</span>}
        {problems.length > 0 ? (
          <button
            className="small"
            style={{ color: "var(--bad)", borderColor: "var(--bad)" }}
            onClick={() => setSelection(firstProblemNode ? { kind: "node", id: firstProblemNode } : { kind: "settings" })}
          >
            {problems.length} problem{problems.length === 1 ? "" : "s"}
          </button>
        ) : (
          <span className="chip good">no problems</span>
        )}
        <span className="spacer" />
        <div className="tabs" role="group" aria-label="View">
          <button aria-pressed={mode === "canvas"} onClick={toCanvas}>
            Canvas
          </button>
          <button aria-pressed={mode === "json"} onClick={toJson}>
            JSON
          </button>
        </div>
        {editable && mode === "canvas" && (
          <>
            <select
              aria-label="Add a step"
              value=""
              style={{ width: "auto" }}
              onChange={(e) => e.target.value && addNode(e.target.value as NodeType)}
            >
              <option value="">+ Add step</option>
              {(Object.keys(NODE_TYPE_LABEL) as NodeType[]).map((t) => (
                <option key={t} value={t}>
                  {NODE_TYPE_LABEL[t]}
                </option>
              ))}
            </select>
            <button
              title="Arrange the steps automatically"
              onClick={() => {
                if (flow) setPositions(autoLayout(flow));
                setLayoutDirty(true);
              }}
            >
              Tidy
            </button>
          </>
        )}
        {mode === "canvas" && flow && (
          <button onClick={() => setSelection({ kind: "settings" })} aria-pressed={selection.kind === "settings"}>
            Settings
          </button>
        )}
        <button onClick={() => setDialog("versions")}>History</button>
        <button onClick={() => navigate(`/flows/${flowId}/calls`)} title="How calls on this flow went">
          Calls
        </button>
        <div className="bar-actions">
          {(editable || checker) && (
            <button onClick={startTest} disabled={testing || saving} title="Talk to this draft from your browser (up to 5 minutes)">
              🎙 Test call
            </button>
          )}
          {editable && (
            <button className={unsaved ? "primary" : ""} disabled={!unsaved || saving} onClick={save} title="Ctrl/⌘ + S">
              Save
            </button>
          )}
          {editable && (
            <button className={unsaved || canDecide ? "" : "primary"} onClick={openSubmit} disabled={saving} title="Send the saved draft to a checker">
              Submit for review
            </button>
          )}
          {canDecide && (
            <button className="primary" onClick={() => setDialog("review")}>
              Review
            </button>
          )}
        </div>
      </div>
      {pending && (
        <div className="pending-banner" role="status">
          <span className="grow">
            {mySubmission ? "You" : pending.submitted_by} submitted this flow for review {timeAgo(pending.submitted_at)}.{" "}
            {canDecide
              ? "It's waiting for you or another checker."
              : mySubmission
                ? "Someone else has to approve it."
                : "It's waiting for a checker."}
            {checksum && checksum !== pending.checksum && " The draft has changed since; the checker sees the submitted version, not these changes."}
          </span>
          <button onClick={() => setDialog("review")}>{canDecide ? "Review it" : "See submission"}</button>
        </div>
      )}

      <div className="editor-body">
        <div className="canvas">
          {!editable && (
            <div className="notice info" style={{ position: "absolute", zIndex: 5, top: 10, left: 10, right: 10 }}>
              View only. Editing needs the maker role in this workspace{checker ? "; as a checker you review submitted changes" : ""}.
            </div>
          )}
          {conflict && (
            <div className="notice warn" style={{ position: "absolute", zIndex: 6, top: 10, left: 10, right: 10 }}>
              Someone else saved this flow since you opened it, so your save was stopped to avoid overwriting their work.{" "}
              <button className="small" onClick={() => navigator.clipboard.writeText(mode === "json" ? jsonText : serialize(spec)).then(() => toast("Your version is on the clipboard"))}>
                Copy my version
              </button>{" "}
              <button className="small" onClick={() => location.reload()}>
                Reload theirs
              </button>
            </div>
          )}
          {mode === "canvas" && flow ? (
            <Canvas
              flow={flow}
              positions={positions}
              problemsByNode={grouped.byNode}
              selection={selection}
              activeNode={activeNode}
              editable={editable}
              onSelect={setSelection}
              onMove={move}
              onConnect={connect}
            />
          ) : (
            <textarea
              className="json-editor"
              aria-label="Flow JSON"
              spellCheck={false}
              value={jsonText}
              readOnly={!editable}
              onChange={(e) => typeJson(e.target.value)}
            />
          )}
          {testing && (
            <TestCall
              flowId={flowId}
              onNode={setActiveNode}
              onClose={() => {
                setTesting(false);
                setActiveNode(null);
              }}
            />
          )}
        </div>

        <aside className="inspector">
          {mode === "json" ? (
            <div>
              <div className="inspector-head">
                <h3 className="grow">JSON</h3>
              </div>
              <p className="hint" style={{ marginTop: -8 }}>
                The exact flow the agent runs. Edit it here for anything the forms don't cover; switch back to the canvas to keep
                editing visually.
              </p>
              {jsonError && <div className="notice bad">Not valid JSON: {jsonError}</div>}
              {problems.length > 0 ? (
                <ul className="problems">
                  {problems.map((p, i) => (
                    <li key={i}>
                      <strong>{problemText(p).where}</strong>: {problemText(p).what}
                    </li>
                  ))}
                </ul>
              ) : (
                !jsonError && <div className="notice info">No problems.</div>
              )}
            </div>
          ) : flow && selectedNode ? (
            <NodeEditor
              key={selectedNode.id}
              flow={flow}
              id={selectedNode.id}
              node={flow.nodes[selectedNode.id]}
              focusTransition={selectedNode.transition}
              editable={editable}
              problems={grouped.byNode[selectedNode.id] ?? []}
              vars={vars}
              tools={tools}
              change={changeNode(selectedNode.id)}
              rename={(newId) => rename(selectedNode.id, newId)}
              remove={() => remove(selectedNode.id)}
              makeStart={() => edit((f) => (f.start_node = selectedNode.id))}
            />
          ) : flow ? (
            <SettingsEditor
              flow={flow}
              workspace={summary.workspace}
              catalog={catalog}
              tools={tools}
              editable={editable}
              problems={grouped.general}
              vars={vars}
              change={(fn) => edit(fn)}
              onSelectNode={(id) => setSelection({ kind: "node", id })}
            />
          ) : null}
        </aside>
      </div>

      {dialog === "submit" && (
        <SubmitDialog
          flowId={flowId}
          spec={spec}
          latestVersion={summary.latest_version}
          problems={problems}
          pending={pending}
          onClose={() => setDialog(null)}
          onSubmitted={(review) => {
            setSummary((s) =>
              s
                ? {
                    ...s,
                    pending_review: {
                      review_id: review.review_id,
                      submitted_by: review.submitted_by,
                      submitted_at: review.submitted_at,
                      checksum: review.checksum,
                    },
                  }
                : s,
            );
            setDialog(null);
            toast("Submitted. A checker in this workspace approves it to make it live.");
          }}
        />
      )}
      {dialog === "review" && pending && (
        <ReviewDialog
          me={me}
          flowId={flowId}
          workspace={summary.workspace}
          reviewId={pending.review_id}
          latestVersion={summary.latest_version}
          onClose={() => setDialog(null)}
          onDecided={(message) => {
            setDialog(null);
            toast(message);
            refreshSummary();
          }}
        />
      )}
      {dialog === "versions" && (
        <VersionsDialog
          flowId={flowId}
          draft={spec}
          editable={editable}
          onClose={() => setDialog(null)}
          onRestore={(restored, version) => {
            setSpec(clone(restored));
            setDirty(true);
            if (isEditable(restored)) {
              setPositions((p) => positionsFor(restored, p));
              setMode("canvas");
            } else {
              setJsonText(serialize(restored));
              setMode("json");
            }
            setSelection({ kind: "settings" });
            setDialog(null);
            toast(`v${version} copied into the draft. Save, then submit it for review to make it live.`);
          }}
        />
      )}
    </div>
  );
}

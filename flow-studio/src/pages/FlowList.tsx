import { useEffect, useMemo, useRef, useState } from "react";
import { ApiError, api, can, type Build, type FlowSummary, type Me } from "../api";
import { Dialog, Field, timeAgo } from "../components/ui";
import { Link, navigate } from "../router";
import { IDENTIFIER_HINT, IDENTIFIER_RE, newFlowTemplate, serialize } from "../spec";

/** Workspace id → display name, from the person's memberships (admins may see others too). */
export function workspaceNames(me: Me): Record<string, string> {
  return Object.fromEntries(me.workspaces.map((m) => [m.workspace_id, m.name || m.workspace_id]));
}

export function FlowList({ me }: { me: Me }) {
  const [flows, setFlows] = useState<FlowSummary[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const createIn = can.createIn(me);
  const names = workspaceNames(me);

  useEffect(() => {
    api.flows().then(setFlows, (e) => setError((e as Error).message));
  }, []);

  // One group per workspace, in the order of the person's memberships, then any others (admins).
  const groups = useMemo(() => {
    if (!flows) return [];
    const order = me.workspaces.map((m) => m.workspace_id).filter((w) => flows.some((f) => f.workspace === w));
    const ids = [...new Set([...order, ...flows.map((f) => f.workspace)])];
    return ids.map((ws) => ({ ws, flows: flows.filter((f) => f.workspace === ws) }));
  }, [flows, me.workspaces]);

  return (
    <main className="page">
      <div className="row">
        <div className="grow">
          <h1>Flows</h1>
          <p className="muted" style={{ margin: "4px 0 0" }}>
            {can.admin(me) ? "Every workspace's flows." : "The flows in your workspaces."} Approved changes reach callers on their next call.
          </p>
        </div>
        {(createIn === "*" || createIn.length > 0) && (
          <button className="primary" onClick={() => setCreating(true)}>
            New flow
          </button>
        )}
      </div>

      {error && <div className="notice bad" style={{ marginTop: 16 }}>{error}</div>}
      {!flows && !error && <p className="muted">Loading…</p>}
      {flows && me.workspaces.length === 0 && !can.admin(me) && (
        <div className="card" style={{ marginTop: 18 }}>
          <p style={{ margin: 0 }}>You aren't in a workspace yet.</p>
          <p className="muted" style={{ marginBottom: 0 }}>
            A workspace owner or platform admin adds you by your email ({me.email}); then its flows show here.
          </p>
        </div>
      )}
      {flows && flows.length === 0 && (me.workspaces.length > 0 || can.admin(me)) && (
        <div className="card" style={{ marginTop: 18 }}>
          <p style={{ margin: 0 }}>No flows in your workspaces yet.</p>
          {createIn !== "*" && createIn.length === 0 && (
            <p className="muted" style={{ marginBottom: 0 }}>
              Creating flows needs the maker role in a workspace; ask its owner.
            </p>
          )}
        </div>
      )}
      {groups.map((g) => (
        <section key={g.ws} className="ws-group">
          <div className="ws-group-head">
            <h2>{names[g.ws] ?? g.ws}</h2>
            <span className="muted">
              {g.flows.length} flow{g.flows.length === 1 ? "" : "s"}
            </span>
            <RoleChips me={me} workspace={g.ws} />
          </div>
          <div className="flow-list">
            {g.flows.map((f) => (
              <Link key={f.flow_id} to={`/flows/${f.flow_id}`} className="flow-item">
                <div>
                  <strong>{f.name}</strong>
                  <div className="meta">
                    <code>{f.flow_id}</code>
                    {f.numbers.length > 0 && <span>☎ {f.numbers.join(", ")}</span>}
                  </div>
                </div>
                <div className="meta" style={{ justifyContent: "flex-end" }}>
                  {f.latest_version ? <span className="chip good">v{f.latest_version} live</span> : <span className="chip">not published</span>}
                  {f.pending_review && (
                    <span className="chip warn" title={`Submitted by ${f.pending_review.submitted_by}`}>
                      waiting for review
                    </span>
                  )}
                  {f.draft_valid === false && <span className="chip bad">draft has problems</span>}
                  {f.draft_updated_at && <span>edited {timeAgo(f.draft_updated_at)}</span>}
                </div>
              </Link>
            ))}
          </div>
        </section>
      ))}
      {creating && <NewFlowDialog me={me} onClose={() => setCreating(false)} />}
    </main>
  );
}

/** The person's roles in a workspace, as small chips. */
export function RoleChips({ me, workspace }: { me: Me; workspace: string }) {
  const m = me.workspaces.find((x) => x.workspace_id === workspace);
  if (!m) return can.admin(me) ? <span className="chip">platform admin</span> : null;
  const roles = [m.maker && "maker", m.checker && "checker", m.owner && "owner"].filter(Boolean) as string[];
  return (
    <span className="roles">
      {(roles.length ? roles : ["viewer"]).map((r) => (
        <span key={r} className="chip">
          {r}
        </span>
      ))}
    </span>
  );
}

const slug = (name: string) =>
  name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^[^a-z]+/, "")
    .replace(/_+$/, "")
    .slice(0, 48);

// The server's limits for a description (about 4 characters a token, so the
// upper limit is roughly 100k tokens, well above a 14–20k-token brief).
const MIN_DESCRIPTION = 40;
const MAX_DESCRIPTION = 400_000;

function NewFlowDialog({ me, onClose }: { me: Me; onClose: () => void }) {
  const createIn = can.createIn(me);
  const [mode, setMode] = useState<"template" | "describe">("template");
  const [name, setName] = useState("");
  const [id, setId] = useState("");
  const [idTouched, setIdTouched] = useState(false);
  const [workspace, setWorkspace] = useState(createIn === "*" ? "" : createIn[0]?.workspace_id ?? "");
  const [description, setDescription] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [build, setBuild] = useState<Build | null>(null);
  const timer = useRef<number | null>(null);
  const [, tick] = useState(0);

  const flowId = idTouched ? id : slug(name);
  const idOk = IDENTIFIER_RE.test(flowId);
  const workspaceOk = /^[a-z][a-z0-9_-]{0,31}$/.test(workspace);
  const describeOk = description.trim().length >= MIN_DESCRIPTION && description.length <= MAX_DESCRIPTION;
  const ready = !!name.trim() && idOk && workspaceOk && (mode === "template" || describeOk);

  useEffect(
    () => () => {
      if (timer.current) clearTimeout(timer.current);
    },
    [],
  );

  const save = async (specText: string) => {
    setBusy(true);
    setError(null);
    try {
      await api.saveDraft(flowId, specText, { workspace, createOnly: true });
      navigate(`/flows/${flowId}`);
    } catch (e) {
      setError(e instanceof ApiError && e.status === 409 ? `A flow called ${flowId} already exists. Pick another id.` : (e as Error).message);
      setBusy(false);
    }
  };

  const poll = (buildId: string) => {
    timer.current = window.setTimeout(async () => {
      try {
        const b = await api.build(buildId);
        setBuild(b);
        tick((n) => n + 1);
        if (b.status === "running") poll(buildId);
        else setBusy(false);
      } catch (e) {
        setError((e as Error).message);
        setBusy(false);
      }
    }, 2000);
  };

  const create = async () => {
    if (mode === "template") return save(serialize(newFlowTemplate(flowId, name.trim())));
    setBusy(true);
    setError(null);
    // Make sure the id is free before spending minutes on a build.
    try {
      await api.flow(flowId);
      setError(`A flow called ${flowId} already exists. Pick another id.`);
      setBusy(false);
      return;
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 404)) {
        setError((e as Error).message);
        setBusy(false);
        return;
      }
    }
    try {
      const b = await api.startBuild({ workspace, flow_id: flowId, name: name.trim(), description });
      setBuild(b);
      poll(b.build_id);
    } catch (e) {
      setError((e as Error).message);
      setBusy(false);
    }
  };

  const result = build?.status === "done" ? build.result : undefined;
  const building = build?.status === "running";
  const locked = busy || !!build;
  const tokens = Math.round(description.length / 4);

  const footer = result ? (
    <>
      <span className="muted grow" style={{ fontSize: 13 }}>
        It's saved as a draft; nothing goes live until it's submitted and approved.
      </span>
      <button disabled={busy} onClick={() => setBuild(null)}>
        Change the description
      </button>
      <button className="primary" disabled={busy} onClick={() => save(serialize(result.flow))}>
        {busy ? "Saving…" : "Save and open"}
      </button>
    </>
  ) : (
    <>
      <button onClick={onClose}>Cancel</button>
      {build?.status === "failed" ? (
        <button className="primary" onClick={() => setBuild(null)}>
          Try again
        </button>
      ) : (
        <button className="primary" disabled={!ready || locked} onClick={create}>
          {mode === "describe" ? (building || busy ? "Building…" : "Build the flow") : busy ? "Creating…" : "Create flow"}
        </button>
      )}
    </>
  );

  return (
    <Dialog title="New flow" narrow={mode === "template"} onClose={onClose} footer={footer}>
      {!build && (
        <div className="mode-pick" role="group" aria-label="How to start">
          <button aria-pressed={mode === "template"} onClick={() => setMode("template")} disabled={locked}>
            Start small
            <small>A two-step flow you build up in the editor.</small>
          </button>
          <button aria-pressed={mode === "describe"} onClick={() => setMode("describe")} disabled={locked}>
            Describe it (AI)
            <small>Write how the call should go; the steps are made for you.</small>
          </button>
        </div>
      )}
      <Field label="Name" hint="What people see, e.g. “Credit card statement help”.">
        <input autoFocus value={name} disabled={locked} onChange={(e) => setName(e.target.value)} />
      </Field>
      <div className="row wrap" style={{ alignItems: "flex-start" }}>
        <div className="grow" style={{ minWidth: 200 }}>
          <Field label="Flow id" hint={`Can't be changed later. ${IDENTIFIER_HINT}.`}>
            <input
              className="mono"
              value={flowId}
              disabled={locked}
              onChange={(e) => {
                setIdTouched(true);
                setId(e.target.value);
              }}
            />
          </Field>
        </div>
        <div className="grow" style={{ minWidth: 200 }}>
          <Field label="Workspace" hint="Its makers edit the flow; its checkers approve changes.">
            {createIn === "*" ? (
              <input value={workspace} disabled={locked} placeholder="e.g. emi" onChange={(e) => setWorkspace(e.target.value.trim())} />
            ) : (
              <select value={workspace} disabled={locked} onChange={(e) => setWorkspace(e.target.value)}>
                {createIn.map((m) => (
                  <option key={m.workspace_id} value={m.workspace_id}>
                    {m.name || m.workspace_id}
                  </option>
                ))}
              </select>
            )}
          </Field>
        </div>
      </div>
      {flowId && !idOk && <div className="notice bad">Flow id: {IDENTIFIER_HINT}.</div>}

      {mode === "template" && (
        <p className="muted" style={{ fontSize: 13 }}>
          It starts as a small working flow (listen, then say goodbye) that you can change.
        </p>
      )}

      {mode === "describe" && !build && (
        <Field
          label="How should the call go?"
          hint={
            <>
              Write it the way you'd brief a new agent: who calls, what to ask and in what order, what to do in each case, what
              never to say. Long documents are fine.
              {description.length > 0 && ` About ${tokens.toLocaleString()} tokens.`}
              {description.length > 0 && description.trim().length < MIN_DESCRIPTION && " Write a bit more."}
              {description.length > MAX_DESCRIPTION && " That's too long; split it into more than one flow."}
            </>
          }
        >
          <textarea
            className="describe"
            value={description}
            disabled={locked}
            placeholder={"Callers are customers asking about their credit card statement.\nFirst confirm their name and the last 4 digits of the card…"}
            onChange={(e) => setDescription(e.target.value)}
          />
        </Field>
      )}

      {building && (
        <>
          <div className="build-progress" role="status">
            <span className="dot thinking" />
            <span className="grow">{build.progress || "Starting…"}</span>
            <span className="muted">{elapsed(build.started_at)}</span>
          </div>
          <p className="muted" style={{ fontSize: 13 }}>
            Long descriptions take a few minutes. Keep this window open.
          </p>
        </>
      )}
      {build?.status === "failed" && <div className="notice bad">The build didn't work: {build.error || "unknown error"}</div>}
      {result && (
        <>
          <div className={result.problems.length ? "notice warn" : "notice info"}>
            Made a flow with {result.steps} step{result.steps === 1 ? "" : "s"}
            {result.attempts > 1 ? `, after fixing its own mistakes ${result.attempts - 1} time${result.attempts > 2 ? "s" : ""}` : ""}.
            {result.problems.length > 0 && ` ${result.problems.length} problem(s) are left for you to fix in the editor.`}
          </div>
          {result.notes.length > 0 && (
            <>
              <strong style={{ fontSize: 13 }}>Check these before submitting it</strong>
              <ul className="build-notes">
                {result.notes.map((n, i) => (
                  <li key={i}>{n}</li>
                ))}
              </ul>
            </>
          )}
          {result.problems.length > 0 && (
            <ul className="build-notes">
              {result.problems.map((p, i) => (
                <li key={i}>
                  <code>{p}</code>
                </li>
              ))}
            </ul>
          )}
        </>
      )}
      {error && <div className="notice bad">{error}</div>}
    </Dialog>
  );
}

function elapsed(iso: string): string {
  const s = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`;
}

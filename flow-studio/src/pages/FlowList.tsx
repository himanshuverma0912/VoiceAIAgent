import { useEffect, useState } from "react";
import { ApiError, api, can, type FlowSummary, type Me } from "../api";
import { Dialog, Field, timeAgo } from "../components/ui";
import { Link, navigate } from "../router";
import { IDENTIFIER_HINT, IDENTIFIER_RE, newFlowTemplate, serialize } from "../spec";

export function FlowList({ me }: { me: Me }) {
  const [flows, setFlows] = useState<FlowSummary[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const createIn = can.createIn(me);

  useEffect(() => {
    api.flows().then(setFlows, (e) => setError((e as Error).message));
  }, []);

  return (
    <main className="page">
      <div className="row">
        <div className="grow">
          <h1>Flows</h1>
          <p className="muted" style={{ margin: "4px 0 0" }}>
            {me.roles.includes("platform-admin") ? "Every team's flows." : "Your teams' conversations."} Published
            changes reach callers on their next call.
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
      {flows && flows.length === 0 && (
        <div className="card" style={{ marginTop: 18 }}>
          <p style={{ margin: 0 }}>No flows for your teams yet.</p>
          {createIn !== "*" && createIn.length === 0 && (
            <p className="muted" style={{ marginBottom: 0 }}>
              Creating flows needs the flow-editor role; ask your platform admin.
            </p>
          )}
        </div>
      )}
      {flows && flows.length > 0 && (
        <div className="flow-list">
          {flows.map((f) => (
            <Link key={f.flow_id} to={`/flows/${f.flow_id}`} className="flow-item">
              <div>
                <strong>{f.name}</strong>
                <div className="meta">
                  <code>{f.flow_id}</code>
                  <span>team {f.team || "unassigned"}</span>
                  {f.numbers.length > 0 && <span>☎ {f.numbers.join(", ")}</span>}
                </div>
              </div>
              <div className="meta" style={{ justifyContent: "flex-end" }}>
                {f.latest_version ? <span className="chip good">v{f.latest_version} live</span> : <span className="chip">not published</span>}
                {f.draft_valid === false && <span className="chip bad">draft has problems</span>}
                {f.draft_updated_at && <span>edited {timeAgo(f.draft_updated_at)}</span>}
              </div>
            </Link>
          ))}
        </div>
      )}
      {creating && <NewFlowDialog me={me} onClose={() => setCreating(false)} />}
    </main>
  );
}

const slug = (name: string) =>
  name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^[^a-z]+/, "")
    .replace(/_+$/, "")
    .slice(0, 48);

function NewFlowDialog({ me, onClose }: { me: Me; onClose: () => void }) {
  const createIn = can.createIn(me);
  const [name, setName] = useState("");
  const [id, setId] = useState("");
  const [idTouched, setIdTouched] = useState(false);
  const [team, setTeam] = useState(createIn === "*" ? "" : createIn[0] ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const flowId = idTouched ? id : slug(name);
  const idOk = IDENTIFIER_RE.test(flowId);
  const teamOk = /^[a-z][a-z0-9_-]{0,31}$/.test(team);

  const create = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.saveDraft(flowId, serialize(newFlowTemplate(flowId, name.trim())), { team, createOnly: true });
      navigate(`/flows/${flowId}`);
    } catch (e) {
      setError(e instanceof ApiError && e.status === 409 ? `A flow called ${flowId} already exists. Pick another id.` : (e as Error).message);
      setBusy(false);
    }
  };

  return (
    <Dialog
      title="New flow"
      narrow
      onClose={onClose}
      footer={
        <>
          <button onClick={onClose}>Cancel</button>
          <button className="primary" disabled={!name.trim() || !idOk || !teamOk || busy} onClick={create}>
            {busy ? "Creating…" : "Create flow"}
          </button>
        </>
      }
    >
      <Field label="Name" hint="What people see, e.g. “Credit card statement help”.">
        <input autoFocus value={name} onChange={(e) => setName(e.target.value)} />
      </Field>
      <Field label="Flow id" hint={`Can't be changed later. ${IDENTIFIER_HINT}.`}>
        <input
          className="mono"
          value={flowId}
          onChange={(e) => {
            setIdTouched(true);
            setId(e.target.value);
          }}
        />
      </Field>
      {flowId && !idOk && <div className="notice bad">Flow id: {IDENTIFIER_HINT}.</div>}
      <Field label="Team" hint="Only this team's editors and publishers can change it.">
        {createIn === "*" ? (
          <input value={team} placeholder="e.g. emi" onChange={(e) => setTeam(e.target.value.trim())} />
        ) : (
          <select value={team} onChange={(e) => setTeam(e.target.value)}>
            {createIn.map((t) => (
              <option key={t}>{t}</option>
            ))}
          </select>
        )}
      </Field>
      <p className="muted" style={{ fontSize: 13 }}>
        It starts as a small working flow (listen, then say goodbye) that you can change.
      </p>
      {error && <div className="notice bad">{error}</div>}
    </Dialog>
  );
}

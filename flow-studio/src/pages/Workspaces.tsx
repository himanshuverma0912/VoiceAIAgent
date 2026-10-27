// Workspaces: groups of flows and the people who work on them. Everyone in a
// workspace sees its flows; makers edit and submit; checkers approve; owners
// manage the members. Platform admins create workspaces.
import { useEffect, useState } from "react";
import { ApiError, api, can, type Me, type Member, type Workspace } from "../api";
import { Dialog, Field, timeAgo, toast } from "../components/ui";
import { Link, navigate } from "../router";
import { RoleChips } from "./FlowList";

const WORKSPACE_RE = /^[a-z][a-z0-9_-]{0,31}$/;
const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

export function Workspaces({ me }: { me: Me }) {
  const [list, setList] = useState<Workspace[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);

  useEffect(() => {
    api.workspaces().then(setList, (e) => setError((e as Error).message));
  }, []);

  return (
    <main className="page">
      <div className="row">
        <div className="grow">
          <h1>Workspaces</h1>
          <p className="muted" style={{ margin: "4px 0 0" }}>
            {can.admin(me) ? "Every workspace." : "The workspaces you're in."} People see only the flows of their workspaces.
          </p>
        </div>
        {can.admin(me) && (
          <button className="primary" onClick={() => setCreating(true)}>
            New workspace
          </button>
        )}
      </div>
      {error && <div className="notice bad" style={{ marginTop: 16 }}>{error}</div>}
      {!list && !error && <p className="muted">Loading…</p>}
      {list && list.length === 0 && (
        <div className="card" style={{ marginTop: 18 }}>
          <p style={{ margin: 0 }}>{can.admin(me) ? "No workspaces yet. Create the first one." : "You aren't in a workspace yet."}</p>
          {!can.admin(me) && (
            <p className="muted" style={{ marginBottom: 0 }}>
              Ask a workspace owner or platform admin to add {me.email}.
            </p>
          )}
        </div>
      )}
      {list && list.length > 0 && (
        <div className="flow-list">
          {list.map((w) => (
            <Link key={w.workspace_id} to={`/workspaces/${w.workspace_id}`} className="flow-item">
              <div>
                <strong>{w.name}</strong>
                <div className="meta">
                  <code>{w.workspace_id}</code>
                  <span>
                    {w.flows} flow{w.flows === 1 ? "" : "s"}
                  </span>
                  <span>
                    {w.members} {w.members === 1 ? "person" : "people"}
                  </span>
                </div>
              </div>
              <div className="meta" style={{ justifyContent: "flex-end" }}>
                <RoleChips me={me} workspace={w.workspace_id} />
              </div>
            </Link>
          ))}
        </div>
      )}
      {creating && <NewWorkspaceDialog onClose={() => setCreating(false)} />}
    </main>
  );
}

function NewWorkspaceDialog({ onClose }: { onClose: () => void }) {
  const [name, setName] = useState("");
  const [id, setId] = useState("");
  const [touched, setTouched] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const wsId = touched
    ? id
    : name
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/^[^a-z]+/, "")
        .replace(/-+$/, "")
        .slice(0, 32);
  const ok = !!name.trim() && WORKSPACE_RE.test(wsId);

  const create = async () => {
    setBusy(true);
    setError(null);
    try {
      const existing = await api.members(wsId).catch(() => null);
      if (existing) {
        setError(`A workspace called ${wsId} already exists.`);
        setBusy(false);
        return;
      }
      await api.putWorkspace(wsId, name.trim());
      navigate(`/workspaces/${wsId}`);
    } catch (e) {
      setError((e as Error).message);
      setBusy(false);
    }
  };

  return (
    <Dialog
      title="New workspace"
      narrow
      onClose={onClose}
      footer={
        <>
          <button onClick={onClose}>Cancel</button>
          <button className="primary" disabled={!ok || busy} onClick={create}>
            {busy ? "Creating…" : "Create"}
          </button>
        </>
      }
    >
      <Field label="Name" hint="E.g. “EMI collections” or “Credit cards”.">
        <input autoFocus value={name} onChange={(e) => setName(e.target.value)} />
      </Field>
      <Field label="Workspace id" hint="Can't be changed later. Lower-case letters, digits, - and _, starting with a letter.">
        <input
          className="mono"
          value={wsId}
          onChange={(e) => {
            setTouched(true);
            setId(e.target.value);
          }}
        />
      </Field>
      <p className="muted" style={{ fontSize: 13 }}>
        Next you add its people: at least one owner, makers who build flows, and checkers who approve them.
      </p>
      {error && <div className="notice bad">{error}</div>}
    </Dialog>
  );
}

type Roles = { maker: boolean; checker: boolean; owner: boolean };

export function WorkspaceMembers({ me, workspace }: { me: Me; workspace: string }) {
  const [info, setInfo] = useState<Workspace | null>(null);
  const [members, setMembers] = useState<Member[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [email, setEmail] = useState("");
  const [roles, setRoles] = useState<Roles>({ maker: true, checker: false, owner: false });
  const [renaming, setRenaming] = useState(false);
  const manage = can.manage(me, workspace);

  const load = () =>
    api.members(workspace).then(
      (r) => {
        setInfo(r.workspace);
        setMembers(r.members);
      },
      (e) => setError(e instanceof ApiError && e.status === 404 ? "There's no such workspace, or you aren't in it." : (e as Error).message),
    );
  useEffect(() => {
    load();
  }, [workspace]);

  /** Your own roles live in the sign-in session's view; reload to pick changes up. */
  const afterOwnChange = (who: string) => {
    if (who === me.email) {
      toast("Your roles changed; reloading.");
      setTimeout(() => location.reload(), 900);
    }
  };

  const put = async (who: string, next: Roles) => {
    setBusy(who);
    try {
      const res = await api.putMember(workspace, who, next);
      toast(`${who}: ${describe(next)}.`);
      afterOwnChange(who);
      await load();
      return res;
    } catch (e) {
      toast((e as Error).message);
      return null;
    } finally {
      setBusy(null);
    }
  };

  const remove = async (who: string) => {
    if (!confirm(`Remove ${who} from ${info?.name ?? workspace}? They stop seeing its flows.`)) return;
    setBusy(who);
    try {
      await api.removeMember(workspace, who);
      toast(`${who} removed.`);
      afterOwnChange(who);
      await load();
    } catch (e) {
      toast((e as Error).message);
    } finally {
      setBusy(null);
    }
  };

  const add = async () => {
    const who = email.trim().toLowerCase();
    if (members?.some((m) => m.email === who)) return toast(`${who} is already in this workspace; change their roles below.`);
    if (await put(who, roles)) setEmail("");
  };

  const removeWorkspace = async () => {
    if (!confirm(`Delete the workspace ${info?.name}? This can't be undone.`)) return;
    try {
      await api.deleteWorkspace(workspace);
      toast("Workspace deleted.");
      navigate("/workspaces");
    } catch (e) {
      toast((e as Error).message);
    }
  };

  if (error)
    return (
      <main className="page">
        <div className="notice bad">{error}</div>
        <Link to="/workspaces">← Workspaces</Link>
      </main>
    );
  if (!info || !members) return <div className="center muted">Loading…</div>;

  return (
    <main className="page">
      <Link to="/workspaces" className="link">
        ← Workspaces
      </Link>
      <div className="row wrap" style={{ marginTop: 10 }}>
        <div className="grow">
          <h1>{info.name}</h1>
          <p className="muted" style={{ margin: "4px 0 0" }}>
            <code>{info.workspace_id}</code> · {info.flows} flow{info.flows === 1 ? "" : "s"} · created {timeAgo(info.created_at)}
            {info.created_by ? ` by ${info.created_by}` : ""}
          </p>
        </div>
        {manage && <button onClick={() => setRenaming(true)}>Rename</button>}
        {can.admin(me) && (
          <button className="danger" disabled={info.flows > 0} title={info.flows > 0 ? "Move or remove its flows first" : ""} onClick={removeWorkspace}>
            Delete
          </button>
        )}
      </div>

      <div className="card" style={{ marginTop: 18 }}>
        <h2 style={{ marginBottom: 4 }}>People</h2>
        <p className="muted" style={{ fontSize: 13, marginTop: 0 }}>
          Everyone here sees the workspace's flows and call history. <strong>Makers</strong> edit and submit changes;{" "}
          <strong>checkers</strong> approve or reject other people's changes (approving publishes); <strong>owners</strong> manage this
          list.
        </p>
        <div className="table-wrap">
          <table className="data">
            <thead>
              <tr>
                <th>Email</th>
                <th>Maker</th>
                <th>Checker</th>
                <th>Owner</th>
                <th>Added</th>
                {manage && <th />}
              </tr>
            </thead>
            <tbody>
              {members.map((m) => (
                <tr key={m.email} className="member-row">
                  <td>
                    {m.email}
                    {m.email === me.email && <span className="muted"> (you)</span>}
                  </td>
                  {(["maker", "checker", "owner"] as const).map((role) => (
                    <td key={role}>
                      <input
                        type="checkbox"
                        aria-label={`${m.email} ${role}`}
                        checked={m[role]}
                        disabled={!manage || busy === m.email}
                        onChange={(e) => put(m.email, { maker: m.maker, checker: m.checker, owner: m.owner, [role]: e.target.checked })}
                      />
                    </td>
                  ))}
                  <td className="muted" title={m.added_by}>
                    {timeAgo(m.added_at)}
                  </td>
                  {manage && (
                    <td className="num">
                      <button className="small" disabled={busy === m.email} onClick={() => remove(m.email)}>
                        Remove
                      </button>
                    </td>
                  )}
                </tr>
              ))}
              {members.length === 0 && (
                <tr>
                  <td colSpan={6} className="muted">
                    Nobody yet.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
        {manage && (
          <div className="add-member">
            <input
              type="email"
              aria-label="Email to add"
              placeholder="name@company.com"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && EMAIL_RE.test(email.trim()) && add()}
            />
            <div className="role-checks">
              {(["maker", "checker", "owner"] as const).map((role) => (
                <label key={role} className="check">
                  <input type="checkbox" checked={roles[role]} onChange={(e) => setRoles({ ...roles, [role]: e.target.checked })} />
                  {role}
                </label>
              ))}
            </div>
            <button className="primary" disabled={!EMAIL_RE.test(email.trim()) || !!busy} onClick={add}>
              Add
            </button>
          </div>
        )}
        {manage && (
          <p className="hint" style={{ marginBottom: 0 }}>
            Use the email they sign in with. They don't need to have signed in before. Nobody can approve their own change, even with
            both roles.
          </p>
        )}
      </div>
      {renaming && (
        <RenameDialog
          workspace={info}
          onClose={() => setRenaming(false)}
          onSaved={() => {
            setRenaming(false);
            load();
          }}
        />
      )}
    </main>
  );
}

function describe(r: Roles): string {
  const parts = [r.maker && "maker", r.checker && "checker", r.owner && "owner"].filter(Boolean);
  return parts.length ? parts.join(", ") : "can view";
}

function RenameDialog(props: { workspace: Workspace; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState(props.workspace.name);
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    try {
      await api.putWorkspace(props.workspace.workspace_id, name.trim());
      toast("Renamed.");
      props.onSaved();
    } catch (e) {
      toast((e as Error).message);
      setBusy(false);
    }
  };
  return (
    <Dialog
      title="Rename workspace"
      narrow
      onClose={props.onClose}
      footer={
        <>
          <button onClick={props.onClose}>Cancel</button>
          <button className="primary" disabled={!name.trim() || busy} onClick={save}>
            Save
          </button>
        </>
      }
    >
      <Field label="Name">
        <input autoFocus value={name} maxLength={80} onChange={(e) => setName(e.target.value)} />
      </Field>
    </Dialog>
  );
}

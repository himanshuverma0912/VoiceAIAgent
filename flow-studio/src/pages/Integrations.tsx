// Platform admins: the systems flows may call during a call (a bank's REST API
// or an MCP server), and their tools. Makers then add those tools to flows.
import { useEffect, useState } from "react";
import { ApiError, api, type Integration, type IntegrationInput, type IntegrationTool } from "../api";
import { Check, Dialog, Field, timeAgo, toast } from "../components/ui";

const ID_RE = /^[a-z][a-z0-9_]{0,31}$/;

const EXAMPLE_TOOLS: IntegrationTool[] = [
  {
    name: "get_card_balance",
    description: "The caller's outstanding balance, minimum due and due date, in rupees.",
    inputs: [
      { name: "card_last4", type: "string", description: "Last 4 digits of the caller's card", required: true },
      { name: "date_of_birth", type: "string", description: "The caller's date of birth, YYYY-MM-DD", required: true },
    ],
    outputs: ["outstanding", "minimum_due", "due_date"],
    sensitive: true,
    locked: ["card_last4", "date_of_birth"],
    max_calls: 3,
  },
];

export function Integrations() {
  const [list, setList] = useState<Integration[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<Integration | "new" | null>(null);

  const load = () => api.integrations().then(setList, (e) => setError((e as Error).message));
  useEffect(() => {
    load();
  }, []);

  const remove = async (i: Integration) => {
    if (!confirm(`Remove ${i.label}? Flows can't use its tools any more.`)) return;
    try {
      await api.deleteIntegration(i.integration_id);
      toast(`${i.label} removed.`);
      load();
    } catch (e) {
      toast((e as Error).message);
    }
  };

  return (
    <main className="page wide">
      <div className="row wrap">
        <div className="grow">
          <h1>Integrations</h1>
          <p className="muted" style={{ margin: "4px 0 0" }}>
            The bank systems flows may call during a call: REST APIs and MCP servers. Makers add their tools to steps; tools that
            need a verified caller only work after a step that verifies the caller, and inputs marked as set by the flow are
            never chosen by the AI.
          </p>
        </div>
        <button className="primary" onClick={() => setEditing("new")}>
          Add integration
        </button>
      </div>
      {error && <div className="notice bad" style={{ marginTop: 16 }}>{error}</div>}
      {!list && !error && <p className="muted">Loading…</p>}
      {list && list.length === 0 && (
        <div className="card" style={{ marginTop: 18 }}>
          <p style={{ margin: 0 }}>No integrations yet.</p>
          <p className="muted" style={{ marginBottom: 0 }}>
            To try tools with made-up customers, add the demo bank: on EC2 run{" "}
            <code>scripts/add-integration.sh infra/integrations/demo_bank.json</code>.
          </p>
        </div>
      )}
      {list?.map((i) => (
        <section key={i.integration_id} className="card calls-section">
          <div className="row wrap">
            <div className="grow">
              <h2 style={{ margin: 0 }}>
                {i.label} {i.enabled ? <span className="chip good">on</span> : <span className="chip">off</span>}
              </h2>
              <div className="muted small-text">
                <code>{i.integration_id}</code> · {i.kind === "mcp" ? "MCP server" : "REST API"} · <code>{i.url}</code>
                {i.test_url ? (
                  <>
                    {" "}
                    · test calls: <code>{i.test_url}</code>
                  </>
                ) : null}
                {" · "}
                {i.workspaces.length ? `workspaces ${i.workspaces.join(", ")}` : "every workspace"}
                {i.auth_env ? (
                  <>
                    {" "}
                    · credential in <code>{i.auth_env}</code>
                  </>
                ) : (
                  " · no credential"
                )}
              </div>
            </div>
            <span className="muted small-text" title={i.updated_by}>
              changed {timeAgo(i.updated_at)}
            </span>
            <button className="small" onClick={() => setEditing(i)}>
              Edit
            </button>
            <button className="small danger" onClick={() => remove(i)}>
              Remove
            </button>
          </div>
          {i.notes && <p className="small-text">{i.notes}</p>}
          <div className="table-wrap">
            <table className="data">
              <thead>
                <tr>
                  <th>Tool</th>
                  <th>Inputs</th>
                  <th>Returns</th>
                  <th>Rules</th>
                </tr>
              </thead>
              <tbody>
                {i.tools.map((t) => (
                  <tr key={t.name}>
                    <td>
                      <code>{t.name}</code>
                      <div className="hint">{t.description}</div>
                    </td>
                    <td className="small-text">
                      {t.inputs.map((x) => (
                        <div key={x.name}>
                          <code>{x.name}</code>
                          {x.required ? "" : " (optional)"}
                          {t.locked.includes(x.name) && <span className="chip" style={{ marginLeft: 4 }}>set by the flow</span>}
                        </div>
                      ))}
                    </td>
                    <td className="small-text">{t.outputs.join(", ") || <span className="muted">—</span>}</td>
                    <td className="small-text">
                      {t.sensitive && <span className="chip warn">needs a verified caller</span>} at most {t.max_calls} per call
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      ))}
      {editing && (
        <IntegrationDialog
          existing={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            load();
          }}
        />
      )}
    </main>
  );
}

function IntegrationDialog(props: { existing: Integration | null; onClose: () => void; onSaved: () => void }) {
  const e = props.existing;
  const [id, setId] = useState(e?.integration_id ?? "");
  const [kind, setKind] = useState<Integration["kind"]>(e?.kind ?? "mcp");
  const [label, setLabel] = useState(e?.label ?? "");
  const [notes, setNotes] = useState(e?.notes ?? "");
  const [url, setUrl] = useState(e?.url ?? "");
  const [testUrl, setTestUrl] = useState(e?.test_url ?? "");
  const [authHeader, setAuthHeader] = useState(e?.auth_header ?? "Authorization");
  const [authEnv, setAuthEnv] = useState(e?.auth_env ?? "");
  const [timeout, setTimeout_] = useState(String(e?.timeout_ms ?? 5000));
  const [workspaces, setWorkspaces] = useState((e?.workspaces ?? []).join(", "));
  const [enabled, setEnabled] = useState(e?.enabled ?? true);
  const [tools, setTools] = useState(JSON.stringify(e?.tools ?? EXAMPLE_TOOLS, null, 2));
  const [problems, setProblems] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);

  const save = async () => {
    let parsed: IntegrationTool[];
    try {
      parsed = JSON.parse(tools);
      if (!Array.isArray(parsed)) throw new Error("must be a list");
    } catch (err) {
      setProblems([`tools: not valid JSON (${(err as Error).message})`]);
      return;
    }
    const input: IntegrationInput = {
      kind,
      label: label.trim(),
      notes: notes.trim(),
      url: url.trim(),
      test_url: testUrl.trim(),
      auth_header: authEnv.trim() ? authHeader.trim() : "",
      auth_env: authEnv.trim(),
      timeout_ms: Number(timeout) || 5000,
      workspaces: workspaces
        .split(",")
        .map((x) => x.trim())
        .filter(Boolean),
      enabled,
      tools: parsed,
    };
    setBusy(true);
    try {
      const res = await api.putIntegration(id, input);
      toast(`${res.integration.label} ${res.created ? "added" : "saved"}.${res.used_by.length ? ` Used by ${res.used_by.join(", ")}.` : ""}`);
      props.onSaved();
    } catch (err) {
      setProblems(err instanceof ApiError && err.problems.length ? err.problems : [(err as Error).message]);
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={e ? `Edit ${e.label}` : "Add integration"}
      onClose={props.onClose}
      footer={
        <>
          <button onClick={props.onClose}>Cancel</button>
          <button className="primary" disabled={busy || !ID_RE.test(id) || !label.trim() || !url.trim()} onClick={save}>
            {e ? "Save" : "Add"}
          </button>
        </>
      }
    >
      {problems.length > 0 && (
        <div className="notice bad">
          {problems.map((p, i) => (
            <div key={i}>{p}</div>
          ))}
        </div>
      )}
      <div className="row wrap" style={{ alignItems: "flex-start" }}>
        <div className="grow" style={{ minWidth: 200 }}>
          <Field label="Integration id" hint={e ? "Can't be changed." : "Flows use its tools as <id>.<tool>, e.g. core_banking."}>
            <input className="mono" value={id} disabled={!!e} onChange={(ev) => setId(ev.target.value.trim())} autoFocus={!e} />
          </Field>
        </div>
        <div className="grow" style={{ minWidth: 200 }}>
          <Field label="Kind">
            <select value={kind} onChange={(ev) => setKind(ev.target.value as Integration["kind"])}>
              <option value="mcp">MCP server (streamable HTTP)</option>
              <option value="http">REST API (JSON)</option>
            </select>
          </Field>
        </div>
      </div>
      <Field label="Name shown to makers">
        <input value={label} onChange={(ev) => setLabel(ev.target.value)} />
      </Field>
      <Field label="Notes (optional)">
        <input value={notes} onChange={(ev) => setNotes(ev.target.value)} />
      </Field>
      <Field label="URL" hint={kind === "mcp" ? "The MCP endpoint, e.g. http://core-banking:8000/mcp" : "The API's base URL; each tool adds its path."}>
        <input className="mono" value={url} onChange={(ev) => setUrl(ev.target.value)} />
      </Field>
      <Field label="Test URL (optional)" hint="Flow Studio test calls use this (a sandbox), so makers never see real customers' data.">
        <input className="mono" value={testUrl} onChange={(ev) => setTestUrl(ev.target.value)} />
      </Field>
      <div className="row wrap" style={{ alignItems: "flex-start" }}>
        <div className="grow" style={{ minWidth: 200 }}>
          <Field label="Credential variable (optional)" hint="In the voice agent's .env; must start with INTEGRATION_. Its value is sent as the header.">
            <input className="mono" value={authEnv} placeholder="INTEGRATION_CORE_BANKING_TOKEN" onChange={(ev) => setAuthEnv(ev.target.value.trim())} />
          </Field>
        </div>
        <div style={{ width: 170 }}>
          <Field label="Header">
            <input className="mono" value={authHeader} disabled={!authEnv} onChange={(ev) => setAuthHeader(ev.target.value.trim())} />
          </Field>
        </div>
        <div style={{ width: 120 }}>
          <Field label="Timeout (ms)">
            <input type="number" min={500} max={30000} value={timeout} onChange={(ev) => setTimeout_(ev.target.value)} />
          </Field>
        </div>
      </div>
      <Field label="Workspaces (optional)" hint="Comma-separated workspace ids. Empty = every workspace.">
        <input value={workspaces} onChange={(ev) => setWorkspaces(ev.target.value)} />
      </Field>
      <Field
        label="Tools (JSON)"
        hint={
          <>
            Each tool: name, description (the AI reads it), inputs (name, type, description, required), outputs (what an API step
            may keep), sensitive (needs a verified caller), locked (inputs the flow must set), max_calls
            {kind === "http" ? ", method (GET or POST; sensitive tools POST) and path, e.g. /cards/{card_last4}/balance" : ""}.
          </>
        }
      >
        <textarea className="mono" rows={14} value={tools} onChange={(ev) => setTools(ev.target.value)} spellCheck={false} />
      </Field>
      <Check label="Switched on (switching off stops its tools on every call at once)" checked={enabled} onChange={setEnabled} />
    </Dialog>
  );
}

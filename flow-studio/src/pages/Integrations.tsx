// Integrations: the systems flows may call during a call (an MCP server or a
// REST API). Platform admins add platform-wide ones; makers add their own
// workspace's (an MCP server their team runs, wherever it is), which a checker
// of the workspace approves before live calls use them.
import { useEffect, useMemo, useState } from "react";
import {
  ApiError,
  api,
  can,
  draftDef,
  integrationForMe,
  type DiscoveredTools,
  type Integration,
  type IntegrationDef,
  type IntegrationInput,
  type IntegrationTool,
  type Me,
} from "../api";
import { Check, Dialog, Field, PageHero, timeAgo, toast } from "../components/ui";
import { reviewsChanged } from "./Reviews";
import { workspaceNames } from "./FlowList";

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

/** Who may do what with an integration (the server checks the same rules). */
const allowed = (me: Me, i: Integration) => {
  const ws = i.owner_workspace ?? "";
  return {
    edit: ws ? can.edit(me, ws) : can.admin(me),
    review: integrationForMe(me, i),
    withdraw: !!i.pending && i.pending_by === me.email,
    switchOff: ws ? can.edit(me, ws) || can.approve(me, ws) : can.admin(me),
    switchOn: ws ? can.approve(me, ws) : can.admin(me),
    remove: ws ? can.approve(me, ws) : can.admin(me),
  };
};

export function Integrations({ me }: { me: Me }) {
  const [list, setList] = useState<Integration[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<Integration | "new" | null>(null);
  const [reviewing, setReviewing] = useState<Integration | null>(null);
  const names = workspaceNames(me);
  const admin = can.admin(me);
  const makerIn = me.workspaces.filter((m) => m.maker);

  const load = () => api.integrations().then(setList, (e) => setError((e as Error).message));
  useEffect(() => {
    load();
  }, []);

  const act = async (what: Promise<unknown>, done: string) => {
    try {
      await what;
      toast(done);
      reviewsChanged();
      load();
    } catch (e) {
      toast((e as Error).message);
    }
  };

  const remove = (i: Integration) => {
    if (!confirm(`Remove ${i.label}? Flows can't use its tools any more.`)) return;
    act(api.deleteIntegration(i.integration_id), `${i.label} removed.`);
  };

  const waiting = list?.filter((i) => integrationForMe(me, i)) ?? [];
  const own = list?.filter((i) => i.owner_workspace) ?? [];
  const platform = list?.filter((i) => !i.owner_workspace) ?? [];
  const groups: { title: string; items: Integration[] }[] = [];
  for (const ws of [...new Set(own.map((i) => i.owner_workspace!))].sort()) {
    groups.push({ title: `${names[ws] ?? ws}'s own`, items: own.filter((i) => i.owner_workspace === ws) });
  }
  if (platform.length) groups.push({ title: "Platform integrations", items: platform });

  const card = (i: Integration) => (
    <IntegrationCard
      key={i.integration_id}
      me={me}
      i={i}
      may={allowed(me, i)}
      onEdit={() => setEditing(i)}
      onReview={() => setReviewing(i)}
      onWithdraw={() => act(api.withdrawIntegration(i.integration_id), "Change withdrawn.")}
      onSwitch={(on) => act(api.setIntegrationEnabled(i.integration_id, on), `${i.label} switched ${on ? "on" : "off"}.`)}
      onRemove={() => remove(i)}
    />
  );

  return (
    <main className="page wide">
      <PageHero
        title="Integrations"
        actions={
          (admin || makerIn.length > 0) && (
            <button className="primary" onClick={() => setEditing("new")}>
              Add an MCP server
            </button>
          )
        }
      >
        The systems flows may call during a call. Connect your team's own MCP server wherever it runs: a checker in your workspace approves it
        before live calls use it, and its token is sealed so only the voice agent can read it.
      </PageHero>
      {error && <div className="notice bad" style={{ marginTop: 16 }}>{error}</div>}
      {!list && !error && <p className="muted">Loading…</p>}
      {list && list.length === 0 && (
        <div className="card" style={{ marginTop: 18 }}>
          <p style={{ margin: 0 }}>No integrations yet.</p>
          <p className="muted" style={{ marginBottom: 0 }}>
            {makerIn.length || admin
              ? "Add your team's MCP server: give its address and token, load its tools, and send it for review."
              : "Ask a maker in your workspace, or a platform admin, to add one."}
          </p>
        </div>
      )}
      {waiting.length > 0 && (
        <section className="ws-group">
          <div className="ws-group-head">
            <h2>Waiting for your review</h2>
            <span className="muted">{waiting.length}</span>
          </div>
          {waiting.map(card)}
        </section>
      )}
      {groups.map((g) => (
        <section key={g.title} className="ws-group">
          <div className="ws-group-head">
            <h2>{g.title}</h2>
            <span className="muted">{g.items.length}</span>
          </div>
          {g.items.filter((i) => !waiting.includes(i)).map(card)}
        </section>
      ))}
      {editing && (
        <IntegrationDialog
          me={me}
          existing={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            reviewsChanged();
            load();
          }}
        />
      )}
      {reviewing && (
        <IntegrationReview
          me={me}
          integration={reviewing}
          onClose={() => setReviewing(null)}
          onDecided={(msg) => {
            setReviewing(null);
            toast(msg);
            reviewsChanged();
            load();
          }}
        />
      )}
    </main>
  );
}

function statusChips(i: Integration) {
  return (
    <>
      {i.enabled ? <span className="chip good">on</span> : <span className="chip">off</span>}
      {i.pending && <span className="chip warn">{i.live ? "change waiting for review" : "waiting for review"}</span>}
      {!i.live && !i.pending && i.rejected_by && <span className="chip bad">turned down</span>}
    </>
  );
}

function authText(d: IntegrationDef): string {
  const a = d.auth;
  if (a?.kind === "header") return `token in ${a.header || "Authorization"}${a.has_secret ? " (saved, sealed)" : ""}`;
  if (a?.kind === "oauth") return `OAuth client ${a.client_id}${a.has_secret ? " (secret saved, sealed)" : ""}`;
  if (d.auth_env) return `credential in ${d.auth_env}`;
  return "no sign-in";
}

function IntegrationCard(props: {
  me: Me;
  i: Integration;
  may: ReturnType<typeof allowed>;
  onEdit: () => void;
  onReview: () => void;
  onWithdraw: () => void;
  onSwitch: (on: boolean) => void;
  onRemove: () => void;
}) {
  const { i, may } = props;
  const shown = (i.live ? i : draftDef(i)) ?? i; // a new one not yet approved: what was proposed
  return (
    <section className="card calls-section" data-integration={i.integration_id}>
      <div className="row wrap">
        <div className="grow">
          <h2 style={{ margin: 0 }}>
            {shown.label} {statusChips(i)}
          </h2>
          <div className="muted small-text">
            <code>{i.integration_id}</code> · {shown.kind === "mcp" ? "MCP server" : "REST API"}
            {shown.url && (
              <>
                {" "}
                · <code>{shown.url}</code>
              </>
            )}
            {shown.test_url && (
              <>
                {" "}
                · test calls: <code>{shown.test_url}</code>
              </>
            )}
            {!i.owner_workspace && <> · {shown.workspaces.length ? `workspaces ${shown.workspaces.join(", ")}` : "every workspace"}</>}
            {shown.url !== undefined && <> · {authText(shown)}</>}
          </div>
        </div>
        <span className="muted small-text" title={i.updated_by}>
          changed {timeAgo(i.updated_at)}
        </span>
        {may.review && (
          <button className="small primary" onClick={props.onReview}>
            Review
          </button>
        )}
        {may.edit && (
          <button className="small" onClick={props.onEdit}>
            {i.pending ? "Edit the change" : "Edit"}
          </button>
        )}
        {may.withdraw && (
          <button className="small" onClick={props.onWithdraw}>
            Withdraw
          </button>
        )}
        {i.enabled && may.switchOff && (i.live || !i.owner_workspace) && (
          <button className="small" onClick={() => props.onSwitch(false)} title="Stops its tools on every call at once">
            Switch off
          </button>
        )}
        {!i.enabled && may.switchOn && (
          <button className="small" onClick={() => props.onSwitch(true)}>
            Switch on
          </button>
        )}
        {may.remove && (
          <button className="small danger" onClick={props.onRemove}>
            Remove
          </button>
        )}
      </div>
      {i.pending && (
        <div className="notice warn" style={{ marginTop: 8 }}>
          {i.pending_by === props.me.email ? "You" : i.pending_by} sent {i.live ? "a change" : "this"} for review {timeAgo(i.pending_at)}
          {i.pending_note ? `: “${i.pending_note}”` : "."} {i.live ? "Live calls use the approved version until a checker approves it; drafts and test calls already use the change." : "Drafts and test calls can use it now; flows using it can be submitted once a checker approves it."}
        </div>
      )}
      {!i.pending && i.rejected_by && (
        <div className="notice bad" style={{ marginTop: 8 }}>
          {i.rejected_by} turned down the last change {timeAgo(i.rejected_at)}: “{i.rejected_note}”
        </div>
      )}
      {shown.notes && <p className="small-text">{shown.notes}</p>}
      <ToolTable tools={(draftDef(i) ?? shown).tools} />
    </section>
  );
}

function ToolTable({ tools }: { tools: IntegrationTool[] }) {
  return (
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
          {tools.map((t) => (
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
  );
}

// ── Adding or changing one ────────────────────────────────────────────────────

type AuthKind = "" | "header" | "oauth";

/** A tool as the maker sets it up: whether flows may use it, and its rules. */
interface ToolRow {
  use: boolean;
  tool: IntegrationTool;
  fromServer: boolean;
}

function mergeTools(found: IntegrationTool[], current: IntegrationTool[], firstLoad: boolean): ToolRow[] {
  const byName = new Map(current.map((t) => [t.name, t]));
  return found.map((t) => {
    const was = byName.get(t.name);
    // Keep what the maker decided (rules, outputs) for tools already set up.
    const tool: IntegrationTool = was
      ? {
          ...t,
          outputs: was.outputs,
          sensitive: was.sensitive,
          locked: was.locked.filter((l) => t.inputs.some((x) => x.name === l)),
          max_calls: was.max_calls,
        }
      : t;
    return { use: firstLoad ? true : !!was, tool, fromServer: true };
  });
}

function IntegrationDialog(props: { me: Me; existing: Integration | null; onClose: () => void; onSaved: () => void }) {
  const { me } = props;
  const e = props.existing;
  const admin = can.admin(me);
  const base: IntegrationDef | null = e ? draftDef(e) ?? e : null;
  const makerIn = me.workspaces.filter((m) => m.maker);
  const [owner, setOwner] = useState(e ? e.owner_workspace ?? "" : admin && makerIn.length === 0 ? "" : makerIn[0]?.workspace_id ?? "");
  const platform = owner === "";
  const [id, setId] = useState(e?.integration_id ?? "");
  const [kind, setKind] = useState<IntegrationDef["kind"]>(base?.kind ?? "mcp");
  const [label, setLabel] = useState(base?.label ?? "");
  const [notes, setNotes] = useState(base?.notes ?? "");
  const [url, setUrl] = useState(base?.url ?? "");
  const [testUrl, setTestUrl] = useState(base?.test_url ?? "");
  const [authKind, setAuthKind] = useState<AuthKind>((base?.auth?.kind as AuthKind) || "");
  const [header, setHeader] = useState(base?.auth?.header || "Authorization");
  const [tokenUrl, setTokenUrl] = useState(base?.auth?.token_url ?? "");
  const [clientId, setClientId] = useState(base?.auth?.client_id ?? "");
  const [scope, setScope] = useState(base?.auth?.scope ?? "");
  const [secret, setSecret] = useState("");
  const [authEnv, setAuthEnv] = useState(base?.auth_env ?? "");
  const [authHeader, setAuthHeader] = useState(base?.auth_header || "Authorization");
  const [timeout, setTimeout_] = useState(String(base?.timeout_ms ?? 5000));
  const [workspaces, setWorkspaces] = useState((base?.workspaces ?? []).join(", "));
  const [enabled, setEnabled] = useState(base?.enabled ?? true);
  const [rows, setRows] = useState<ToolRow[]>((base?.tools ?? []).map((t) => ({ use: true, tool: t, fromServer: false })));
  const [asJson, setAsJson] = useState(kind === "http");
  const [json, setJson] = useState(JSON.stringify(base?.tools ?? EXAMPLE_TOOLS, null, 2));
  const [found, setFound] = useState<DiscoveredTools | null>(null);
  const [note, setNote] = useState("");
  const [problems, setProblems] = useState<string[]>([]);
  const [busy, setBusy] = useState<"" | "load" | "save">("");
  const hadSecret = !!base?.auth?.has_secret;
  const goesToReview = !admin;
  const addressChanged = !!base && (url.trim() !== (base.url ?? "") || testUrl.trim() !== (base.test_url ?? "") || tokenUrl.trim() !== (base.auth?.token_url ?? ""));
  const secretKept = hadSecret && !secret && !addressChanged && authKind === base?.auth?.kind;

  const authInput = (): NonNullable<IntegrationInput["auth"]> =>
    authKind === "header"
      ? { kind: "header", header: header.trim(), secret }
      : authKind === "oauth"
        ? { kind: "oauth", token_url: tokenUrl.trim(), client_id: clientId.trim(), scope: scope.trim(), secret }
        : { kind: "" };

  const load = async () => {
    setBusy("load");
    setProblems([]);
    try {
      const res = await api.discoverTools({ workspace: platform ? undefined : owner, url: url.trim(), auth: authInput() });
      setFound(res);
      setRows((current) => mergeTools(res.tools, current.map((r) => r.tool), current.length === 0));
      setAsJson(false);
      if (!label.trim() && res.server.name) setLabel(res.server.name);
    } catch (err) {
      setProblems(err instanceof ApiError && err.problems.length ? err.problems : [(err as Error).message]);
    } finally {
      setBusy("");
    }
  };

  const save = async () => {
    let tools: IntegrationTool[];
    if (asJson) {
      try {
        tools = JSON.parse(json);
        if (!Array.isArray(tools)) throw new Error("must be a list");
      } catch (err) {
        setProblems([`tools: not valid JSON (${(err as Error).message})`]);
        return;
      }
    } else {
      tools = rows.filter((r) => r.use).map((r) => r.tool);
    }
    const input: IntegrationInput = {
      kind,
      label: label.trim(),
      notes: notes.trim(),
      url: url.trim(),
      test_url: testUrl.trim(),
      timeout_ms: Number(timeout) || 5000,
      workspaces: platform
        ? workspaces
            .split(",")
            .map((x) => x.trim())
            .filter(Boolean)
        : [owner],
      enabled,
      tools,
      auth: authInput(),
      ...(platform && authKind === "" && authEnv.trim() ? { auth_header: authHeader.trim(), auth_env: authEnv.trim() } : {}),
      ...(platform ? {} : { workspace: owner }),
      ...(goesToReview && note.trim() ? { note: note.trim() } : {}),
    };
    setBusy("save");
    try {
      const res = await api.putIntegration(id, input);
      toast(
        res.pending
          ? `${res.integration.label ?? id} sent for review. A checker in the workspace approves it.`
          : `${input.label} ${res.created ? "added" : "saved"}.${res.used_by.length ? ` Used by ${res.used_by.join(", ")}.` : ""}`,
      );
      props.onSaved();
    } catch (err) {
      setProblems(err instanceof ApiError && err.problems.length ? err.problems : [(err as Error).message]);
      setBusy("");
    }
  };

  const setTool = (k: number, change: Partial<IntegrationTool> | ((t: IntegrationTool) => Partial<IntegrationTool>)) =>
    setRows((rs) => rs.map((r, i) => (i === k ? { ...r, tool: { ...r.tool, ...(typeof change === "function" ? change(r.tool) : change) } } : r)));

  const usable = asJson || rows.some((r) => r.use);
  const ready = ID_RE.test(id) && label.trim() && url.trim() && usable && (platform ? admin : !!owner);

  return (
    <Dialog
      title={e ? `${goesToReview ? "Change" : "Edit"} ${base?.label ?? e.integration_id}` : "Add an MCP server"}
      onClose={props.onClose}
      footer={
        <>
          <button onClick={props.onClose}>Cancel</button>
          <button className="primary" disabled={!!busy || !ready} onClick={save}>
            {busy === "save" ? "Saving…" : goesToReview ? "Send for review" : e ? "Save" : "Add"}
          </button>
        </>
      }
    >
      {problems.length > 0 && (
        <div className="notice bad" role="alert">
          {problems.map((p, i) => (
            <div key={i}>{p}</div>
          ))}
        </div>
      )}
      {goesToReview && (
        <div className="notice info">
          A checker in the workspace approves this before live calls use it. Until then your drafts and test calls can use it already.
        </div>
      )}
      <div className="row wrap" style={{ alignItems: "flex-start" }}>
        <div className="grow" style={{ minWidth: 200 }}>
          <Field label="Workspace" hint={e ? "Can't be changed." : "Only this workspace's flows can use it."}>
            <select value={owner} disabled={!!e} onChange={(ev) => setOwner(ev.target.value)}>
              {admin && <option value="">Platform (any workspace you choose)</option>}
              {(admin ? me.workspaces : makerIn).map((m) => (
                <option key={m.workspace_id} value={m.workspace_id}>
                  {m.name || m.workspace_id}
                </option>
              ))}
            </select>
          </Field>
        </div>
        <div className="grow" style={{ minWidth: 200 }}>
          <Field label="Integration id" hint={e ? "Can't be changed." : "Flows use its tools as <id>.<tool>, e.g. loans_mcp."}>
            <input className="mono" value={id} disabled={!!e} onChange={(ev) => setId(ev.target.value.trim())} autoFocus={!e} />
          </Field>
        </div>
        <div style={{ width: 210 }}>
          <Field label="Kind">
            <select
              value={kind}
              onChange={(ev) => {
                const k = ev.target.value as IntegrationDef["kind"];
                setKind(k);
                if (k === "http") setAsJson(true);
              }}
            >
              <option value="mcp">MCP server (streamable HTTP)</option>
              <option value="http">REST API (JSON)</option>
            </select>
          </Field>
        </div>
      </div>
      <Field label="URL" hint={kind === "mcp" ? "The MCP endpoint as the voice agent reaches it, e.g. https://loans-mcp.payments.bank.internal/mcp" : "The API's base URL; each tool adds its path."}>
        <input className="mono" value={url} onChange={(ev) => setUrl(ev.target.value)} placeholder="https://" />
      </Field>
      <Field label="Test URL (optional)" hint="Flow Studio test calls use this (a sandbox), so nobody sees real customers' data while testing.">
        <input className="mono" value={testUrl} onChange={(ev) => setTestUrl(ev.target.value)} />
      </Field>
      <fieldset className="auth-box">
        <legend>Sign-in</legend>
        <div className="tabs" role="group" aria-label="Sign-in">
          {(
            [
              ["", "None"],
              ["header", "Token in a header"],
              ["oauth", "OAuth client credentials"],
            ] as [AuthKind, string][]
          ).map(([k, text]) => (
            <button key={k} type="button" aria-pressed={authKind === k} onClick={() => setAuthKind(k)}>
              {text}
            </button>
          ))}
        </div>
        {authKind === "header" && (
          <div className="row wrap" style={{ alignItems: "flex-start", marginTop: 10 }}>
            <div style={{ width: 170 }}>
              <Field label="Header">
                <input className="mono" value={header} onChange={(ev) => setHeader(ev.target.value.trim())} />
              </Field>
            </div>
            <div className="grow" style={{ minWidth: 220 }}>
              <Field label="Token" hint={secretKept ? "A token is saved; leave this empty to keep it." : "Include any prefix, e.g. Bearer abc123."}>
                <input
                  type="password"
                  className="mono"
                  autoComplete="off"
                  value={secret}
                  placeholder={secretKept ? "•••••••• (saved)" : "Bearer …"}
                  onChange={(ev) => setSecret(ev.target.value)}
                />
              </Field>
            </div>
          </div>
        )}
        {authKind === "oauth" && (
          <>
            <Field label="Token URL">
              <input className="mono" value={tokenUrl} onChange={(ev) => setTokenUrl(ev.target.value)} placeholder="https://idp.bank.internal/oauth2/token" />
            </Field>
            <div className="row wrap" style={{ alignItems: "flex-start" }}>
              <div className="grow" style={{ minWidth: 160 }}>
                <Field label="Client id">
                  <input className="mono" value={clientId} onChange={(ev) => setClientId(ev.target.value)} />
                </Field>
              </div>
              <div className="grow" style={{ minWidth: 160 }}>
                <Field label="Client secret" hint={secretKept ? "Saved; leave empty to keep it." : undefined}>
                  <input type="password" className="mono" autoComplete="off" value={secret} placeholder={secretKept ? "•••••••• (saved)" : ""} onChange={(ev) => setSecret(ev.target.value)} />
                </Field>
              </div>
              <div className="grow" style={{ minWidth: 140 }}>
                <Field label="Scope (optional)">
                  <input className="mono" value={scope} onChange={(ev) => setScope(ev.target.value)} />
                </Field>
              </div>
            </div>
          </>
        )}
        {authKind === "" && platform && (
          <div className="row wrap" style={{ alignItems: "flex-start", marginTop: 10 }}>
            <div className="grow" style={{ minWidth: 200 }}>
              <Field label="Or: credential variable (platform only)" hint="In the voice agent's .env; must start with INTEGRATION_. Its value is sent as the header.">
                <input className="mono" value={authEnv} placeholder="INTEGRATION_CORE_BANKING_TOKEN" onChange={(ev) => setAuthEnv(ev.target.value.trim())} />
              </Field>
            </div>
            <div style={{ width: 170 }}>
              <Field label="Header">
                <input className="mono" value={authHeader} disabled={!authEnv} onChange={(ev) => setAuthHeader(ev.target.value.trim())} />
              </Field>
            </div>
          </div>
        )}
        <p className="hint" style={{ margin: "8px 0 0" }}>
          {addressChanged && hadSecret && !secret
            ? "The address changed, so enter the token again: a saved one is only ever sent where it was first meant to go."
            : "The token is sealed the moment you save, so only the voice agent can read it: not the database, not Flow Studio, not admins."}
        </p>
      </fieldset>

      <div className="row wrap" style={{ marginTop: 14 }}>
        <h3 className="grow" style={{ margin: 0 }}>
          Tools
        </h3>
        {kind === "mcp" && (
          <button className="small primary" disabled={!!busy || !url.trim() || (authKind !== "" && !secret)} onClick={load} title={authKind !== "" && !secret ? "Enter the token to connect" : undefined}>
            {busy === "load" ? "Connecting…" : rows.some((r) => r.fromServer) ? "Load again" : "Connect & load tools"}
          </button>
        )}
        <button className="small" onClick={() => {
          if (!asJson) setJson(JSON.stringify(rows.filter((r) => r.use).map((r) => r.tool), null, 2));
          setAsJson(!asJson);
        }}>
          {asJson ? "Pick from a list" : "Edit as JSON"}
        </button>
      </div>
      {found && (
        <p className="small-text muted">
          Connected to {found.server.name || "the server"}
          {found.server.version ? ` ${found.server.version}` : ""}: {found.tools.length} tool{found.tools.length === 1 ? "" : "s"}.
          {found.skipped.length > 0 && ` Left out: ${found.skipped.map((s) => `${s.name} (${s.reason})`).join("; ")}.`}
        </p>
      )}
      {asJson ? (
        <Field
          label="Tools (JSON)"
          hint={
            <>
              Each tool: name, description (the AI reads it), inputs (name, type, description, required), outputs (what an API step may keep),
              sensitive (needs a verified caller), locked (inputs the flow must set), max_calls
              {kind === "http" ? ", method (GET or POST; sensitive tools POST) and path, e.g. /cards/{card_last4}/balance" : ""}.
            </>
          }
        >
          <textarea className="mono" rows={12} value={json} onChange={(ev) => setJson(ev.target.value)} spellCheck={false} />
        </Field>
      ) : rows.length === 0 ? (
        <p className="muted small-text">{kind === "mcp" ? "Connect to load the server's tools, then choose which flows may use and set their rules." : "Edit the tools as JSON."}</p>
      ) : (
        <div className="tool-pick">
          {rows.map((r, k) => (
            <div key={r.tool.name} className={`tool-row${r.use ? "" : " off"}`}>
              <Check label={r.tool.name} checked={r.use} onChange={(v) => setRows((rs) => rs.map((x, i) => (i === k ? { ...x, use: v } : x)))} />
              <div className="hint">{r.tool.description || <em>No description: the AI needs one to know when to use it.</em>}</div>
              {r.use && (
                <div className="tool-rules">
                  <Check label="Needs a verified caller" checked={r.tool.sensitive} onChange={(v) => setTool(k, { sensitive: v })} />
                  {r.tool.inputs.length > 0 && (
                    <div className="small-text">
                      Set by the flow, never the AI:{" "}
                      {r.tool.inputs.map((x) => (
                        <label key={x.name} className="check inline">
                          <input
                            type="checkbox"
                            checked={r.tool.locked.includes(x.name)}
                            onChange={(ev) =>
                              setTool(k, (t) => ({ locked: ev.target.checked ? [...t.locked, x.name] : t.locked.filter((l) => l !== x.name) }))
                            }
                          />
                          <code>{x.name}</code>
                        </label>
                      ))}
                    </div>
                  )}
                  <div className="row wrap" style={{ gap: 10 }}>
                    <label className="small-text">
                      Returns (for API steps){" "}
                      <input
                        className="mono"
                        style={{ width: 220 }}
                        value={r.tool.outputs.join(", ")}
                        placeholder="e.g. balance, due_date"
                        onChange={(ev) =>
                          setTool(k, {
                            outputs: ev.target.value
                              .split(",")
                              .map((x) => x.trim())
                              .filter(Boolean),
                          })
                        }
                      />
                    </label>
                    <label className="small-text">
                      At most{" "}
                      <input type="number" min={1} max={20} style={{ width: 60 }} value={r.tool.max_calls} onChange={(ev) => setTool(k, { max_calls: Number(ev.target.value) || 1 })} /> per call
                    </label>
                  </div>
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      <div className="row wrap" style={{ alignItems: "flex-start", marginTop: 12 }}>
        <div className="grow" style={{ minWidth: 200 }}>
          <Field label="Name shown in flows">
            <input value={label} onChange={(ev) => setLabel(ev.target.value)} />
          </Field>
        </div>
        <div style={{ width: 130 }}>
          <Field label="Timeout (ms)">
            <input type="number" min={500} max={30000} value={timeout} onChange={(ev) => setTimeout_(ev.target.value)} />
          </Field>
        </div>
      </div>
      <Field label="Notes (optional)">
        <input value={notes} onChange={(ev) => setNotes(ev.target.value)} />
      </Field>
      {platform && (
        <Field label="Workspaces (optional)" hint="Comma-separated workspace ids. Empty = every workspace.">
          <input value={workspaces} onChange={(ev) => setWorkspaces(ev.target.value)} />
        </Field>
      )}
      {goesToReview && (
        <Field label="Note for the checker (optional)">
          <textarea rows={2} value={note} onChange={(ev) => setNote(ev.target.value)} placeholder="What it is and why the flows need it" />
        </Field>
      )}
      <Check label="Switched on (switching off stops its tools on every call at once)" checked={enabled} onChange={setEnabled} />
    </Dialog>
  );
}

// ── Reviewing a change ────────────────────────────────────────────────────────

/** What a change does, in words, for the checker. */
export function integrationChanges(live: IntegrationDef | null, next: IntegrationDef): { text: string; risk?: boolean }[] {
  const out: { text: string; risk?: boolean }[] = [];
  if (!live) {
    out.push({ text: `New ${next.kind === "mcp" ? "MCP server" : "REST API"} at ${next.url}`, risk: true });
    if (next.test_url) out.push({ text: `Test calls go to ${next.test_url}` });
    out.push({ text: `Signs in with ${authText(next)}` });
    for (const t of next.tools) {
      out.push({
        text: `Tool ${t.name}${t.sensitive ? " (needs a verified caller)" : ""}${t.locked.length ? `; set by the flow: ${t.locked.join(", ")}` : ""}`,
        // Picks whose data, yet the AI may fill it in on an unverified call.
        risk: !t.sensitive && t.inputs.some((x) => /card|account|dob|birth|pan|aadhaar|mobile|phone/i.test(x.name) && !t.locked.includes(x.name)),
      });
    }
    return out;
  }
  if (live.url !== next.url) out.push({ text: `Address: ${live.url} → ${next.url}`, risk: true });
  if ((live.test_url ?? "") !== (next.test_url ?? "")) out.push({ text: `Test address: ${live.test_url || "none"} → ${next.test_url || "none"}` });
  if (authText(live) !== authText(next) || live.auth?.token_url !== next.auth?.token_url) out.push({ text: `Sign-in: ${authText(live)} → ${authText(next)}`, risk: true });
  if (live.label !== next.label) out.push({ text: `Name: ${live.label} → ${next.label}` });
  if (live.timeout_ms !== next.timeout_ms) out.push({ text: `Timeout: ${live.timeout_ms} → ${next.timeout_ms} ms` });
  const before = new Map(live.tools.map((t) => [t.name, t]));
  const after = new Map(next.tools.map((t) => [t.name, t]));
  for (const t of next.tools) {
    const b = before.get(t.name);
    if (!b) {
      out.push({ text: `New tool ${t.name}${t.sensitive ? " (needs a verified caller)" : ""}`, risk: !t.sensitive });
      continue;
    }
    if (b.sensitive && !t.sensitive) out.push({ text: `${t.name} no longer needs a verified caller`, risk: true });
    if (!b.sensitive && t.sensitive) out.push({ text: `${t.name} now needs a verified caller` });
    const freed = b.locked.filter((l) => !t.locked.includes(l));
    if (freed.length) out.push({ text: `${t.name}: the AI may now choose ${freed.join(", ")}`, risk: true });
    const locked = t.locked.filter((l) => !b.locked.includes(l));
    if (locked.length) out.push({ text: `${t.name}: the flow must now set ${locked.join(", ")}` });
    if (b.description !== t.description) out.push({ text: `${t.name}: description changed` });
    if (b.max_calls !== t.max_calls) out.push({ text: `${t.name}: at most ${b.max_calls} → ${t.max_calls} per call` });
    if (b.outputs.join() !== t.outputs.join()) out.push({ text: `${t.name}: returns ${b.outputs.join(", ") || "nothing"} → ${t.outputs.join(", ") || "nothing"}` });
  }
  for (const t of live.tools) if (!after.has(t.name)) out.push({ text: `Removes tool ${t.name} (flows using it stop working)`, risk: true });
  if (live.enabled !== next.enabled) out.push({ text: next.enabled ? "Switches it on" : "Switches it off" });
  if (out.length === 0) out.push({ text: "No changes to what live calls use" });
  return out;
}

export function IntegrationReview(props: { me: Me; integration: Integration; onClose: () => void; onDecided: (message: string) => void }) {
  const i = props.integration;
  const next = i.pending!;
  const changes = useMemo(() => integrationChanges(i.live ? i : null, next), [i, next]);
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string[]>([]);

  const decide = async (approve: boolean) => {
    setBusy(true);
    try {
      if (approve) {
        const res = await api.approveIntegration(i.integration_id, note.trim());
        props.onDecided(`${next.label} approved: live calls use it now.${res.used_by.length ? ` Used by ${res.used_by.join(", ")}.` : ""}`);
      } else {
        await api.rejectIntegration(i.integration_id, note.trim());
        props.onDecided(`${next.label} sent back to ${i.pending_by}.`);
      }
    } catch (err) {
      setError(err instanceof ApiError && err.problems.length ? err.problems : [(err as Error).message]);
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={`Review: ${next.label}`}
      onClose={props.onClose}
      footer={
        <>
          <button onClick={props.onClose}>Cancel</button>
          <button className="danger" disabled={busy || !note.trim()} title={note.trim() ? undefined : "Say what needs to change"} onClick={() => decide(false)}>
            Turn down
          </button>
          <button className="primary" disabled={busy} onClick={() => decide(true)}>
            Approve
          </button>
        </>
      }
    >
      {error.length > 0 && (
        <div className="notice bad" role="alert">
          {error.map((p, k) => (
            <div key={k}>{p}</div>
          ))}
        </div>
      )}
      <p className="small-text">
        <strong>{i.pending_by}</strong> sent {i.live ? "a change to" : "a new integration for"} workspace <code>{i.owner_workspace || "platform"}</code>{" "}
        {timeAgo(i.pending_at)}
        {i.pending_note ? `: “${i.pending_note}”` : "."}
      </p>
      <ul className="change-list">
        {changes.map((c, k) => (
          <li key={k} className={c.risk ? "risk" : undefined}>
            {c.text}
          </li>
        ))}
      </ul>
      <p className="hint">
        Check the address is your team's server, tools that read a customer's data need a verified caller, and inputs that pick whose data (card,
        account, date of birth) are set by the flow. The token isn't shown to anyone; approving lets live calls use it.
      </p>
      <ToolTable tools={next.tools} />
      <Field label="Note (required to turn it down)">
        <textarea rows={2} value={note} onChange={(ev) => setNote(ev.target.value)} />
      </Field>
    </Dialog>
  );
}

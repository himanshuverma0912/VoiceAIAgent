// Platform admins: the models flows may use. Teams pick from these in a flow's
// Settings; a draft naming anything else can't be test-called or published.
import { useEffect, useState } from "react";
import { ApiError, api, type CatalogEntry, type CatalogInput, type CatalogKind } from "../api";
import { Dialog, Field, toast, timeAgo } from "../components/ui";

const KINDS: { kind: CatalogKind; title: string; idLabel: string; idHint: string; about: string }[] = [
  {
    kind: "llm",
    title: "LLMs",
    idLabel: "LiteLLM alias",
    idHint: "Must exist in infra/litellm-config.yaml first (model_name), e.g. voice-fast.",
    about: "Aliases set up in LiteLLM. Which real model sits behind an alias is changed there, with no flow changes.",
  },
  {
    kind: "stt",
    title: "Speech-to-text",
    idLabel: "Deepgram model",
    idHint: "e.g. nova-3 or nova-2.",
    about: "Deepgram models, used through LiteLLM.",
  },
  {
    kind: "tts_voice",
    title: "Voices",
    idLabel: "Cartesia voice id",
    idHint: "From play.cartesia.ai (the voice's id).",
    about: "Cartesia voices. List the languages a voice suits so flows in other languages get a warning.",
  },
];

type Editing = { kind: CatalogKind; entry: CatalogEntry | null };

const list = (s: string) =>
  s
    .split(",")
    .map((x) => x.trim())
    .filter(Boolean);

export function ModelCatalog() {
  const [entries, setEntries] = useState<CatalogEntry[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<Editing | null>(null);

  const load = () => api.catalog().then(setEntries, (e) => setError((e as Error).message));
  useEffect(() => {
    load();
  }, []);

  const toggle = async (e: CatalogEntry) => {
    try {
      const input: CatalogInput = { label: e.label, notes: e.notes, languages: e.languages, teams: e.teams, enabled: !e.enabled };
      const res = await api.putCatalog(e.kind, e.model_id, input);
      toast(
        e.enabled
          ? `${e.label} switched off.${res.used_by.length ? ` Still in ${res.used_by.join(", ")}: their next publish needs another model.` : ""}`
          : `${e.label} switched on.`,
      );
      load();
    } catch (err) {
      toast((err as Error).message);
    }
  };

  const remove = async (e: CatalogEntry) => {
    if (!confirm(`Remove ${e.label} (${e.model_id}) from the catalog?`)) return;
    try {
      await api.deleteCatalog(e.kind, e.model_id);
      toast(`${e.label} removed.`);
      load();
    } catch (err) {
      toast((err as Error).message);
    }
  };

  return (
    <main className="page wide">
      <h1>Model catalog</h1>
      <p className="muted" style={{ margin: "4px 0 0" }}>
        The models teams can choose in a flow's Settings. A flow naming anything else, something switched off, or something not
        offered to its team can't be test-called or published. Flows that leave a setting empty use the agent's default.
      </p>
      {error && <div className="notice bad" style={{ marginTop: 16 }}>{error}</div>}
      {!entries && !error && <p className="muted">Loading…</p>}
      {entries &&
        KINDS.map((k) => {
          const rows = entries.filter((e) => e.kind === k.kind);
          return (
            <section key={k.kind} className="card calls-section">
              <div className="row">
                <h2 className="grow">{k.title}</h2>
                <button className="small" onClick={() => setEditing({ kind: k.kind, entry: null })}>
                  Add
                </button>
              </div>
              <p className="muted small-text">{k.about}</p>
              {rows.length === 0 ? (
                <p className="hint">None yet: every flow uses the agent's default.</p>
              ) : (
                <div className="table-wrap">
                  <table className="data">
                    <thead>
                      <tr>
                        <th>Name</th>
                        <th>{k.idLabel}</th>
                        <th>Languages</th>
                        <th>Teams</th>
                        <th>Status</th>
                        <th>Changed</th>
                        <th />
                      </tr>
                    </thead>
                    <tbody>
                      {rows.map((e) => (
                        <tr key={e.model_id}>
                          <td>
                            <strong>{e.label}</strong>
                            {e.notes && <div className="hint">{e.notes}</div>}
                          </td>
                          <td>
                            <code>{e.model_id}</code>
                          </td>
                          <td>{e.languages.length ? e.languages.join(", ") : <span className="muted">any</span>}</td>
                          <td>{e.teams.length ? e.teams.join(", ") : <span className="muted">every team</span>}</td>
                          <td>{e.enabled ? <span className="chip good">on</span> : <span className="chip">off</span>}</td>
                          <td className="muted" title={e.updated_by}>
                            {timeAgo(e.updated_at)}
                          </td>
                          <td className="num">
                            <div className="row" style={{ justifyContent: "flex-end" }}>
                              <button className="small" onClick={() => setEditing({ kind: e.kind, entry: e })}>
                                Edit
                              </button>
                              <button className="small" onClick={() => toggle(e)}>
                                {e.enabled ? "Switch off" : "Switch on"}
                              </button>
                              <button className="small danger" onClick={() => remove(e)}>
                                Remove
                              </button>
                            </div>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </section>
          );
        })}
      {editing && (
        <EntryDialog
          editing={editing}
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

function EntryDialog(props: { editing: Editing; onClose: () => void; onSaved: () => void }) {
  const k = KINDS.find((x) => x.kind === props.editing.kind)!;
  const e = props.editing.entry;
  const [id, setId] = useState(e?.model_id ?? "");
  const [label, setLabel] = useState(e?.label ?? "");
  const [notes, setNotes] = useState(e?.notes ?? "");
  const [languages, setLanguages] = useState((e?.languages ?? []).join(", "));
  const [teams, setTeams] = useState((e?.teams ?? []).join(", "));
  const [enabled, setEnabled] = useState(e?.enabled ?? true);
  const [problems, setProblems] = useState<string[]>([]);
  const [saving, setSaving] = useState(false);

  const save = async () => {
    setSaving(true);
    const input: CatalogInput = { label, notes, languages: list(languages), teams: list(teams), enabled };
    try {
      const res = await api.putCatalog(k.kind, id.trim(), input);
      toast(res.created ? `${label} added.` : `${label} saved.`);
      props.onSaved();
    } catch (err) {
      setProblems(err instanceof ApiError && err.problems.length ? err.problems : [(err as Error).message]);
      setSaving(false);
    }
  };

  return (
    <Dialog
      narrow
      title={e ? `Edit ${e.label}` : `Add to ${k.title}`}
      onClose={props.onClose}
      footer={
        <>
          <button onClick={props.onClose}>Cancel</button>
          <button className="primary" disabled={saving || !id.trim() || !label.trim()} onClick={save}>
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
      <Field label={k.idLabel} hint={e ? "Can't be changed; add a new entry instead." : k.idHint}>
        <input className="mono" value={id} disabled={!!e} onChange={(ev) => setId(ev.target.value)} autoFocus={!e} />
      </Field>
      <Field label="Name shown to teams">
        <input value={label} onChange={(ev) => setLabel(ev.target.value)} autoFocus={!!e} />
      </Field>
      <Field label="Notes (optional)" hint="When to use it, speed or cost. Shown under the dropdown.">
        <input value={notes} onChange={(ev) => setNotes(ev.target.value)} />
      </Field>
      <Field label="Languages (optional)" hint="Comma-separated codes, e.g. hi, en. Empty = any language.">
        <input value={languages} onChange={(ev) => setLanguages(ev.target.value)} />
      </Field>
      <Field label="Teams (optional)" hint="Comma-separated, e.g. emi, complaints. Empty = every team.">
        <input value={teams} onChange={(ev) => setTeams(ev.target.value)} />
      </Field>
      <label className="check">
        <input type="checkbox" checked={enabled} onChange={(ev) => setEnabled(ev.target.checked)} />
        Switched on (teams can choose it)
      </label>
    </Dialog>
  );
}

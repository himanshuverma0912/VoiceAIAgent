import { diffWords } from "diff";
import { useEffect, useMemo, useState } from "react";
import { ApiError, api, type Version } from "../api";
import { changesBetween, readablePath, type Change } from "../diff";
import { problemText } from "../spec";
import { Dialog, timeAgo } from "./ui";

export function ChangeList({ changes }: { changes: Change[] }) {
  if (changes.length === 0) return <p className="muted">No differences.</p>;
  return (
    <div>
      {changes.map((c, i) => (
        <div key={i} className="change">
          <div className="change-head">
            <span className={`chip ${c.kind === "added" ? "good" : c.kind === "removed" ? "bad" : "warn"}`}>{c.kind}</span>
            <code className="grow">{readablePath(c.path)}</code>
          </div>
          <div className="change-body">
            {c.kind === "changed" ? (
              diffWords(c.before ?? "", c.after ?? "").map((part, j) =>
                part.added ? <ins key={j}>{part.value}</ins> : part.removed ? <del key={j}>{part.value}</del> : <span key={j}>{part.value}</span>,
              )
            ) : c.kind === "added" ? (
              <ins>{c.after}</ins>
            ) : (
              <del>{c.before}</del>
            )}
          </div>
        </div>
      ))}
    </div>
  );
}

/** Review what will go live, then publish. */
export function PublishDialog(props: {
  flowId: string;
  spec: unknown;
  latestVersion: number | null;
  problems: string[];
  onClose: () => void;
  onPublished: (version: number) => void;
}) {
  const [live, setLive] = useState<unknown | null | undefined>(props.latestVersion ? undefined : null);
  const [error, setError] = useState<string | null>(null);
  const [serverProblems, setServerProblems] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (props.latestVersion)
      api.version(props.flowId, props.latestVersion).then(
        (v) => setLive(v.spec),
        (e) => setError((e as Error).message),
      );
  }, [props.flowId, props.latestVersion]);

  const changes = useMemo(() => (live === undefined ? null : changesBetween(live, props.spec)), [live, props.spec]);
  const problems = serverProblems.length ? serverProblems : props.problems;
  const next = (props.latestVersion ?? 0) + 1;

  const publish = async () => {
    setBusy(true);
    setError(null);
    try {
      const res = await api.publish(props.flowId);
      props.onPublished(res.version);
    } catch (e) {
      if (e instanceof ApiError && e.problems.length) setServerProblems(e.problems);
      setError((e as Error).message);
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={props.latestVersion ? `Review changes since v${props.latestVersion}` : "Review before the first publish"}
      onClose={props.onClose}
      footer={
        <>
          <span className="muted grow" style={{ fontSize: 13 }}>
            Calls to numbers that follow this flow use v{next} from their next call.
          </span>
          <button onClick={props.onClose}>Cancel</button>
          <button className="primary" disabled={busy || problems.length > 0 || !changes || changes.length === 0} onClick={publish}>
            {busy ? "Publishing…" : `Publish v${next}`}
          </button>
        </>
      }
    >
      {problems.length > 0 && (
        <>
          <div className="notice bad">Fix these first; a flow with problems can't go live.</div>
          <ul className="problems">
            {problems.map((p, i) => (
              <li key={i}>
                <strong>{problemText(p).where}</strong>: {problemText(p).what}
              </li>
            ))}
          </ul>
        </>
      )}
      {error && !serverProblems.length && <div className="notice bad">{error}</div>}
      {changes === null ? (
        <p className="muted">Loading the live version…</p>
      ) : changes.length === 0 ? (
        <div className="notice info">Nothing changed since v{props.latestVersion}; it's already live.</div>
      ) : (
        <>
          <p className="muted" style={{ marginTop: 0 }}>
            {changes.length} change{changes.length === 1 ? "" : "s"}
            {props.latestVersion ? ` compared with v${props.latestVersion}` : ""}.
          </p>
          <ChangeList changes={changes} />
        </>
      )}
    </Dialog>
  );
}

/** Published versions: who and when, what differs from the draft, restore one. */
export function VersionsDialog(props: {
  flowId: string;
  draft: unknown;
  editable: boolean;
  onClose: () => void;
  onRestore: (spec: unknown, version: number) => void;
}) {
  const [versions, setVersions] = useState<Version[] | null>(null);
  const [picked, setPicked] = useState<Version | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api.versions(props.flowId).then(setVersions, (e) => setError((e as Error).message));
  }, [props.flowId]);

  const pick = async (v: Version) => {
    try {
      setPicked(await api.version(props.flowId, v.version));
    } catch (e) {
      setError((e as Error).message);
    }
  };

  return (
    <Dialog
      title="Version history"
      onClose={props.onClose}
      footer={
        picked && props.editable ? (
          <>
            <span className="muted grow" style={{ fontSize: 13 }}>
              Replaces the draft in the editor; nothing goes live until you publish.
            </span>
            <button className="primary" onClick={() => props.onRestore(picked.spec, picked.version)}>
              Copy v{picked.version} into the draft
            </button>
          </>
        ) : undefined
      }
    >
      {error && <div className="notice bad">{error}</div>}
      {!versions && !error && <p className="muted">Loading…</p>}
      {versions && versions.length === 0 && <p className="muted">Not published yet.</p>}
      {versions && versions.length > 0 && (
        <div className="versions">
          {versions.map((v, i) => (
            <button key={v.version} className="version-row" aria-pressed={picked?.version === v.version} onClick={() => pick(v)}>
              <strong>v{v.version}</strong>
              {i === 0 && <span className="chip good">latest</span>}
              <span className="grow muted">
                {v.published_by} · {timeAgo(v.published_at)}
              </span>
              <code className="faint">{v.checksum.slice(0, 8)}</code>
            </button>
          ))}
        </div>
      )}
      {picked && (
        <div className="section">
          <h3 style={{ marginBottom: 8 }}>Draft compared with v{picked.version}</h3>
          <ChangeList changes={changesBetween(picked.spec, props.draft)} />
        </div>
      )}
    </Dialog>
  );
}

import { diffWords } from "diff";
import { useEffect, useMemo, useState } from "react";
import { ApiError, api, can, type Me, type Review, type ReviewRef, type Version } from "../api";
import { changesBetween, readablePath, type Change } from "../diff";
import { problemText } from "../spec";
import { Dialog, Field, timeAgo } from "./ui";

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

function ProblemList({ problems }: { problems: string[] }) {
  return (
    <ul className="problems">
      {problems.map((p, i) => (
        <li key={i}>
          <strong>{problemText(p).where}</strong>: {problemText(p).what}
        </li>
      ))}
    </ul>
  );
}

/** Loads a version's flow (null = nothing published yet). */
function useVersionSpec(flowId: string, version: number | null) {
  const [spec, setSpec] = useState<unknown | null | undefined>(version ? undefined : null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    if (version)
      api.version(flowId, version).then(
        (v) => setSpec(v.spec),
        (e) => setError((e as Error).message),
      );
  }, [flowId, version]);
  return { spec, error };
}

/** Maker: review the changes against the live version and submit them for a checker's approval. */
export function SubmitDialog(props: {
  flowId: string;
  spec: unknown;
  latestVersion: number | null;
  problems: string[];
  pending: ReviewRef | null;
  onClose: () => void;
  onSubmitted: (review: Review) => void;
}) {
  const live = useVersionSpec(props.flowId, props.latestVersion);
  const [note, setNote] = useState("");
  const [error, setError] = useState<string | null>(live.error);
  const [serverProblems, setServerProblems] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);

  const changes = useMemo(() => (live.spec === undefined ? null : changesBetween(live.spec, props.spec)), [live.spec, props.spec]);
  const problems = serverProblems.length ? serverProblems : props.problems;

  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      props.onSubmitted(await api.submitReview(props.flowId, note.trim()));
    } catch (e) {
      if (e instanceof ApiError && e.problems.length) setServerProblems(e.problems);
      setError((e as Error).message);
      setBusy(false);
    }
  };

  return (
    <Dialog
      title="Submit for review"
      onClose={props.onClose}
      footer={
        <>
          <span className="muted grow" style={{ fontSize: 13 }}>
            A checker in this workspace approves it; then it goes live on the next call.
          </span>
          <button onClick={props.onClose}>Cancel</button>
          <button className="primary" disabled={busy || problems.length > 0 || !changes || changes.length === 0} onClick={submit}>
            {busy ? "Submitting…" : "Submit for review"}
          </button>
        </>
      }
    >
      {problems.length > 0 && (
        <>
          <div className="notice bad">Fix these first; a flow with problems can't be submitted.</div>
          <ProblemList problems={problems} />
        </>
      )}
      {error && !serverProblems.length && <div className="notice bad">{error}</div>}
      {props.pending && (
        <div className="notice warn">
          {props.pending.submitted_by} submitted this flow {timeAgo(props.pending.submitted_at)} and it's waiting for a checker.
          Submitting again replaces that submission with this draft.
        </div>
      )}
      <Field label="What changed, and why" hint="The checker reads this first. Plain words are best.">
        <textarea rows={3} value={note} maxLength={2000} onChange={(e) => setNote(e.target.value)} autoFocus />
      </Field>
      {changes === null ? (
        <p className="muted">Loading the live version…</p>
      ) : changes.length === 0 ? (
        <div className="notice info">Nothing changed since v{props.latestVersion}; there's nothing to review.</div>
      ) : (
        <>
          <h3 style={{ margin: "4px 0 8px" }}>
            {changes.length} change{changes.length === 1 ? "" : "s"}
            {props.latestVersion ? ` compared with v${props.latestVersion} (live)` : " (first version)"}
          </h3>
          <ChangeList changes={changes} />
        </>
      )}
    </Dialog>
  );
}

/** Checker: see exactly what was submitted, then approve (publishes it) or reject with a reason.
 * The maker who submitted it sees it too, and can withdraw it. */
export function ReviewDialog(props: {
  me: Me;
  flowId: string;
  workspace: string;
  reviewId: number;
  latestVersion: number | null;
  onClose: () => void;
  onDecided: (message: string) => void;
}) {
  const [review, setReview] = useState<Review | null>(null);
  const [base, setBase] = useState<unknown | null | undefined>(undefined);
  const [note, setNote] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [problems, setProblems] = useState<string[]>([]);
  const [busy, setBusy] = useState<null | "approve" | "reject" | "withdraw">(null);

  useEffect(() => {
    api.review(props.flowId, props.reviewId).then(
      async (r) => {
        setReview(r);
        setBase(r.base_version ? (await api.version(props.flowId, r.base_version)).spec : null);
      },
      (e) => setError((e as Error).message),
    );
  }, [props.flowId, props.reviewId]);

  const changes = useMemo(() => (review && base !== undefined ? changesBetween(base, review.spec) : null), [review, base]);
  const mine = review?.submitted_by === props.me.email;
  const canDecide = !!review && review.status === "pending" && can.approve(props.me, props.workspace) && !mine;
  const next = (props.latestVersion ?? 0) + 1;

  const act = async (kind: "approve" | "reject" | "withdraw") => {
    setBusy(kind);
    setError(null);
    try {
      if (kind === "approve") {
        const v = await api.approve(props.flowId, props.reviewId, note.trim());
        props.onDecided(`Approved: v${v.version} is live from the next call.`);
      } else if (kind === "reject") {
        await api.reject(props.flowId, props.reviewId, note.trim());
        props.onDecided(`Rejected; ${review?.submitted_by} sees your reason.`);
      } else {
        await api.withdraw(props.flowId, props.reviewId);
        props.onDecided("Submission withdrawn.");
      }
    } catch (e) {
      if (e instanceof ApiError && e.problems.length) setProblems(e.problems);
      setError((e as Error).message);
      setBusy(null);
    }
  };

  return (
    <Dialog
      title="Review submission"
      onClose={props.onClose}
      footer={
        review?.status !== "pending" ? undefined : canDecide ? (
          <>
            <span className="muted grow" style={{ fontSize: 13 }}>
              Approving publishes exactly this submission as v{next}.
            </span>
            <button className="danger" disabled={!!busy || !note.trim()} title={note.trim() ? "" : "Write the reason first"} onClick={() => act("reject")}>
              {busy === "reject" ? "Rejecting…" : "Reject"}
            </button>
            <button className="primary" disabled={!!busy || problems.length > 0} onClick={() => act("approve")}>
              {busy === "approve" ? "Publishing…" : `Approve & publish v${next}`}
            </button>
          </>
        ) : mine ? (
          <>
            <span className="muted grow" style={{ fontSize: 13 }}>
              You submitted this, so another checker has to approve it.
            </span>
            <button disabled={!!busy} onClick={() => act("withdraw")}>
              {busy === "withdraw" ? "Withdrawing…" : "Withdraw"}
            </button>
          </>
        ) : (
          <span className="muted" style={{ fontSize: 13 }}>
            Waiting for a checker in this workspace.
          </span>
        )
      }
    >
      {error && <div className="notice bad">{error}</div>}
      {problems.length > 0 && <ProblemList problems={problems} />}
      {!review && !error && <p className="muted">Loading…</p>}
      {review && (
        <>
          <div className="review-head">
            <div>
              <strong>{review.submitted_by}</strong> submitted this {timeAgo(review.submitted_at)}
              {review.base_version ? ` (changes to v${review.base_version})` : " (first version)"}.
            </div>
            {review.note ? <blockquote className="review-note">{review.note}</blockquote> : <p className="muted">No note.</p>}
            {review.status !== "pending" && (
              <div className="notice info">
                {review.status} by {review.decided_by}
                {review.decided_at ? ` ${timeAgo(review.decided_at)}` : ""}
                {review.decision_note ? `: “${review.decision_note}”` : ""}
              </div>
            )}
          </div>
          {canDecide && (
            <Field label="Your comment" hint="Needed to reject (the maker sees it); optional when approving.">
              <textarea rows={2} value={note} maxLength={2000} onChange={(e) => setNote(e.target.value)} />
            </Field>
          )}
          {changes === null ? (
            <p className="muted">Loading the changes…</p>
          ) : (
            <>
              <h3 style={{ margin: "8px 0" }}>
                {changes.length} change{changes.length === 1 ? "" : "s"}
                {review.base_version ? ` compared with v${review.base_version}` : ""}
              </h3>
              <ChangeList changes={changes} />
            </>
          )}
        </>
      )}
    </Dialog>
  );
}

const REVIEW_CHIP: Record<Review["status"], string> = { pending: "warn", approved: "good", rejected: "bad", withdrawn: "", superseded: "" };

/** Published versions: who and when, what differs from the draft, restore one. */
export function VersionsDialog(props: {
  flowId: string;
  draft: unknown;
  editable: boolean;
  onClose: () => void;
  onRestore: (spec: unknown, version: number) => void;
}) {
  const [versions, setVersions] = useState<Version[] | null>(null);
  const [reviews, setReviews] = useState<Review[] | null>(null);
  const [picked, setPicked] = useState<Version | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api.versions(props.flowId).then(setVersions, (e) => setError((e as Error).message));
    api.reviews(props.flowId).then(setReviews, () => setReviews([]));
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
              Replaces the draft in the editor; nothing goes live until it's submitted and approved.
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
                {v.submitted_by ? `${v.submitted_by} → approved by ${v.published_by}` : v.published_by} · {timeAgo(v.published_at)}
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
      {reviews && reviews.length > 0 && (
        <div className="section">
          <h3 style={{ marginBottom: 8 }}>Reviews</h3>
          <table className="data">
            <tbody>
              {reviews.map((r) => (
                <tr key={r.review_id}>
                  <td>
                    <span className={`chip ${REVIEW_CHIP[r.status]}`}>{r.status}</span>
                  </td>
                  <td>
                    {r.submitted_by} · {timeAgo(r.submitted_at)}
                    {r.note && <div className="hint">“{r.note}”</div>}
                  </td>
                  <td>
                    {r.decided_by && `${r.decided_by}${r.version ? ` → v${r.version}` : ""}`}
                    {r.decision_note && <div className="hint">“{r.decision_note}”</div>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Dialog>
  );
}

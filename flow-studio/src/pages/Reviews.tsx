// Reviews: changes makers have submitted that are waiting for a checker.
import { useEffect, useState } from "react";
import { api, can, type FlowSummary, type Me, type Review } from "../api";
import { ReviewDialog } from "../components/Review";
import { timeAgo, toast } from "../components/ui";
import { Link, usePath } from "../router";
import { workspaceNames } from "./FlowList";

const CHANGED = "flow-studio.reviews-changed";

/** Tell the top bar's count to refresh (after a decision). */
export const reviewsChanged = () => window.dispatchEvent(new Event(CHANGED));

/** How many submissions are waiting for this person to decide (for the top bar). */
export function useReviewCount(me: Me): number {
  const [count, setCount] = useState(0);
  const path = usePath();
  const checksSomewhere = can.admin(me) || me.workspaces.some((m) => m.checker);
  useEffect(() => {
    if (!checksSomewhere) return;
    let live = true;
    const load = () =>
      api.pendingReviews().then(
        (rs) => live && setCount(rs.filter((r) => r.for_me).length),
        () => {},
      );
    load();
    const timer = setInterval(load, 60_000);
    window.addEventListener(CHANGED, load);
    return () => {
      live = false;
      clearInterval(timer);
      window.removeEventListener(CHANGED, load);
    };
  }, [checksSomewhere, path]);
  return count;
}

export function Reviews({ me }: { me: Me }) {
  const [reviews, setReviews] = useState<Review[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState<{ review: Review; flow: FlowSummary } | null>(null);
  const names = workspaceNames(me);

  const load = () => api.pendingReviews().then(setReviews, (e) => setError((e as Error).message));
  useEffect(() => {
    load();
  }, []);

  const show = async (review: Review) => {
    try {
      setOpen({ review, flow: await api.flow(review.flow_id) });
    } catch (e) {
      toast((e as Error).message);
    }
  };

  const mine = reviews?.filter((r) => r.for_me) ?? [];
  const others = reviews?.filter((r) => !r.for_me) ?? [];

  const list = (rows: Review[]) => (
    <div className="flow-list">
      {rows.map((r) => (
        <button key={r.review_id} className="flow-item" style={{ textAlign: "left", font: "inherit" }} onClick={() => show(r)}>
          <div>
            <strong>{r.flow_name}</strong>
            <div className="meta">
              <span>{names[r.workspace] ?? r.workspace}</span>
              <span>
                {r.submitted_by === me.email ? "you" : r.submitted_by} · {timeAgo(r.submitted_at)}
              </span>
            </div>
            {r.note && (
              <div className="muted" style={{ fontSize: 13, marginTop: 4 }}>
                “{r.note.length > 160 ? r.note.slice(0, 160) + "…" : r.note}”
              </div>
            )}
          </div>
          <div className="meta" style={{ justifyContent: "flex-end" }}>
            <span className="chip">{r.base_version ? `changes to v${r.base_version}` : "first version"}</span>
            {r.for_me ? <span className="chip warn">yours to decide</span> : r.submitted_by === me.email ? <span className="chip">yours</span> : null}
          </div>
        </button>
      ))}
    </div>
  );

  return (
    <main className="page">
      <h1>Reviews</h1>
      <p className="muted" style={{ margin: "4px 0 0" }}>
        Makers submit changes; a checker in the same workspace, who isn't the maker, approves them to make them live.
      </p>
      {error && <div className="notice bad" style={{ marginTop: 16 }}>{error}</div>}
      {!reviews && !error && <p className="muted">Loading…</p>}
      {reviews && reviews.length === 0 && (
        <div className="card" style={{ marginTop: 18 }}>
          <p style={{ margin: 0 }}>Nothing is waiting for review in your workspaces.</p>
        </div>
      )}
      {mine.length > 0 && (
        <section className="ws-group">
          <div className="ws-group-head">
            <h2>Waiting for you</h2>
            <span className="muted">{mine.length}</span>
          </div>
          {list(mine)}
        </section>
      )}
      {others.length > 0 && (
        <section className="ws-group">
          <div className="ws-group-head">
            <h2>{mine.length ? "Also waiting" : "Waiting for a checker"}</h2>
            <span className="muted">{others.length}</span>
          </div>
          {list(others)}
        </section>
      )}
      {open && (
        <ReviewDialog
          me={me}
          flowId={open.review.flow_id}
          workspace={open.review.workspace}
          reviewId={open.review.review_id}
          latestVersion={open.flow.latest_version}
          onClose={() => setOpen(null)}
          onDecided={(message) => {
            setOpen(null);
            toast(message);
            reviewsChanged();
            load();
          }}
        />
      )}
      <p className="muted" style={{ fontSize: 13, marginTop: 24 }}>
        A flow's full review history is under History in its editor. <Link to="/">Back to flows</Link>
      </p>
    </main>
  );
}

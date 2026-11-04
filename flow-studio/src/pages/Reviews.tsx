// Reviews: changes makers have submitted that are waiting for a checker.
import { useEffect, useState } from "react";
import { api, can, integrationForMe, type FlowSummary, type Integration, type Me, type Review } from "../api";
import { ReviewDialog } from "../components/Review";
import { PageHero, timeAgo, toast } from "../components/ui";
import { Link, navigate, usePath } from "../router";
import { workspaceNames } from "./FlowList";
import { IntegrationReview } from "./Integrations";

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
      Promise.all([api.pendingReviews(), api.integrations().catch(() => [] as Integration[])]).then(
        ([rs, is]) => live && setCount(rs.filter((r) => r.for_me).length + is.filter((i) => integrationForMe(me, i)).length),
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
  }, [checksSomewhere, path, me]);
  return count;
}

export function Reviews({ me }: { me: Me }) {
  const [reviews, setReviews] = useState<Review[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState<{ review: Review; flow: FlowSummary } | null>(null);
  const [integrations, setIntegrations] = useState<Integration[]>([]);
  const [reviewing, setReviewing] = useState<Integration | null>(null);
  const names = workspaceNames(me);

  const load = () => {
    api.pendingReviews().then(setReviews, (e) => setError((e as Error).message));
    api.integrations().then((is) => setIntegrations(is.filter((i) => i.pending)), () => {});
  };
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
  const integrationsForMe = integrations.filter((i) => integrationForMe(me, i));
  const otherIntegrations = integrations.filter((i) => !integrationForMe(me, i));

  const integrationList = (rows: Integration[]) => (
    <div className="flow-list">
      {rows.map((i) => (
        <button
          key={i.integration_id}
          className="flow-item"
          style={{ textAlign: "left", font: "inherit" }}
          onClick={() => (integrationForMe(me, i) ? setReviewing(i) : navigate("/integrations"))}
        >
          <div>
            <strong>{i.pending?.label ?? i.label}</strong>
            <div className="meta">
              <span>{names[i.owner_workspace ?? ""] ?? i.owner_workspace ?? "platform"}</span>
              <span>
                {i.pending_by === me.email ? "you" : i.pending_by} · {timeAgo(i.pending_at)}
              </span>
            </div>
            {i.pending_note && (
              <div className="muted" style={{ fontSize: 13, marginTop: 4 }}>
                “{i.pending_note.length > 160 ? i.pending_note.slice(0, 160) + "…" : i.pending_note}”
              </div>
            )}
          </div>
          <div className="meta" style={{ justifyContent: "flex-end" }}>
            <span className="chip">{i.live ? "integration change" : "new integration"}</span>
            {integrationForMe(me, i) ? <span className="chip warn">yours to decide</span> : i.pending_by === me.email ? <span className="chip">yours</span> : null}
          </div>
        </button>
      ))}
    </div>
  );

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
      <PageHero title="Reviews">
        Makers submit changes; a checker in the same workspace, who isn't the maker, approves them to make them live.
      </PageHero>
      {error && <div className="notice bad" style={{ marginTop: 16 }}>{error}</div>}
      {!reviews && !error && <p className="muted">Loading…</p>}
      {reviews && reviews.length === 0 && integrations.length === 0 && (
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
      {integrationsForMe.length > 0 && (
        <section className="ws-group">
          <div className="ws-group-head">
            <h2>Integrations waiting for you</h2>
            <span className="muted">{integrationsForMe.length}</span>
          </div>
          {integrationList(integrationsForMe)}
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
      {otherIntegrations.length > 0 && (
        <section className="ws-group">
          <div className="ws-group-head">
            <h2>Integrations waiting for a checker</h2>
            <span className="muted">{otherIntegrations.length}</span>
          </div>
          {integrationList(otherIntegrations)}
        </section>
      )}
      {reviewing && (
        <IntegrationReview
          me={me}
          integration={reviewing}
          onClose={() => setReviewing(null)}
          onDecided={(message) => {
            setReviewing(null);
            toast(message);
            reviewsChanged();
            load();
          }}
        />
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

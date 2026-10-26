// How calls on one flow went: outcomes, where callers drop off, reply speed,
// and each version's results. Counts and timings only: the config service never
// has a caller's number, anything said, or any value collected.
import { useEffect, useState } from "react";
import { api, type Call, type CallSummary, type Channel, type FlowSummary, type Outcome } from "../api";
import { timeAgo } from "../components/ui";
import { Link } from "../router";

const OUTCOME: Record<Outcome, { label: string; tone: string; help: string }> = {
  completed: { label: "Completed", tone: "good", help: "Reached an end step and the agent said goodbye" },
  ended_early: { label: "Ended early", tone: "", help: "The caller was done before an end step; the agent closed the call" },
  caller_hung_up: { label: "Caller hung up", tone: "warn", help: "The caller left before the agent closed the call" },
  time_limit: { label: "Time limit", tone: "", help: "A test call stopped at 5 minutes" },
  error: { label: "Error", tone: "bad", help: "A model or the connection failed" },
  other: { label: "Other", tone: "", help: "Ended some other way (e.g. a deploy)" },
};
const OUTCOME_ORDER: Outcome[] = ["completed", "ended_early", "caller_hung_up", "time_limit", "error", "other"];

const PERIODS = [1, 7, 30, 90];
const CHANNELS: { value: Channel; label: string }[] = [
  { value: "phone", label: "Phone calls" },
  { value: "studio_test", label: "Test calls" },
  { value: "all", label: "All" },
];

function seconds(ms: number | null | undefined): string {
  if (ms == null) return "–";
  if (ms < 1000) return `${ms} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)} s`;
  const m = Math.floor(s / 60);
  return `${m} min ${Math.round(s % 60)} s`;
}

/** "12 min ago" today; "25 Oct, 14:32" before that. */
function when(iso: string): string {
  if (Date.now() - new Date(iso).getTime() < 86400_000) return timeAgo(iso);
  return new Date(iso).toLocaleString(undefined, { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
}

function pct(n: number, of: number): string {
  return of ? `${Math.round((100 * n) / of)}%` : "–";
}

function versionLabel(version: number | null, source: string): string {
  if (version != null) return `v${version}`;
  return source === "studio-draft" ? "draft" : source === "fallback-file" ? "built-in file" : source;
}

function readPrefs(): { days: number; channel: Channel } {
  try {
    const p = JSON.parse(localStorage.getItem("flow-studio.calls") || "{}");
    return {
      days: PERIODS.includes(p.days) ? p.days : 7,
      channel: CHANNELS.some((c) => c.value === p.channel) ? p.channel : "phone",
    };
  } catch {
    return { days: 7, channel: "phone" };
  }
}

export function CallHistory({ flowId }: { flowId: string }) {
  const [prefs, setPrefs] = useState(readPrefs);
  const [flow, setFlow] = useState<FlowSummary | null>(null);
  const [summary, setSummary] = useState<CallSummary | null>(null);
  const [calls, setCalls] = useState<Call[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    api.flow(flowId).then(setFlow, (e) => setError((e as Error).message));
  }, [flowId]);

  useEffect(() => {
    try {
      localStorage.setItem("flow-studio.calls", JSON.stringify(prefs));
    } catch {
      /* private window: just don't remember */
    }
    let stale = false;
    setLoading(true);
    Promise.all([api.callSummary(flowId, prefs.days, prefs.channel), api.calls(flowId, prefs.days, prefs.channel)]).then(
      ([s, c]) => {
        if (stale) return;
        setSummary(s);
        setCalls(c);
        setError(null);
        setLoading(false);
      },
      (e) => {
        if (stale) return;
        setError((e as Error).message);
        setLoading(false);
      },
    );
    return () => {
      stale = true;
    };
  }, [flowId, prefs]);

  return (
    <main className="page wide">
      <Link to={`/flows/${flowId}`} className="link">
        ← Back to the flow
      </Link>
      <div className="calls-head">
        <div className="grow">
          <h1>Calls · {flow?.name || flowId}</h1>
          <p className="muted" style={{ margin: "4px 0 0" }}>
            How calls went, step by step. Only counts and timings are kept: no phone numbers, nothing said, no answers.
          </p>
        </div>
        <div className="calls-filters">
        <div className="tabs" role="group" aria-label="Period">
          {PERIODS.map((d) => (
            <button key={d} aria-pressed={prefs.days === d} onClick={() => setPrefs({ ...prefs, days: d })}>
              {d === 1 ? "24 h" : `${d} days`}
            </button>
          ))}
        </div>
        <div className="tabs" role="group" aria-label="Which calls">
          {CHANNELS.map((c) => (
            <button key={c.value} aria-pressed={prefs.channel === c.value} onClick={() => setPrefs({ ...prefs, channel: c.value })}>
              {c.label}
            </button>
          ))}
        </div>
        </div>
      </div>

      {error && <div className="notice bad" style={{ marginTop: 16 }}>{error}</div>}
      {!summary && !error && <p className="muted">Loading…</p>}
      {summary && (
        <div className={loading ? "calls-body loading" : "calls-body"}>
          {summary.calls === 0 ? (
            <div className="card" style={{ marginTop: 18 }}>
              <p style={{ margin: 0 }}>
                No {prefs.channel === "studio_test" ? "test calls" : prefs.channel === "phone" ? "phone calls" : "calls"} in this
                period.
              </p>
              {prefs.channel === "phone" && (
                <p className="muted" style={{ marginBottom: 0 }}>
                  Test calls from Flow Studio are counted separately; switch to “Test calls” to see them.
                </p>
              )}
            </div>
          ) : (
            <Report summary={summary} calls={calls || []} />
          )}
        </div>
      )}
    </main>
  );
}

function Report({ summary, calls }: { summary: CallSummary; calls: Call[] }) {
  const n = summary.calls;
  const o = summary.outcomes;
  return (
    <>
      {summary.truncated && (
        <div className="notice warn" style={{ marginTop: 16 }}>
          More than 20,000 calls in this period; the figures cover the newest 20,000. Pick a shorter period.
        </div>
      )}
      <div className="tiles">
        <Tile label="Calls" value={n.toLocaleString()} />
        <Tile label="Completed" value={pct(o.completed, n)} note={`${o.completed} reached an end step`} />
        <Tile label="Caller hung up" value={pct(o.caller_hung_up, n)} note={`${o.caller_hung_up} left before the end`} />
        <Tile label="Typical length" value={seconds(summary.duration_ms?.p50)} note={`slowest 5%: ${seconds(summary.duration_ms?.p95)}`} />
        <Tile
          label="Reply time"
          value={seconds(summary.latency_ms?.p50)}
          note={summary.latency_ms ? `slow replies: ${seconds(summary.latency_ms.p95)}` : "no replies measured"}
        />
      </div>

      <section className="card calls-section">
        <h2>Calls per day</h2>
        <DayBars days={summary.per_day} />
      </section>

      <section className="card calls-section">
        <h2>How calls ended</h2>
        <div className="outcomes">
          {OUTCOME_ORDER.filter((k) => o[k] > 0 || k === "completed" || k === "caller_hung_up").map((k) => (
            <div key={k} className="outcome" title={OUTCOME[k].help}>
              <span className={`chip ${OUTCOME[k].tone}`}>{OUTCOME[k].label}</span>
              <strong>{o[k]}</strong>
              <span className="muted">{pct(o[k], n)}</span>
            </div>
          ))}
        </div>
      </section>

      <section className="card calls-section">
        <h2>Steps</h2>
        <p className="muted small-text">
          How many calls reached each step, how long callers spent there, and where calls stopped. A step where many callers
          hang up is the first place to look.
        </p>
        <div className="table-wrap">
          <table className="data">
            <thead>
              <tr>
                <th>Step</th>
                <th className="num">Reached</th>
                <th className="num">Time there</th>
                <th className="num">Calls ended here</th>
                <th className="num">Caller hung up here</th>
              </tr>
            </thead>
            <tbody>
              {summary.steps.map((s) => (
                <tr key={s.node}>
                  <td>
                    <code>{s.node}</code>
                  </td>
                  <td className="num">
                    <span className="meter" aria-hidden="true">
                      <span style={{ width: `${(100 * s.reached) / n}%` }} />
                    </span>
                    {s.reached} <span className="muted">({pct(s.reached, n)})</span>
                  </td>
                  <td className="num">{seconds(s.avg_ms)}</td>
                  <td className="num">{s.ended_here}</td>
                  <td className="num">
                    {s.hung_up_here > 0 ? <span className="chip warn">{s.hung_up_here}</span> : <span className="muted">0</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <section className="card calls-section">
        <h2>Versions</h2>
        <p className="muted small-text">Compare a new version with the one before it.</p>
        <div className="table-wrap">
          <table className="data">
            <thead>
              <tr>
                <th>Version</th>
                <th className="num">Calls</th>
                <th className="num">Completed</th>
                <th className="num">Caller hung up</th>
                <th className="num">Average length</th>
                <th className="num">Last call</th>
              </tr>
            </thead>
            <tbody>
              {summary.versions.map((v) => (
                <tr key={`${v.version}-${v.source}`}>
                  <td>{versionLabel(v.version, v.source)}</td>
                  <td className="num">{v.calls}</td>
                  <td className="num">{pct(v.completed, v.calls)}</td>
                  <td className="num">{pct(v.caller_hung_up, v.calls)}</td>
                  <td className="num">{seconds(v.avg_duration_ms)}</td>
                  <td className="num">{timeAgo(v.last_call_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <section className="card calls-section">
        <h2>Usage</h2>
        <p className="muted small-text">Totals for these calls, for estimating cost.</p>
        <div className="usage">
          <div>
            <strong>{(summary.usage.llm_input_tokens + summary.usage.llm_output_tokens).toLocaleString()}</strong>
            <span className="muted">
              LLM tokens ({summary.usage.llm_input_tokens.toLocaleString()} in, {summary.usage.llm_output_tokens.toLocaleString()} out)
            </span>
          </div>
          <div>
            <strong>{Math.round(summary.usage.stt_audio_seconds / 60).toLocaleString()} min</strong>
            <span className="muted">speech-to-text</span>
          </div>
          <div>
            <strong>{summary.usage.tts_characters.toLocaleString()}</strong>
            <span className="muted">characters spoken ({Math.round(summary.usage.tts_audio_seconds / 60)} min)</span>
          </div>
          {summary.avg_turns && (
            <div>
              <strong>
                {summary.avg_turns.caller} / {summary.avg_turns.agent}
              </strong>
              <span className="muted">times caller / agent spoke, per call</span>
            </div>
          )}
        </div>
      </section>

      <section className="card calls-section">
        <h2>Recent calls</h2>
        <div className="table-wrap">
          <table className="data">
            <thead>
              <tr>
                <th>When</th>
                <th>Version</th>
                <th>Ended</th>
                <th className="num">Length</th>
                <th className="num">Reply time</th>
                <th>Steps</th>
              </tr>
            </thead>
            <tbody>
              {calls.map((c) => (
                <tr key={c.call_id}>
                  <td title={new Date(c.started_at).toLocaleString()}>
                    {when(c.started_at)}
                    {c.channel === "studio_test" && <span className="chip" style={{ marginLeft: 6 }}>test</span>}
                  </td>
                  <td>{versionLabel(c.flow_version, c.flow_source)}</td>
                  <td>
                    <span className={`chip ${OUTCOME[c.outcome].tone}`} title={OUTCOME[c.outcome].help}>
                      {OUTCOME[c.outcome].label}
                    </span>
                  </td>
                  <td className="num">{seconds(c.duration_ms)}</td>
                  <td className="num">{seconds(c.latency_ms?.p50)}</td>
                  <td className="path">
                    {c.steps.map((s, i) => (
                      <span key={i} title={seconds(s.ms)}>
                        {i > 0 && <span className="faint"> → </span>}
                        <code>{s.node}</code>
                      </span>
                    ))}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {calls.length === 50 && <p className="muted small-text">The newest 50 calls.</p>}
      </section>
    </>
  );
}

function Tile({ label, value, note }: { label: string; value: string; note?: string }) {
  return (
    <div className="tile">
      <div className="tile-label">{label}</div>
      <div className="tile-value">{value}</div>
      {note && <div className="tile-note">{note}</div>}
    </div>
  );
}

function DayBars({ days }: { days: CallSummary["per_day"] }) {
  const [hover, setHover] = useState<number | null>(null);
  const max = Math.max(1, ...days.map((d) => d.calls));
  const label = (iso: string) => new Date(`${iso}T12:00:00`).toLocaleDateString(undefined, { day: "numeric", month: "short" });
  const every = Math.ceil(days.length / 10); // at most ~10 date labels
  const h = hover != null ? days[hover] : null;
  return (
    <div className="daybars" onMouseLeave={() => setHover(null)}>
      <div className="daybars-tip" aria-live="polite">
        {h ? (
          <>
            <strong>{label(h.date)}</strong> · {h.calls} call{h.calls === 1 ? "" : "s"} · {h.completed} completed
          </>
        ) : (
          <span className="muted">Most in a day: {Math.max(0, ...days.map((d) => d.calls))} · point at a day for details</span>
        )}
      </div>
      <div className="daybars-plot" role="img" aria-label={days.map((d) => `${d.date}: ${d.calls}`).join(", ")}>
        {days.map((d, i) => (
          <div
            key={d.date}
            className={hover === i ? "daybar hover" : "daybar"}
            onMouseEnter={() => setHover(i)}
            onFocus={() => setHover(i)}
            tabIndex={0}
          >
            <span className="bar" style={{ height: `${(100 * d.calls) / max}%` }} />
          </div>
        ))}
      </div>
      <div className="daybars-axis">
        {days.map((d, i) => (
          <span key={d.date}>{i % every === 0 || i === days.length - 1 ? label(d.date) : ""}</span>
        ))}
      </div>
    </div>
  );
}

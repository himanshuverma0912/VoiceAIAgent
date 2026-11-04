// Reply checks: what the agent says is checked sentence by sentence before the
// caller hears it. The flow's settings turn checks on for every step; a
// conversation step can add more. The voice agent runs them (reply_checks.py).
import { REPLY_CHECKS, type ReplyCheck } from "../spec";

export function ReplyChecks(props: {
  selected: ReplyCheck[];
  /** On a step: the flow's checks, shown as already on. */
  everyStep?: ReplyCheck[];
  editable: boolean;
  change: (checks: ReplyCheck[]) => void;
}) {
  const everyStep = props.everyStep ?? [];
  const toggle = (id: ReplyCheck, on: boolean) =>
    props.change(on ? REPLY_CHECKS.map((c) => c.id).filter((c) => c === id || props.selected.includes(c)) : props.selected.filter((c) => c !== id));
  return (
    <div className="reply-checks">
      {REPLY_CHECKS.map((c) => {
        const inherited = everyStep.includes(c.id);
        const on = props.selected.includes(c.id) || inherited;
        return (
          <label key={c.id} className={`reply-check${on ? " on" : ""}`}>
            <input
              type="checkbox"
              checked={on}
              // Listed here as well as on every step (a problem): untick to remove it here.
              disabled={!props.editable || (inherited && !props.selected.includes(c.id))}
              onChange={(e) => toggle(c.id, e.target.checked)}
            />
            <span>
              <strong>{c.label}</strong>
              {inherited && <span className="chip">on every step</span>}
              <span className="hint">{c.about}</span>
            </span>
          </label>
        );
      })}
    </div>
  );
}

import { useEffect, useRef, useState, type ChangeEvent, type KeyboardEvent as ReactKeyboardEvent, type ReactNode } from "react";

export function Dialog(props: {
  title: ReactNode;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  narrow?: boolean;
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && props.onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [props.onClose]);
  return (
    <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && props.onClose()}>
      <div className={`dialog${props.narrow ? " narrow" : ""}`} role="dialog" aria-modal="true">
        <div className="dialog-head">
          <h2 className="grow">{props.title}</h2>
          <button className="icon" aria-label="Close" onClick={props.onClose}>
            ✕
          </button>
        </div>
        <div className="dialog-body">{props.children}</div>
        {props.footer && <div className="dialog-foot">{props.footer}</div>}
      </div>
    </div>
  );
}

let showToast: (msg: string) => void = () => {};
export const toast = (msg: string) => showToast(msg);

export function Toasts() {
  const [msg, setMsg] = useState<string | null>(null);
  useEffect(() => {
    let timer: number | undefined;
    showToast = (m) => {
      setMsg(m);
      window.clearTimeout(timer);
      timer = window.setTimeout(() => setMsg(null), 3200);
    };
    return () => window.clearTimeout(timer);
  }, []);
  return msg ? (
    <div className="toast" role="status">
      {msg}
    </div>
  ) : null;
}

export function Field(props: { label: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <label className="field">
      <span>{props.label}</span>
      {props.children}
      {props.hint && <div className="hint">{props.hint}</div>}
    </label>
  );
}

export function Check(props: { label: string; checked: boolean; onChange: (v: boolean) => void; disabled?: boolean }) {
  return (
    <label className="check">
      <input
        type="checkbox"
        checked={props.checked}
        disabled={props.disabled}
        onChange={(e) => props.onChange(e.target.checked)}
      />
      {props.label}
    </label>
  );
}

/** "a, b, c" <-> ["a", "b", "c"]; empty text means no list. */
export function ListInput(props: {
  value: string[] | null | undefined;
  onChange: (v: string[] | undefined) => void;
  disabled?: boolean;
  placeholder?: string;
}) {
  const [text, setText] = useState((props.value ?? []).join(", "));
  useEffect(() => {
    setText((props.value ?? []).join(", "));
  }, [JSON.stringify(props.value)]);
  return (
    <input
      value={text}
      disabled={props.disabled}
      placeholder={props.placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const items = e.target.value.split(",").map((s) => s.trim()).filter(Boolean);
        props.onChange(items.length ? items : undefined);
      }}
    />
  );
}

/**
 * A text box that suggests variables after "{{".
 * Up/Down choose, Enter or Tab inserts, Escape closes.
 */
export function VarText(props: {
  value: string;
  onChange: (v: string) => void;
  vars: { name: string; about: string }[];
  disabled?: boolean;
  rows?: number;
  placeholder?: string;
  single?: boolean;
  ariaLabel?: string;
}) {
  const ref = useRef<HTMLTextAreaElement & HTMLInputElement>(null);
  const [query, setQuery] = useState<string | null>(null);
  const [active, setActive] = useState(0);
  const matches = query === null ? [] : props.vars.filter((v) => v.name.startsWith(query)).slice(0, 12);

  const update = (el: HTMLTextAreaElement | HTMLInputElement) => {
    const before = el.value.slice(0, el.selectionStart ?? el.value.length);
    const m = /\{\{\s*([A-Za-z0-9_]*)$/.exec(before);
    setQuery(m ? m[1] : null);
    setActive(0);
  };

  const insert = (name: string) => {
    const el = ref.current;
    if (!el) return;
    const caret = el.selectionStart ?? el.value.length;
    const before = el.value.slice(0, caret).replace(/\{\{\s*[A-Za-z0-9_]*$/, `{{${name}}}`);
    const after = el.value.slice(caret).replace(/^[A-Za-z0-9_]*\s*\}\}/, "");
    props.onChange(before + after);
    setQuery(null);
    requestAnimationFrame(() => {
      el.focus();
      el.setSelectionRange(before.length, before.length);
    });
  };

  const common = {
    ref,
    value: props.value,
    disabled: props.disabled,
    placeholder: props.placeholder,
    "aria-label": props.ariaLabel,
    onChange: (e: ChangeEvent<HTMLTextAreaElement | HTMLInputElement>) => {
      props.onChange(e.target.value);
      update(e.target);
    },
    onKeyDown: (e: ReactKeyboardEvent) => {
      if (!matches.length) return;
      if (e.key === "ArrowDown") setActive((a) => (a + 1) % matches.length);
      else if (e.key === "ArrowUp") setActive((a) => (a - 1 + matches.length) % matches.length);
      else if (e.key === "Enter" || e.key === "Tab") insert(matches[active].name);
      else if (e.key === "Escape") setQuery(null);
      else return;
      e.preventDefault();
    },
    onBlur: () => setTimeout(() => setQuery(null), 150),
  };
  return (
    <div className="var-wrap">
      {props.single ? <input {...common} /> : <textarea rows={props.rows ?? 3} {...common} />}
      {matches.length > 0 && (
        <div className="var-menu" role="listbox">
          {matches.map((v, i) => (
            <button
              key={v.name}
              type="button"
              role="option"
              aria-selected={i === active}
              onMouseDown={(e) => {
                e.preventDefault();
                insert(v.name);
              }}
            >
              <code>{`{{${v.name}}}`}</code>
              <span className="about">{v.about}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

export function timeAgo(iso: string | null | undefined): string {
  if (!iso) return "";
  const s = Math.round((Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return new Date(iso).toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });
}

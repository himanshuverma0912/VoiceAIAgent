// A tiny router: "/" is the flows list, "/flows/<id>" a flow's editor.
import { useEffect, useState, type ReactNode } from "react";

type Guard = () => boolean; // false: stay on the page
let leaveGuard: Guard | null = null;

/** While set, navigating away asks the guard first (used for unsaved changes). */
export function setLeaveGuard(guard: Guard | null): void {
  leaveGuard = guard;
}

export function navigate(path: string): void {
  if (path === location.pathname) return;
  if (leaveGuard && !leaveGuard()) return;
  history.pushState(null, "", path);
  window.dispatchEvent(new PopStateEvent("popstate"));
}

export function usePath(): string {
  const [path, setPath] = useState(location.pathname);
  useEffect(() => {
    const onPop = () => setPath(location.pathname);
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);
  return path;
}

export function Link(props: { to: string; className?: string; children: ReactNode }) {
  return (
    <a
      href={props.to}
      className={props.className}
      onClick={(e) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
        e.preventDefault();
        navigate(props.to);
      }}
    >
      {props.children}
    </a>
  );
}

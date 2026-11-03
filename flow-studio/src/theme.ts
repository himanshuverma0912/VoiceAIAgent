// Light or dark. public/theme-init.js sets it before the page draws; this keeps
// it in step afterwards: the toggle saves a choice (in this browser only), and
// without one the page follows the computer's setting as it changes.
import { useEffect, useState } from "react";

export type Theme = "light" | "dark";
const KEY = "flow-studio.theme";

function saved(): Theme | null {
  try {
    const v = localStorage.getItem(KEY);
    return v === "light" || v === "dark" ? v : null;
  } catch {
    return null;
  }
}

const system = (): Theme => (window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "dark" : "light");

function apply(theme: Theme): void {
  document.documentElement.dataset.theme = theme;
}

export function useTheme(): [Theme, () => void] {
  const [theme, setTheme] = useState<Theme>(() => saved() ?? system());

  useEffect(() => apply(theme), [theme]);

  // No saved choice: follow the computer when it switches (e.g. at sunset).
  useEffect(() => {
    const mq = window.matchMedia?.("(prefers-color-scheme: dark)");
    if (!mq) return;
    const onChange = () => {
      if (!saved()) setTheme(system());
    };
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  const toggle = () =>
    setTheme((t) => {
      const next: Theme = t === "dark" ? "light" : "dark";
      try {
        localStorage.setItem(KEY, next);
      } catch {
        // Storage blocked (private window): the choice lasts until the page is closed.
      }
      return next;
    });

  return [theme, toggle];
}

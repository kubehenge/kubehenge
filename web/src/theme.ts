import { useSyncExternalStore } from "react";

/** Theme is the colour scheme someone picked, or "system" to follow the operating system. */
export type Theme = "system" | "light" | "dark";

export const themes: { theme: Theme; label: string }[] = [
  { theme: "system", label: "System" },
  { theme: "light", label: "Light" },
  { theme: "dark", label: "Dark" },
];

const key = "kubehenge-theme";
const listeners = new Set<() => void>();

export function readTheme(): Theme {
  const stored = localStorage.getItem(key);
  return themes.find((t) => t.theme === stored)?.theme ?? "system";
}

/** applyTheme sets the page's colour scheme; main applies the stored one before the first paint. */
export function applyTheme(theme: Theme): void {
  if (theme === "system") {
    delete document.documentElement.dataset["theme"];
  } else {
    document.documentElement.dataset["theme"] = theme;
  }
}

export function setTheme(theme: Theme): void {
  if (theme === "system") {
    localStorage.removeItem(key);
  } else {
    localStorage.setItem(key, theme);
  }
  applyTheme(theme);
  for (const l of listeners) {
    l();
  }
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function useTheme(): Theme {
  return useSyncExternalStore(subscribe, readTheme);
}

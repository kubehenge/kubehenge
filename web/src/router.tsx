import { useId, useSyncExternalStore, type JSX, type MouseEvent, type ReactNode } from "react";

// A few routes need no router library: the path lives in the history API and every change is
// announced with one event.

const listeners = new Set<() => void>();

function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("popstate", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("popstate", listener);
  };
}

export function navigate(path: string, replace = false): void {
  if (replace) {
    window.history.replaceState(null, "", path);
  } else {
    window.history.pushState(null, "", path);
  }
  for (const l of listeners) {
    l();
  }
}

export function usePath(): string {
  return useSyncExternalStore(subscribe, () => window.location.pathname);
}

/** useQuery is a parameter of the address's query, or null without it. */
export function useQuery(name: string): string | null {
  const search = useSyncExternalStore(subscribe, () => window.location.search);
  return new URLSearchParams(search).get(name);
}

// match returns the decoded path segments a pattern's :names stand for, or null, also for a
// segment that is no valid percent-encoding.
export function match(pattern: string, path: string): Record<string, string> | null {
  const want = pattern.split("/").filter(Boolean);
  const got = path.split("/").filter(Boolean);
  if (want.length !== got.length) {
    return null;
  }
  const params: Record<string, string> = {};
  for (const [i, w] of want.entries()) {
    const g = got[i] ?? "";
    if (w.startsWith(":")) {
      try {
        params[w.slice(1)] = decodeURIComponent(g);
      } catch {
        return null;
      }
    } else if (w !== g) {
      return null;
    }
  }
  return params;
}

export function Link({
  to,
  children,
  className,
  current,
  label,
  description,
}: {
  to: string;
  children: ReactNode;
  className?: string;
  current?: boolean;
  /** The link's name, where its content alone does not say it. */
  label?: string;
  /** Said after the name, such as a tab's badge, without becoming part of it. */
  description?: string | undefined;
}): JSX.Element {
  const describedBy = useId();
  const onClick = (e: MouseEvent<HTMLAnchorElement>) => {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) {
      return;
    }
    e.preventDefault();
    navigate(to);
  };
  return (
    <a
      href={to}
      onClick={onClick}
      className={className}
      aria-current={current === true ? "page" : undefined}
      aria-label={label}
      aria-describedby={description === undefined ? undefined : describedBy}
      title={label}
    >
      {children}
      {description !== undefined && (
        <span id={describedBy} hidden>
          {description}
        </span>
      )}
    </a>
  );
}

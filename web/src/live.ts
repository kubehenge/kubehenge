import { useEffect, useEffectEvent } from "react";

// The server tells the page when the clusters or their machines change, so that what shows them is
// read again at once rather than at its next poll. The polls go on meanwhile, for when the stream
// is down. One stream serves every view, open while any view listens.

type Listener = () => void;

const listeners = new Set<Listener>();
let source: EventSource | undefined;

function told(): void {
  // A hidden tab reads again when it is shown.
  if (document.hidden) {
    return;
  }
  for (const l of listeners) {
    l();
  }
}

function listen(l: Listener): () => void {
  listeners.add(l);
  // A stream the server refused, after a session ended, stays closed until a view listens again.
  if (typeof EventSource !== "undefined" && (source === undefined || source.readyState === EventSource.CLOSED)) {
    source = new EventSource("/api/events");
    source.addEventListener("clusters", told);
  }
  return () => {
    listeners.delete(l);
    if (listeners.size === 0) {
      source?.close();
      source = undefined;
    }
  };
}

/**
 * useClusterChanges calls changed each time the server says the clusters or their machines changed,
 * while on: not before the management cluster is set up, which the stream needs.
 */
export function useClusterChanges(changed: () => void, on = true): void {
  const onChange = useEffectEvent(changed);
  useEffect(() => (on ? listen(() => onChange()) : undefined), [on]);
}

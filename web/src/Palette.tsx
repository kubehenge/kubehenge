import { useEffectEvent, useId, useLayoutEffect, useRef, useState, type JSX, type KeyboardEvent } from "react";

import { Dialog } from "./ui";

/** Something the palette can do: go to a page, or start an action. */
export interface Command {
  id: string;
  label: string;
  /** What kind of thing it is, shown beside it: a cluster, a view, a page. */
  kind: string;
  /** The key that does it outside the palette, when there is one. */
  keys?: string;
  /** Further words it is found by. */
  words?: string;
  run: () => void;
}

// The palette shows this many matches. The rest are a few more letters away.
const shownMatches = 12;

// What a matched letter counts for: one alone, more for one right after the last, and more again
// at the start of a word.
const letterScore = 1;
const runScore = 3;
const wordStartScore = 2;

// score says how well a query matches a text, letters in order, or -1 when it does not.
export function score(query: string, text: string): number {
  const q = query.toLowerCase().replaceAll(/\s/gu, "");
  if (q === "") {
    return 0;
  }
  const t = text.toLowerCase();
  let at = 0;
  let total = 0;
  let last = -2;
  for (const ch of q) {
    const found = t.indexOf(ch, at);
    if (found === -1) {
      return -1;
    }
    total += found === last + 1 ? runScore : letterScore;
    if (found === 0 || /[\s/:.-]/u.test(t[found - 1] ?? "")) {
      total += wordStartScore;
    }
    last = found;
    at = found + 1;
  }
  return total;
}

function matches(commands: Command[], query: string): Command[] {
  if (query.trim() === "") {
    return commands.slice(0, shownMatches);
  }
  return commands
    .map((c) => ({ c, s: score(query, `${c.label} ${c.words ?? ""}`) }))
    .filter((m) => m.s >= 0)
    .toSorted((a, b) => b.s - a.s)
    .slice(0, shownMatches)
    .map((m) => m.c);
}

/** Palette finds any page, cluster or action by a few letters of its name. */
export function Palette({ commands, onClose }: { commands: Command[]; onClose: () => void }): JSX.Element {
  const ref = useRef<HTMLDialogElement>(null);
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const listId = useId();
  const shown = matches(commands, query);
  const chosen = shown[Math.min(active, shown.length - 1)];

  useLayoutEffect(() => {
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const d = ref.current;
    if (d && !d.open) {
      d.showModal();
      d.querySelector("input")?.focus();
    }
    // A command that leads to another page moves the focus there itself, after this.
    return () => opener?.focus();
  }, []);

  const run = (c: Command | undefined): void => {
    if (c === undefined) {
      return;
    }
    onClose();
    c.run();
  };

  const onKey = (e: KeyboardEvent<HTMLInputElement>): void => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive((a) => (shown.length === 0 ? 0 : (Math.min(a, shown.length - 1) + 1) % shown.length));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((a) => (shown.length === 0 ? 0 : (Math.min(a, shown.length - 1) - 1 + shown.length) % shown.length));
    } else if (e.key === "Enter") {
      e.preventDefault();
      run(chosen);
    }
  };

  return (
    <dialog
      ref={ref}
      className="palette"
      aria-label="Go to or run"
      closedby="any"
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
    >
      <input
        type="text"
        role="combobox"
        aria-label="Go to or run"
        aria-expanded="true"
        aria-controls={listId}
        aria-activedescendant={chosen ? `${listId}-${chosen.id}` : undefined}
        placeholder="Go to or run…"
        autoComplete="off"
        spellCheck={false}
        value={query}
        onChange={(e) => {
          setQuery(e.target.value);
          setActive(0);
        }}
        onKeyDown={onKey}
      />
      {/* oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- a list that filters as one types, which no select does */}
      <div id={listId} role="listbox" aria-label="Matches" className="palette-list">
        {shown.map((c) => (
          <div
            key={c.id}
            id={`${listId}-${c.id}`}
            // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- an option of that list
            role="option"
            tabIndex={-1}
            aria-selected={c === chosen}
            className="palette-option"
            onMouseMove={() => setActive(shown.indexOf(c))}
            onClick={() => run(c)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                run(c);
              }
            }}
          >
            <span className="palette-label">{c.label}</span>
            <span className="palette-kind">{c.kind}</span>
            {c.keys !== undefined && (
              <span className="palette-keys" aria-hidden="true">
                <Keys keys={c.keys} />
              </span>
            )}
          </div>
        ))}
      </div>
      {shown.length === 0 && <p className="palette-none">No matches</p>}
    </dialog>
  );
}

// Keys shows a key, or a sequence of keys pressed one after the other, as keycaps: a letter as its
// key is labelled.
function Keys({ keys }: { keys: string }): JSX.Element {
  return (
    <>
      {keys.split(" ").map((k, i) => (
        <kbd key={`${String(i)}-${k}`}>{k.toUpperCase()}</kbd>
      ))}
    </>
  );
}

/** A key, or a sequence of two ("g c"), that does something anywhere on the page, outside fields and dialogs. */
export interface Shortcut {
  keys: string;
  what: string;
  /** Whether the key is for this page. A key for another page does nothing here. */
  on?: boolean;
  run: () => void;
}

// typing tells whether a key goes into a field, where it is the user's text, not a command.
function typing(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) {
    return false;
  }
  return target.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(target.tagName);
}

// The second key of a sequence such as "g c" counts only this soon after the first.
const sequenceWait = 1500;

/**
 * useShortcuts runs the page's keys: Ctrl K (or ⌘ K) opens the palette from anywhere, the others
 * only where nothing else takes them, outside fields and with no dialog open. Keys that leave the
 * page or change its layout are two-key sequences ("g c"), so a stray key does neither.
 */
export function useShortcuts(openPalette: () => void, shortcuts: Shortcut[]): void {
  const pending = useRef<{ key: string; at: number }>(undefined);
  const onKey = useEffectEvent((e: globalThis.KeyboardEvent) => {
    if (e.key.toLowerCase() === "k" && (e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey) {
      e.preventDefault();
      if (document.querySelector("dialog[open]") === null) {
        openPalette();
      }
      return;
    }
    if (e.ctrlKey || e.metaKey || e.altKey || e.defaultPrevented || typing(e.target)) {
      return;
    }
    if (document.querySelector("dialog[open]") !== null) {
      return;
    }
    const live = shortcuts.filter((x) => x.on !== false);
    const first = pending.current;
    pending.current = undefined;
    const now = performance.now();
    const sequence = first !== undefined && now - first.at < sequenceWait ? `${first.key} ${e.key}` : undefined;
    const s = live.find((x) => x.keys === sequence) ?? live.find((x) => x.keys === e.key);
    if (s) {
      e.preventDefault();
      s.run();
      return;
    }
    if (live.some((x) => x.keys.startsWith(`${e.key} `))) {
      e.preventDefault();
      pending.current = { key: e.key, at: now };
    }
  });
  // Before the browser paints, so a key pressed as soon as the bar shows is not lost.
  useLayoutEffect(() => {
    const listener = (e: globalThis.KeyboardEvent): void => onKey(e);
    window.addEventListener("keydown", listener);
    return () => window.removeEventListener("keydown", listener);
  }, []);
}

/** ShortcutsDialog lists the keys the page answers to. */
export function ShortcutsDialog({ shortcuts, onClose }: { shortcuts: Shortcut[]; onClose: () => void }): JSX.Element {
  return (
    <Dialog
      title="Keyboard shortcuts"
      onClose={onClose}
      actions={
        <button type="button" className="primary" onClick={onClose}>
          Done
        </button>
      }
    >
      <table className="grid keys">
        <tbody>
          <tr>
            <th scope="row">
              <kbd>Ctrl</kbd> <kbd>K</kbd>
            </th>
            <td>Go to or run</td>
          </tr>
          {shortcuts.map((s) => (
            <tr key={s.keys}>
              <th scope="row">
                <Keys keys={s.keys} />
              </th>
              <td>{s.what}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </Dialog>
  );
}

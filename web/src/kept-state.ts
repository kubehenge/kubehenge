import { keepCache } from "./poll-cache";

// A refresh shows the fleet as this tab last read it, until the fresh reads answer. The answers
// are kept in the tab's sessionStorage, so they outlast a refresh and go with the tab.

/**
 * keptFor is how old a kept answer may be and still show: ten of the fleet's idle polls. An open
 * tab writes its answers at every read, so only a tab left hidden or closed holds older ones, and
 * those are more likely wrong than right.
 */
export const keptFor = 300_000;

const prefix = "kubehenge-kept-";

interface Stored<T> {
  user: string;
  at: number;
  entries: [string, T][];
}

// isStored tells whether what the tab kept is in the form this page writes.
const isStored = <T>(s: unknown): s is Stored<T> =>
  typeof s === "object" &&
  s !== null &&
  "at" in s &&
  typeof s.at === "number" &&
  "user" in s &&
  typeof s.user === "string" &&
  "entries" in s &&
  Array.isArray(s.entries);

// The user the kept answers were read for, once the session says.
let user = "";

function load<T>(name: string): Stored<T> | undefined {
  try {
    const text = sessionStorage.getItem(prefix + name);
    const s: unknown = text === null ? undefined : JSON.parse(text);
    return isStored<T>(s) && Date.now() - s.at < keptFor ? s : undefined;
  } catch {
    return undefined;
  }
}

// StoredCache is a cache of answers that writes itself to sessionStorage on every answer, and
// empties its copy there with the other caches.
class StoredCache<T> extends Map<string, T> {
  readonly name: string;
  owner = "";

  constructor(name: string) {
    super();
    this.name = name;
    this.restore();
  }

  /** restore takes up what the tab kept, as a page load does. */
  restore(): void {
    const s = load<T>(this.name);
    super.clear();
    this.owner = s?.user ?? "";
    for (const [k, v] of s?.entries ?? []) {
      super.set(k, v);
    }
  }

  override set(key: string, value: T): this {
    super.set(key, value);
    this.owner = user;
    const s: Stored<T> = { user, at: Date.now(), entries: [...this.entries()] };
    try {
      sessionStorage.setItem(prefix + this.name, JSON.stringify(s));
    } catch {
      // A full or refused storage only loses the head start after a refresh.
    }
    return this;
  }

  override clear(): void {
    super.clear();
    this.owner = "";
    try {
      sessionStorage.removeItem(prefix + this.name);
    } catch {
      // Nothing was kept.
    }
  }
}

const stored = new Set<StoredCache<unknown>>();

/** keptCache is a cache of one kind of answer for usePoll that a refresh of the tab keeps. */
export function keptCache<T>(name: string): Map<string, T> {
  const c = new StoredCache<T>(name);
  stored.add(c);
  return keepCache(c);
}

/** keptUser names who the session is for: what was kept for anyone else is dropped. */
export function keptUser(name: string): void {
  user = name;
  for (const c of stored) {
    if (c.owner !== "" && c.owner !== name) {
      c.clear();
    }
  }
}

/** restoreKept takes up every kept answer again, as a refresh does. */
export function restoreKept(): void {
  for (const c of stored) {
    c.restore();
  }
}

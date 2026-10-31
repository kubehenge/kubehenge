// Every cache of answers the pages keep, so that a session's end empties them all: what one user
// was shown must not show to the next who signs in on the same page.
const caches = new Set<Map<string, unknown>>();

/** keepCache registers a cache to be emptied with the others. */
export function keepCache<T>(cache: Map<string, T>): Map<string, T> {
  caches.add(cache);
  return cache;
}

/** clearPollCaches empties every cache of answers. */
export function clearPollCaches(): void {
  for (const c of caches) {
    c.clear();
  }
}

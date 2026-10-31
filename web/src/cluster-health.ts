import { api } from "./api";
import type { ClusterHealth } from "./types";
import { pollCache, pollIdle, usePoll, type Poll } from "./ui";

/** pollUnwell is how often what is not healthy is read, to follow it getting better. */
export const pollUnwell = 10_000;

const healthCache = pollCache<ClusterHealth>();

/**
 * useClusterHealth reads a cluster's health page, shared by every view that shows part of it. It
 * lives apart from the health page so that the views using it leave that page to its own chunk.
 */
export function useClusterHealth(c: { namespace: string; name: string }): Poll<ClusterHealth> {
  return usePoll(
    async () => api.health(c.namespace, c.name),
    (shown) => (shown?.verdict === "healthy" ? pollIdle : pollUnwell),
    { cache: healthCache, key: `${c.namespace}/${c.name}` },
  );
}

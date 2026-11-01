import { createContext, use } from "react";

import type { ChangeRequest, Plan } from "./types";

/** The changes a cluster's page has staged, as the server planned them, and how to apply or drop them. */
export interface Staging {
  /** generation is the cluster's the first change was staged against, which the rest are made against too. */
  staged: { req: ChangeRequest; plan: Plan; generation: number } | undefined;
  /** dropped says why staged changes were dropped, until the next is staged. */
  dropped: string | undefined;
  /** stage adds a change to those staged and plans them all. It throws what the server refuses. */
  stage: (req: ChangeRequest) => Promise<void>;
  discard: () => void;
  apply: () => Promise<void>;
}

export const StagingContext = createContext<Staging | null>(null);

/** useStaging is the page's staged changes, or null where a change is made at once. */
export function useStaging(): Staging | null {
  return use(StagingContext);
}

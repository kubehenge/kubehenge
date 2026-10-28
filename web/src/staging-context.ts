import { createContext, use } from "react";

import type { ChangeRequest, Plan } from "./types";

/** The changes a cluster's page has staged, as the server planned them, and how to apply or drop them. */
export interface Staging {
  staged: { req: ChangeRequest; plan: Plan } | undefined;
  /** stage adds a change to those staged and plans them all; it throws what the server refuses. */
  stage: (req: ChangeRequest) => Promise<void>;
  discard: () => void;
  apply: () => Promise<void>;
}

export const StagingContext = createContext<Staging | null>(null);

/** useStaging is the page's staged changes, or null where a change is made at once. */
export function useStaging(): Staging | null {
  return use(StagingContext);
}

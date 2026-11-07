import { renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { useCreateCheck } from "./CreateCheck";
import { fakeServer } from "./fake-server";
import type { CreateRequest } from "./types";

const req = (name: string): CreateRequest => ({
  name,
  class: "kubehenge-k3s",
  version: "v1.35.9+k3s1",
  controlPlaneReplicas: 1,
  workerReplicas: 1,
  variables: {},
});

describe("the API server's refusal of a new cluster", () => {
  it("is offered to the step that can fix it until the form changes", async () => {
    fakeServer({
      "POST /api/clusters?dryRun=All": () => ({
        status: 422,
        body: { error: "spec.topology.variables[nodeCustomisation]: /etc/x is a path Kubehenge keeps" },
      }),
    });
    const { result } = renderHook(() => useCreateCheck());
    result.current.run(req("a"));
    await waitFor(() => expect(result.current.read?.error).toContain("is a path Kubehenge keeps"));
    expect(result.current.refusalOf(req("a"))).toContain("is a path Kubehenge keeps");
    expect(result.current.refusalOf(req("b"))).toBeUndefined();
  });
});

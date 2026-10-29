import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fakeServer } from "./fake-server";
import { standing } from "./fleet";
import type { Cluster } from "./types";

const deleting: Cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5+rke2r1",
  created: "2026-10-20T08:00:00Z",
  deleting: true,
  deletedAt: "2026-10-21T10:00:00Z",
  phase: "Deleting",
  available: false,
  conditions: [
    {
      type: "DeletionStalled",
      status: "True",
      reason: "Waiting",
      message: "the deletion has waited 25m0s on Machine web-md-0-a. If it cannot finish, abandon it",
      since: "2026-10-21T10:25:00Z",
    },
  ],
  controlPlane: { desired: 1, ready: 0, available: 0, upToDate: 0 },
  workers: { desired: 1, ready: 0, available: 0, upToDate: 0 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: {},
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  deletion: {
    waiting: [{ kind: "Machine", name: "web-md-0-a", reason: "Drain not completed yet" }],
    mayLeave: ["VM 101 on pve1", "Pool kubehenge/clusters.web"],
  },
};

const signedIn = { "GET /api/session": { body: { user: "dev@pve", csrf: "c", canTrust: true } } };

describe("a cluster being deleted", () => {
  it("says what its deletion waits on, and abandons it once the name is typed", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": { body: deleting },
      "POST /api/clusters/clusters/web/abandon-deletion": { status: 202 },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    const notice = await screen.findByRole("region", { name: "Deletion" });
    expect(notice).toHaveTextContent("Deletion stalled");
    expect(within(notice).getByRole("list", { name: "Waiting on" })).toHaveTextContent(
      "Machine web-md-0-a Drain not completed yet",
    );

    await user.click(within(notice).getByRole("button", { name: "Abandon…" }));
    const dialog = screen.getByRole("dialog", { name: "Abandon web's deletion" });
    expect(within(dialog).getByRole("list", { name: "May be left behind" })).toHaveTextContent("VM 101 on pve1");
    const abandon = within(dialog).getByRole("button", { name: "Abandon" });
    expect(abandon).toBeDisabled();
    await user.type(within(dialog).getByLabelText("Confirm name"), "web");
    await user.click(abandon);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === "POST /api/clusters/clusters/web/abandon-deletion")?.body).toStrictEqual({
      confirm: "web",
    });
  });

  it("shows an abandonment under way and what it left behind", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": {
        body: {
          ...deleting,
          deletion: { abandoned: "2026-10-21T10:30:00Z", leftBehind: ["VM 101 on pve1"] },
        } satisfies Cluster,
      },
    });
    render(<App />);
    const notice = await screen.findByRole("region", { name: "Deletion" });
    expect(notice).toHaveTextContent("Abandoning deletion");
    expect(notice).toHaveTextContent("Left behind: VM 101 on pve1");
    expect(within(notice).queryByRole("button", { name: "Abandon…" })).not.toBeInTheDocument();
  });

  it("needs someone in the fleet once its deletion stalled, until it is abandoned", () => {
    expect(standing(deleting)).toMatchObject({ group: "needs", reason: "Deletion stalled" });
    expect(standing({ ...deleting, conditions: [] })).toMatchObject({ group: "running", reason: "Being deleted" });
    expect(standing({ ...deleting, deletion: { abandoned: "2026-10-21T10:30:00Z" } })).toMatchObject({
      group: "running",
    });
  });

  it("is not offered to abandon to those who may not change the cluster", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": { body: { ...deleting, allowed: { change: false, kubeconfig: false } } },
    });
    render(<App />);
    await expect(screen.findByRole("button", { name: "Abandon…" })).resolves.toBeDisabled();
  });
});

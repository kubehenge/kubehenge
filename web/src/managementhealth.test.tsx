import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { fakeServer } from "./fake-server";
import { ManagementHealthCard, healthVerdict } from "./ManagementHealth";
import type { ManagementHealth } from "./types";

function show(health: ManagementHealth): void {
  fakeServer({ "GET /api/setup/health": { status: 200, body: health } });
  render(<ManagementHealthCard />);
}

const ready = (name: string) => ({ name, ready: true, kubelet: "v1.34.1" });
const deployment = (namespace: string, name: string, readyCount = 1) => ({
  namespace,
  name,
  ready: readyCount,
  wanted: 1,
});

describe("the management cluster's health", () => {
  it("counts ready nodes and controllers and lists nothing more when all are", async () => {
    show({
      answers: true,
      nodes: [ready("mgmt-1"), ready("mgmt-2"), ready("mgmt-3")],
      controllers: [deployment("capi-system", "capi-controller-manager")],
    });
    await expect(screen.findByText("Healthy")).resolves.toBeInTheDocument();
    expect(screen.getByText("3 of 3 ready")).toBeInTheDocument();
    expect(screen.getByText("1 of 1 ready")).toBeInTheDocument();
    expect(screen.queryByRole("list")).not.toBeInTheDocument();
  });

  it("names the nodes and controllers that are short", async () => {
    show({
      answers: true,
      nodes: [ready("mgmt-1"), { name: "mgmt-2", ready: false, message: "Kubelet stopped posting node status" }],
      controllers: [
        deployment("capi-system", "capi-controller-manager"),
        deployment("capmox-system", "capmox-controller-manager", 0),
      ],
    });
    await expect(screen.findByText("Degraded")).resolves.toBeInTheDocument();
    const items = within(screen.getByRole("list")).getAllByRole("listitem");
    expect(items.map((i) => i.textContent)).toStrictEqual([
      "Not ready mgmt-2 Kubelet stopped posting node status",
      "0 of 1 capmox-system/capmox-controller-manager",
    ]);
  });

  it("says when the cluster does not answer, with the reason behind Details", async () => {
    show({ answers: false, error: "dial tcp 192.0.2.20:6443: connect: connection refused" });
    await expect(screen.findByText("Not answering")).resolves.toBeInTheDocument();
    expect(screen.getByText("Details")).toBeInTheDocument();
    expect(screen.getByText("dial tcp 192.0.2.20:6443: connect: connection refused")).toBeInTheDocument();
  });

  it("calls a cluster with no ready node down", () => {
    expect(healthVerdict({ answers: true, nodes: [{ name: "mgmt-1", ready: false }] })).toStrictEqual({
      state: "error",
      text: "Down",
    });
  });
});

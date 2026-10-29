import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fakeServer } from "./fake-server";
import { stallText } from "./Recovery";
import type { Cluster, ControlPlaneRecovery, Machine } from "./types";

const command = `kubectl --namespace clusters annotate cluster web kubehenge.dev/etcd-restore="{\\"machine\\":\\"web-cp-a\\",\\"at\\":\\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\\"}"`;

const down =
  "control-plane machine web-cp-b, which replaces web-cp-a, has not become ready in 25m0s: * NodeHealthy: Waiting for a Node. " +
  "The cluster runs on the etcd members of both until it is, and its API does not answer, so one of them does not run. " +
  `Start what stopped if it can be; otherwise reset etcd to web-cp-a's own and replace web-cp-b, with Recover on the cluster's page or: ${command}`;

const machine = (name: string, created: string, ready: boolean): Machine => ({
  name,
  role: "control-plane",
  phase: "Running",
  ready,
  created,
  ...(ready ? {} : { waiting: "Waiting for a Node" }),
});

const stalled: Cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5+rke2r1",
  created: "2026-10-20T08:00:00Z",
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [
    {
      type: "ControlPlaneRollStalled",
      status: "True",
      reason: "APIUnreachable",
      message: down,
      since: "2026-10-21T10:00:00Z",
    },
  ],
  controlPlane: { desired: 1, ready: 0, available: 0, upToDate: 0 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: {},
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [machine("web-cp-b", "2026-10-21T09:35:00Z", false), machine("web-cp-a", "2026-10-20T08:00:00Z", true)],
};

const signedIn = { "GET /api/session": { body: { user: "dev@pve", csrf: "c", canTrust: true } } };

describe("a stalled roll of a single control plane", () => {
  it("is told apart from the kubectl command that does what Recover does", () => {
    const parted = stallText(down);
    expect(parted.command).toBe(command);
    expect(parted.said).toMatch(/^Control-plane machine web-cp-b, which replaces web-cp-a.*replace web-cp-b\.$/u);
    expect(stallText("something else")).toStrictEqual({ said: "Something else." });
  });

  it("is shown on the cluster's page and recovered keeping the machine it replaces, once the name is typed", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    const automatic: ControlPlaneRecovery = { automatic: true };
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": { body: stalled },
      "GET /api/clusters/clusters/web/control-plane-recovery?machine=web-cp-a": { body: automatic },
      "GET /api/clusters/clusters/web/control-plane-recovery?machine=web-cp-b": { body: automatic },
      "POST /api/clusters/clusters/web/control-plane-recovery": { status: 202 },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    const notice = await screen.findByRole("region", { name: "Stalled control-plane roll" });
    expect(notice).toHaveTextContent("Control-plane roll stalled, API down");
    expect(notice).toHaveTextContent("web-cp-b not ready for");
    expect(within(notice).getByLabelText("Recover with kubectl")).toHaveTextContent("annotate cluster web");

    await user.click(within(notice).getByRole("button", { name: "Recover…" }));
    const dialog = screen.getByRole("dialog", { name: "Recover web's control plane" });
    expect(within(dialog).getByLabelText("Keep")).toHaveValue("web-cp-a");
    expect(dialog).toHaveTextContent("Replaced: web-cp-b");
    const recover = within(dialog).getByRole("button", { name: "Recover" });
    await user.type(await within(dialog).findByLabelText("Confirm name"), "web");
    await user.click(recover);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(
      calls.findLast((c) => c.key === "POST /api/clusters/clusters/web/control-plane-recovery")?.body,
    ).toStrictEqual({
      machine: "web-cp-a",
      confirm: "web",
    });
  });

  it("lists the steps to recover by hand where the controller does not reach the guest agent", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    const byHand: ControlPlaneRecovery = {
      automatic: false,
      plan: {
        distribution: "rke2",
        steps: [
          {
            title: "Reset etcd to this node alone",
            where: "web-cp-a",
            command: "rke2 server --cluster-reset --etcd-s3=false",
          },
        ],
        replaced: ["web-cp-b"],
      },
    };
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": { body: stalled },
      "GET /api/clusters/clusters/web/control-plane-recovery?machine=web-cp-a": { body: byHand },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Recover…" }));
    const dialog = screen.getByRole("dialog", { name: "Recover web's control plane" });
    await expect(within(dialog).findByLabelText("Reset etcd to this node alone")).resolves.toHaveTextContent(
      "--cluster-reset",
    );
    expect(within(dialog).queryByRole("button", { name: "Recover" })).not.toBeInTheDocument();
    expect(calls.map((c) => c.key)).not.toContain("POST /api/clusters/clusters/web/control-plane-recovery");
  });

  it("is not offered to recover to those who may not take the kubeconfig, and shows how a recovery goes", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": {
        body: {
          ...stalled,
          allowed: { change: true, kubeconfig: false },
          recovery: {
            requestedAt: "2026-10-21T10:30:00Z",
            phase: "Restoring",
            reset: true,
            machine: "web-cp-a",
            node: "web-cp-a",
            by: "root@pam",
            started: "2026-10-21T10:30:05Z",
          },
        } satisfies Cluster,
      },
    });
    render(<App />);
    await expect(screen.findByRole("button", { name: "Recover…" })).resolves.toBeDisabled();
    await expect(screen.findByText("Resetting etcd on web-cp-a")).resolves.toBeInTheDocument();
  });
});

describe("a plan that rolls the only control plane", () => {
  it("warns before it is applied", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    const settled: Cluster = {
      ...stalled,
      conditions: [],
      machines: [machine("web-cp-a", "2026-10-20T08:00:00Z", true)],
    };
    const warning = "Replaces the only control plane.";
    fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": { body: settled },
      "GET /api/versions/rke2": { body: { versions: ["v1.36.6+rke2r1"] } },
      "POST /api/clusters/clusters/web/plan": {
        body: {
          cluster: { ...settled, version: "v1.36.6+rke2r1" },
          changes: [{ setting: "version", effect: "all" }],
          warnings: [warning],
        },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Upgrade" }));
    const dialog = screen.getByRole("dialog", { name: "Upgrade web" });
    await user.selectOptions(
      await within(dialog).findByRole("combobox", { name: "Kubernetes version" }),
      "v1.36.6+rke2r1",
    );
    await user.click(within(dialog).getByRole("button", { name: "Upgrade" }));
    await user.click(await screen.findByRole("button", { name: "Review plan" }));
    expect(screen.getByRole("dialog", { name: "Review plan" })).toHaveTextContent(warning);
  });
});

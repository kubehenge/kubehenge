import { act, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { fakeServer, held, type Route } from "./fake-server";
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

  it("marks the health stale while reading it again fails or hangs", async () => {
    const healthy = { answers: true, nodes: [ready("mgmt-1")], controllers: [] };
    const routes: Record<string, Route> = { "GET /api/setup/health": { body: healthy } };
    fakeServer(routes);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    render(<ManagementHealthCard />);
    await expect(screen.findByText("Healthy")).resolves.toBeInTheDocument();
    expect(screen.queryByText("Stale")).not.toBeInTheDocument();

    routes["GET /api/setup/health"] = { status: 502, body: { error: "the management cluster does not answer" } };
    await act(() => vi.advanceTimersByTimeAsync(30_000));
    expect(screen.getByText("Stale")).toHaveAttribute("title", "the management cluster does not answer");
    expect(screen.getByText("1 of 1 ready")).toBeInTheDocument();

    routes["GET /api/setup/health"] = { body: healthy };
    await act(() => vi.advanceTimersByTimeAsync(30_000));
    expect(screen.queryByText("Stale")).not.toBeInTheDocument();

    routes["GET /api/setup/health"] = held().route;
    await act(() => vi.advanceTimersByTimeAsync(90_000));
    expect(screen.getByText("Stale")).toHaveAttribute("title", "No answer from Kubehenge. Check the connection.");

    routes["GET /api/setup/health"] = { body: healthy };
    await act(() => vi.advanceTimersByTimeAsync(60_000));
    expect(screen.queryByText("Stale")).not.toBeInTheDocument();
    vi.useRealTimers();
  });

  it("shows when the controller cannot reach the Proxmox VE API, with the reason behind Details", async () => {
    show({
      answers: true,
      nodes: [ready("mgmt-1")],
      controllers: [deployment("capi-system", "capi-controller-manager")],
      proxmox: { ok: false, error: "lookup pve.internal: no such host", checked: "2026-01-02T12:00:00Z" },
    });
    await expect(screen.findByText("Degraded")).resolves.toBeInTheDocument();
    expect(screen.getByText("Not reachable")).toBeInTheDocument();
    expect(screen.getByText("lookup pve.internal: no such host")).toBeInTheDocument();
  });

  it("shows a reachable Proxmox VE API as healthy", async () => {
    show({
      answers: true,
      nodes: [ready("mgmt-1")],
      controllers: [],
      proxmox: { ok: true, checked: "2026-01-02T12:00:00Z" },
    });
    await expect(screen.findByText("Healthy")).resolves.toBeInTheDocument();
    expect(screen.getByText("Reachable")).toBeInTheDocument();
  });

  it("names each token that is refused or lacks a privilege", async () => {
    show({
      answers: true,
      nodes: [ready("mgmt-1")],
      controllers: [],
      proxmox: {
        ok: true,
        checked: "2026-01-02T12:00:00Z",
        tokens: [
          {
            name: "Controller",
            problem: "Privileges",
            message: "Controller token lacks Realm.AllocateUser on /access/realm/pve.",
          },
          {
            name: "Cluster team-a/web",
            problem: "Certificate",
            message: "Cluster team-a/web token cannot verify the API.",
          },
        ],
      },
    });
    await expect(screen.findByText("Degraded")).resolves.toBeInTheDocument();
    const list = screen.getByRole("list", { name: "Proxmox VE tokens" });
    expect(list).toHaveTextContent("Controller token lacks Realm.AllocateUser on /access/realm/pve.");
    expect(list).toHaveTextContent("Certificate");
  });

  it("warns when the token cannot see host maintenance", async () => {
    show({
      answers: true,
      nodes: [ready("mgmt-1")],
      controllers: [],
      proxmox: { ok: true, blind: "Sys.Audit on /", checked: "2026-01-02T12:00:00Z" },
    });
    await expect(screen.findByText("Degraded")).resolves.toBeInTheDocument();
    expect(screen.getByText("Host maintenance is not visible: the token lacks Sys.Audit on /")).toBeInTheDocument();
  });

  it("counts the etcd members and calls the cluster degraded when one is not healthy", async () => {
    show({ answers: true, nodes: [ready("mgmt-1")], controllers: [], etcd: { healthy: 2, members: 3 } });
    await expect(screen.findByText("Degraded")).resolves.toBeInTheDocument();
    expect(screen.getByText("2 of 3 members")).toBeInTheDocument();
    expect(healthVerdict({ answers: true, etcd: { healthy: 3, members: 3 } }).text).toBe("Healthy");
  });

  it("answers for each Proxmox VE node whether it may reboot, and why not", async () => {
    fakeServer({
      "GET /api/setup/health": {
        body: { answers: true, nodes: [ready("mgmt-1")], controllers: [], etcd: { healthy: 3, members: 3 } },
      },
      "GET /api/appliance/reboot-ready": {
        body: {
          checked: "2026-01-02T12:00:00Z",
          ready: true,
          nodes: [
            { node: "pve1", ready: true },
            { node: "pve2", ready: false, blocker: "team-a/web: stopping pve2 would leave 1 of 3 etcd members" },
          ],
          answer: { ready: true },
        },
      },
    });
    render(<ManagementHealthCard />);
    const list = await screen.findByRole("list", { name: "Ready for reboot" });
    expect(
      within(list)
        .getAllByRole("listitem")
        .map((i) => i.textContent),
    ).toStrictEqual(["Ready pve1", "Wait pve2 team-a/web: stopping pve2 would leave 1 of 3 etcd members"]);
  });

  it("says once why no node may reboot while a member is down", async () => {
    const blocker = "team-a/web: etcd member web-cp-1 on pve2 is not healthy: its VM is stopped";
    fakeServer({
      "GET /api/setup/health": { body: { answers: true, nodes: [ready("mgmt-1")], controllers: [] } },
      "GET /api/appliance/reboot-ready": {
        body: {
          checked: "2026-01-02T12:00:00Z",
          ready: false,
          blocker,
          nodes: [
            { node: "pve1", ready: false, blocker },
            { node: "pve2", ready: false, blocker },
          ],
          answer: { ready: false },
        },
      },
    });
    render(<ManagementHealthCard />);
    await expect(screen.findByText(blocker)).resolves.toBeInTheDocument();
    expect(screen.getAllByText(blocker)).toHaveLength(1);
    expect(screen.getAllByText("Wait")).toHaveLength(3);
  });

  it("lets a node go that holds no member of the degraded cluster, with the cluster as a note", async () => {
    const note = "team-a/web: etcd member web-cp-1 on pve2 is not healthy: its VM is stopped";
    fakeServer({
      "GET /api/setup/health": { body: { answers: true, nodes: [ready("mgmt-1")], controllers: [] } },
      "GET /api/appliance/reboot-ready": {
        body: {
          checked: "2026-01-02T12:00:00Z",
          ready: false,
          blocker: note,
          nodes: [
            {
              node: "pve1",
              ready: false,
              blocker: "team-a/web: stopping pve1 would leave 1 of 3 etcd members healthy",
            },
            { node: "pve4", ready: true, note },
          ],
          answer: { ready: false },
        },
      },
    });
    render(<ManagementHealthCard />);
    const text = await screen.findByText(note);
    expect(text).toHaveClass("muted");
    const list = await screen.findByRole("list", { name: "Ready for reboot" });
    expect(within(list).getByText("Ready")).toBeInTheDocument();
  });

  it("calls a cluster with no ready node down", () => {
    expect(healthVerdict({ answers: true, nodes: [{ name: "mgmt-1", ready: false }] })).toStrictEqual({
      state: "error",
      text: "Down",
    });
  });
});

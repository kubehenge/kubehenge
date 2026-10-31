import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { fakeServer, held, reads, type Route } from "./fake-server";
import { HealthView } from "./Health";
import type { Cluster, ClusterHealth, UnwellPod } from "./types";

const minutesAgo = (n: number): string => new Date(Date.now() - n * 60_000).toISOString();

const cluster: Cluster = {
  namespace: "default",
  name: "web",
  class: "kubehenge-kubeadm",
  distribution: "kubeadm",
  version: "v1.36.1",
  generation: 1,
  created: minutesAgo(90),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [{ type: "Available", status: "True", since: minutesAgo(80) }],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: {},
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: true },
};

const crashing: UnwellPod = {
  namespace: "shop",
  name: "api-7d9f",
  node: "web-md-0-a",
  severity: "error",
  reason: "CrashLoopBackOff",
  message: "Exited with code 1, 4 minutes ago.",
  restarts: 7,
  since: minutesAgo(12),
  containers: ["api", "sidecar"],
};

const degraded: ClusterHealth = {
  read: new Date().toISOString(),
  verdict: "degraded",
  attention: [
    {
      severity: "error",
      section: "pods",
      what: "Pod shop/api-7d9f: CrashLoopBackOff",
      message: "Exited with code 1, 4 minutes ago.",
      since: minutesAgo(12),
      pod: { namespace: "shop", name: "api-7d9f" },
    },
    { severity: "warning", section: "addons", what: "Add-on ingress: pending-install", since: minutesAgo(3) },
    { severity: "warning", section: "backups", what: "Backups overdue", message: "No backup has completed yet." },
  ],
  nodes: {
    items: [
      {
        name: "web-cp-a",
        machine: "web-cp-a",
        role: "control-plane",
        joined: true,
        ready: true,
        kubelet: "v1.36.1",
        cpu: { requested: 950, allocatable: 2000 },
        memory: { requested: 1_073_741_824, allocatable: 4_294_967_296 },
        pods: 14,
        vm: {
          id: 101,
          node: "pve1",
          status: "running",
          cpu: 0.25,
          cores: 2,
          memory: 2_147_483_648,
          maxMemory: 4_294_967_296,
          uptime: 5400,
          load: [0.1, 0.2, null, 0.4],
        },
      },
      {
        name: "web-md-0-a",
        role: "worker",
        pool: "md-0",
        joined: true,
        ready: true,
        pressure: ["MemoryPressure"],
        pods: 9,
      },
    ],
  },
  pods: { items: [crashing] },
  volumes: { items: [{ namespace: "shop", name: "data", storageClass: "local", since: minutesAgo(6) }] },
  addons: {
    items: [
      {
        name: "web-cilium",
        release: "cilium",
        namespace: "kube-system",
        chart: "cilium",
        status: "deployed",
        ready: true,
      },
      {
        name: "web-ingress",
        release: "ingress",
        namespace: "ingress",
        chart: "ingress-nginx",
        status: "pending-install",
        ready: false,
      },
    ],
  },
  pools: { items: [{ name: "md-0", desired: 1, ready: 1, available: 1, upToDate: 1, rollingOut: false }] },
  controlPlane: {
    kind: "KubeadmControlPlane",
    conditions: [{ type: "EtcdClusterHealthy", status: "True", since: minutesAgo(80) }],
  },
  healthChecks: { items: [{ name: "web-md-0", expected: 1, healthy: 1, allowed: true }] },
  events: {
    items: [
      {
        source: "workload",
        type: "Warning",
        reason: "BackOff",
        message: "Back-off restarting failed container api",
        object: "Pod shop/api-7d9f",
        count: 7,
        last: minutesAgo(1),
      },
      {
        source: "management",
        type: "Normal",
        reason: "SuccessfulCreate",
        message: "Created machine web-md-0-a",
        object: "MachineSet web-md-0-x",
        count: 1,
        last: minutesAgo(40),
      },
    ],
  },
  logs: true,
};

const healthPath = "GET /api/clusters/default/web/health";
const logsPath = (container: string): string =>
  `GET /api/clusters/default/web/pods/shop/api-7d9f/logs?container=${container}`;

// machineOnly is a node as the server sends it when the cluster did not answer: only its machine.
const machineOnly = (name: string, role: "control-plane" | "worker") => ({
  name,
  machine: name,
  role,
  joined: false,
  ready: false,
});

describe("health page", () => {
  it("says what is wrong first, and shows a crashing pod's last run to those who may read logs", async () => {
    const routes: Record<string, Route> = {
      [healthPath]: { body: degraded },
      [logsPath("api")]: { body: { container: "api", previous: true, lines: ["starting", "panic: no database"] } },
      [logsPath("sidecar")]: { body: { container: "sidecar", previous: false, lines: [] } },
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    render(<HealthView cluster={cluster} headlamp="https://pve.home.arpa:8444/c/default.web" />);

    await expect(screen.findByText("Degraded")).resolves.toBeInTheDocument();
    const tiles = within(screen.getByRole("list", { name: "At a glance" }));
    expect(tiles.getByRole("link", { name: /Control plane\s*1 of 1 ready\s*etcd healthy/u })).toHaveAttribute(
      "href",
      "#health-nodes",
    );
    expect(tiles.getByRole("link", { name: /Node pressure\s*1 of 2 nodes/u })).toBeInTheDocument();
    expect(tiles.getByRole("link", { name: /Pods not well\s*1/u })).toHaveAttribute("href", "#health-pods");
    expect(tiles.getByRole("link", { name: /Add-ons\s*1 of 2 deployed/u })).toHaveAttribute("href", "#health-addons");
    expect(screen.getByRole("link", { name: "Pod shop/api-7d9f: CrashLoopBackOff" })).toHaveAttribute(
      "href",
      "#health-pods",
    );
    // The backups view, not this page, has the details of the cluster's backups.
    expect(screen.getByRole("link", { name: "Backups overdue" })).toHaveAttribute(
      "href",
      "/clusters/default/web/backups",
    );
    expect(screen.getByText("Memory pressure")).toBeInTheDocument();
    expect(screen.getByLabelText("web-cp-a's memory use")).toHaveAttribute("value", "2147483648");
    expect(screen.getAllByRole("link", { name: "Headlamp" })[0]).toHaveAttribute(
      "href",
      "https://pve.home.arpa:8444/c/default.web/pods/shop/api-7d9f",
    );

    await user.click(within(screen.getByRole("row", { name: /^api-7d9f/u })).getByRole("button", { name: "Logs" }));
    const dialog = within(await screen.findByRole("dialog", { name: "Logs of api-7d9f" }));
    await expect(dialog.findByLabelText("Log of api")).resolves.toHaveTextContent("panic: no database");
    expect(dialog.getByText("Previous run")).toBeInTheDocument();
    await user.selectOptions(dialog.getByLabelText("Container"), "sidecar");
    await expect(dialog.findByLabelText("Log of sidecar")).resolves.toHaveTextContent("No lines yet");
    expect(reads(calls, logsPath("sidecar"))).toBe(1);
  });

  it("offers no logs without the right, and says why nothing from inside the cluster shows", async () => {
    fakeServer({
      [healthPath]: {
        body: {
          ...degraded,
          logs: false,
          workload: "The cluster's API server is not up yet.",
          nodes: { items: [] },
          pods: { items: [] },
          attention: [],
        },
      },
    });
    render(<HealthView cluster={cluster} headlamp={undefined} />);

    await expect(screen.findByText("The cluster's API server is not up yet.")).resolves.toBeInTheDocument();
    expect(
      within(screen.getByRole("list", { name: "At a glance" })).getByRole("link", {
        name: /Pods not well\s*Not read/u,
      }),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Logs" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Headlamp" })).not.toBeInTheDocument();
  });

  it("reads no node's readiness off a cluster that does not answer", async () => {
    fakeServer({
      [healthPath]: {
        body: {
          ...degraded,
          verdict: "unreachable",
          workload: "Its API server does not answer: connection refused",
          nodes: { items: [machineOnly("web-cp-a", "control-plane"), machineOnly("web-md-0-a", "worker")] },
          pods: { items: [] },
          attention: [],
        },
      },
    });
    render(<HealthView cluster={cluster} headlamp={undefined} />);

    const glance = within(await screen.findByRole("list", { name: "At a glance" }));
    for (const tile of ["Control plane", "Workers", "Node pressure", "Pods not well"]) {
      expect(glance.getByRole("link", { name: new RegExp(`${tile}\\s*Not read`, "u") })).toBeInTheDocument();
    }
  });

  it("says the autoscaler brings workers to a pool at none, which keeps the control plane to itself", async () => {
    const atNone = { name: "md-0", desired: 0, ready: 0, available: 0, upToDate: 0, rollingOut: false };
    fakeServer({
      [healthPath]: {
        body: {
          ...degraded,
          nodes: { items: degraded.nodes.items.filter((n) => n.role === "control-plane") },
          pools: { items: [{ ...atNone, autoscaling: { min: 0, max: 3 } }] },
        },
      },
    });
    render(<HealthView cluster={cluster} />);

    const tiles = within(await screen.findByRole("list", { name: "At a glance" }));
    expect(
      tiles.getByRole("link", {
        name: /Workers\s*None\s*Autoscaled from 0/u,
      }),
    ).toBeInTheDocument();
  });

  it("asks again more often while the cluster is not healthy", async () => {
    const routes: Record<string, Route> = { [healthPath]: { body: degraded } };
    const calls = fakeServer(routes);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    render(<HealthView cluster={cluster} />);
    await expect(screen.findByText("Degraded")).resolves.toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(reads(calls, healthPath)).toBe(2);

    routes[healthPath] = { body: { ...degraded, verdict: "healthy", attention: [] } };
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    await expect(screen.findByText("Available", { selector: ".verdict *" })).resolves.toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(20_000));
    expect(reads(calls, healthPath)).toBe(3);
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(reads(calls, healthPath)).toBe(4);
    vi.useRealTimers();
  });

  it("gives the verdict the cluster's badge gives, also where only the controller has failed", async () => {
    fakeServer({ [healthPath]: { body: { ...degraded, verdict: "healthy", attention: [] } } });
    render(<HealthView cluster={{ ...cluster, controllerError: "Proxmox VE task UPID:pve1 failed: no space" }} />);
    await expect(screen.findByText("Error", { selector: ".verdict *" })).resolves.toBeInTheDocument();
    expect(screen.queryByText("Available", { selector: ".verdict *" })).not.toBeInTheDocument();
  });

  it("greys the tiles out while the last read failed, until one succeeds", async () => {
    const routes: Record<string, Route> = { [healthPath]: { body: degraded } };
    fakeServer(routes);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    render(<HealthView cluster={cluster} />);
    await expect(screen.findByRole("list", { name: "At a glance" })).resolves.not.toHaveClass("tiles-stale");
    expect(screen.queryByText(/^Stale/u)).not.toBeInTheDocument();

    routes[healthPath] = { status: 502, body: { error: "the management cluster does not answer" } };
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    await expect(screen.findByRole("list", { name: "At a glance, stale" })).resolves.toHaveClass("tiles-stale");
    expect(screen.getByText(/^Stale, read at/u)).toHaveClass("hint-warning");
    expect(screen.getByText("Degraded")).toBeInTheDocument();

    routes[healthPath] = { body: degraded };
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    await expect(screen.findByRole("list", { name: "At a glance" })).resolves.not.toHaveClass("tiles-stale");
    vi.useRealTimers();
  });

  it("greys the tiles out once a read hangs, and reads again once it is given up", async () => {
    const routes: Record<string, Route> = { [healthPath]: { body: degraded } };
    const calls = fakeServer(routes);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    render(<HealthView cluster={cluster} />);
    await expect(screen.findByRole("list", { name: "At a glance" })).resolves.not.toHaveClass("tiles-stale");

    routes[healthPath] = held().route;
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(screen.getByRole("list", { name: "At a glance" })).not.toHaveClass("tiles-stale");
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(screen.getByRole("list", { name: "At a glance, stale" })).toHaveClass("tiles-stale");
    expect(screen.getByText(/^Stale, read at/u)).toBeInTheDocument();
    expect(reads(calls, healthPath)).toBe(2);

    routes[healthPath] = { body: degraded };
    await act(() => vi.advanceTimersByTimeAsync(20_000));
    await expect(screen.findByRole("list", { name: "At a glance" })).resolves.not.toHaveClass("tiles-stale");
    vi.useRealTimers();
  });

  it("filters the events by where they come from", async () => {
    fakeServer({ [healthPath]: { body: degraded } });
    const user = userEvent.setup({ delay: null });
    render(<HealthView cluster={cluster} />);

    await expect(screen.findByText("Back-off restarting failed container api")).resolves.toBeInTheDocument();
    expect(screen.getByText("Created machine web-md-0-a")).toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText("From"), "management");
    expect(screen.queryByText("Back-off restarting failed container api")).not.toBeInTheDocument();
    expect(screen.getByText("Created machine web-md-0-a")).toBeInTheDocument();
  });
});

import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { applyStaged, fakeServer, type Call } from "./fake-server";
import { sizeProblem } from "./pool-machine";
import { poolNameProblem } from "./Pools";
import type { Cluster } from "./types";

const web: Cluster = {
  namespace: "default",
  name: "web",
  class: "kubehenge-k3s",
  distribution: "k3s",
  version: "v1.36.5+k3s1",
  generation: 1,
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 2, ready: 2, available: 2, upToDate: 2 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 2 }],
  variables: {
    controlPlaneMachine: { cores: 2, memoryMiB: 4096, diskGiB: 30 },
    workerMachine: { cores: 2, memoryMiB: 8192, diskGiB: 40 },
  },
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

// md-0 has labels and a taint of its own. md-1 takes the cluster's labels.
const tagged: Cluster = {
  ...web,
  workerPools: [
    {
      name: "md-0",
      class: "worker",
      replicas: 2,
      nodes: {
        labels: [
          { key: "team", value: "a" },
          { key: "zone", value: "" },
        ],
        taints: [{ key: "dedicated", value: "db", effect: "NoSchedule" }],
      },
    },
    { name: "md-1", class: "worker", replicas: 1 },
  ],
  variables: { ...web.variables, workerNodes: { labels: [{ key: "team", value: "cluster" }], taints: [] } },
};

const classes = [
  {
    name: "kubehenge-k3s",
    distribution: "k3s",
    variables: [
      {
        name: "workerMachine",
        required: false,
        schema: {
          type: "object",
          properties: { cores: { minimum: 2 }, memoryMiB: { minimum: 4096 }, diskGiB: { minimum: 20 } },
        },
      },
    ],
  },
];

const tags = (pool: string): (string | null)[] =>
  within(screen.getByRole("row", { name: new RegExp(`^${pool}`, "u") }))
    .getAllByRole("listitem")
    .map((i) => i.textContent);

async function machinesView(shown: Cluster = web): Promise<{ calls: Call[]; user: UserEvent }> {
  window.history.replaceState(null, "", "/clusters/default/web/machines");
  const calls = fakeServer({
    "GET /api/session": { body: { user: "alice@pve", csrf: "c1" } },
    "GET /api/clusters/default/web": { body: shown },
    "GET /api/classes": { body: classes },
    "GET /api/proxmox/nodes": { body: [] },
    "PATCH /api/clusters/default/web": { body: shown },
  });
  render(<App />);
  await screen.findByRole("region", { name: "Pools" });
  return { calls, user: userEvent.setup({ delay: null }) };
}

const sent = (calls: Call[]): unknown => calls.findLast((c) => c.key === "PATCH /api/clusters/default/web")?.body;

describe("the pools", () => {
  it("show each pool's nodes and size", async () => {
    await machinesView();
    const rows = within(screen.getByRole("region", { name: "Pools" })).getAllByRole("row");
    expect(rows.map((r) => r.textContent)).toStrictEqual([
      "PoolNodesCoresMemoryDiskActions",
      "Control plane124 GiB30 GiBResize…",
      "md-0228 GiB40 GiBResize…Labels and taints…Delete…",
    ]);
  });

  it("show each pool's labels and taints, its own or the cluster's", async () => {
    await machinesView(tagged);
    expect(tags("md-0")).toStrictEqual(["team=a", "zone=", "dedicated=db:NoSchedule"]);
    expect(tags("md-1")).toStrictEqual(["team=cluster"]);
  });

  it("show a pool's labels and taints, and send them when saved", async () => {
    const { calls, user } = await machinesView();
    await user.click(screen.getByRole("button", { name: "Edit pool md-0 nodes" }));
    const dialog = screen.getByRole("dialog", { name: "Edit pool md-0 nodes" });
    expect(within(dialog).queryByText(/replaces this pool's nodes/u)).not.toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.click(within(dialog).getByRole("button", { name: "Add label" }));
    const label = within(dialog).getByRole("group", { name: "Label 1" });
    await user.type(within(label).getByLabelText("Key"), "team");
    await user.type(within(label).getByLabelText("Value"), "a");
    expect(
      within(dialog).getByText("Changing labels or taints replaces this pool's nodes, one at a time."),
    ).toBeVisible();
    await user.click(within(dialog).getByRole("button", { name: "Add taint" }));
    const taint = within(dialog).getByRole("group", { name: "Taint 1" });
    await user.type(within(taint).getByLabelText("Key"), "dedicated");
    await user.type(within(taint).getByLabelText("Value"), "db");
    await user.selectOptions(within(taint).getByLabelText("Effect"), "NoExecute");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await applyStaged(user);
    await waitFor(() =>
      expect(sent(calls)).toStrictEqual({
        pools: {
          "md-0": {
            nodes: {
              labels: [{ key: "team", value: "a" }],
              taints: [{ key: "dedicated", value: "db", effect: "NoExecute" }],
            },
          },
        },
      }),
    );
  });

  it("show a problem with a field only once it was touched", async () => {
    const { user } = await machinesView();
    await user.click(screen.getByRole("button", { name: "Edit pool md-0 nodes" }));
    const dialog = screen.getByRole("dialog", { name: "Edit pool md-0 nodes" });
    await user.click(within(dialog).getByRole("button", { name: "Add label" }));
    const label = within(dialog).getByRole("group", { name: "Label 1" });
    const key = within(label).getByLabelText("Key");
    expect(key).not.toHaveAccessibleDescription(/Enter a key/u);
    expect(within(label).queryByText("Enter a key")).not.toBeInTheDocument();
    await user.click(key);
    await user.tab();
    expect(key).toHaveAccessibleDescription("Enter a key");
    await user.type(key, "k8s.io/x");
    expect(key).toHaveAccessibleDescription(/The kubelet refuses labels in kubernetes.io and k8s.io/u);
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.click(within(dialog).getByRole("button", { name: "Add label" }));
    const second = within(dialog).getByRole("group", { name: "Label 2" });
    await user.type(within(second).getByLabelText("Key"), "k8s.io/x");
    expect(within(second).getByLabelText("Key")).toHaveAccessibleDescription(/kubelet refuses/u);
    await user.click(within(dialog).getByRole("button", { name: "Add taint" }));
    expect(within(within(dialog).getByRole("group", { name: "Taint 1" })).queryByText("Enter a key")).toBeNull();
  });

  it("take the cluster's labels and taints again", async () => {
    const { calls, user } = await machinesView(tagged);
    await user.click(screen.getByRole("button", { name: "Edit pool md-0 nodes" }));
    const dialog = screen.getByRole("dialog", { name: "Edit pool md-0 nodes" });
    expect(within(dialog).getByRole("group", { name: "Label 2" })).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Use the cluster's" }));
    expect(within(dialog).getByText(/replaces this pool's nodes/u)).toBeVisible();
    expect(within(dialog).queryByRole("group", { name: "Label 2" })).not.toBeInTheDocument();
    expect(within(dialog).getByLabelText("Value")).toHaveValue("cluster");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await applyStaged(user);
    await waitFor(() => expect(sent(calls)).toStrictEqual({ pools: { "md-0": { nodes: null } } }));
  });

  it("add a pool of its own size", async () => {
    const { calls, user } = await machinesView();
    await user.click(screen.getByRole("button", { name: "Add pool…" }));
    const dialog = screen.getByRole("dialog", { name: "Add pool to web" });
    await user.type(within(dialog).getByLabelText("Name"), "gpu");
    const memory = within(dialog).getByLabelText("Memory (MiB)");
    await user.clear(memory);
    await user.type(memory, "2048");
    expect(memory).toHaveAccessibleDescription(/At least 4096 MiB of memory/u);
    expect(within(dialog).getByRole("button", { name: "Add" })).toBeDisabled();
    await user.clear(memory);
    await user.type(memory, "16384");
    await user.click(within(dialog).getByRole("button", { name: "Add" }));
    await applyStaged(user);
    await waitFor(() =>
      expect(sent(calls)).toStrictEqual({
        pools: { gpu: { replicas: 1, machine: { cores: 2, memoryMiB: 16_384, diskGiB: 40 } } },
      }),
    );
  });

  it("resize the control plane", async () => {
    const { calls, user } = await machinesView();
    await user.click(screen.getByRole("button", { name: "Resize the control plane" }));
    const dialog = screen.getByRole("dialog", { name: "Resize web's control plane" });
    const cores = within(dialog).getByLabelText("Cores");
    await user.clear(cores);
    await user.type(cores, "4");
    await user.click(within(dialog).getByRole("button", { name: "Resize" }));
    await applyStaged(user);
    await waitFor(() =>
      expect(sent(calls)).toStrictEqual({
        variables: { controlPlaneMachine: { cores: 4, memoryMiB: 4096, diskGiB: 30 } },
      }),
    );
  });

  it("delete a pool", async () => {
    const { calls, user } = await machinesView();
    await user.click(screen.getByRole("button", { name: "Delete pool md-0" }));
    await user.click(
      within(screen.getByRole("dialog", { name: "Delete pool md-0" })).getByRole("button", { name: "Delete" }),
    );
    await applyStaged(user);
    await waitFor(() => expect(sent(calls)).toStrictEqual({ pools: { "md-0": null } }));
  });

  it("refuse a name the server would", () => {
    expect(poolNameProblem(web, "GPU")).toBe("Lowercase letters, digits and dashes, from a letter");
    expect(poolNameProblem(web, "md-0")).toBe("A pool has this name");
    expect(poolNameProblem(web, "p".repeat(42))).toBe("At most 41 characters for this cluster");
    expect(poolNameProblem(web, "gpu-2")).toBeUndefined();
  });

  it("hold a size to the class's least", () => {
    const min = { cores: 2, memoryMiB: 4096, diskGiB: 20 };
    expect(sizeProblem({ cores: 1, memoryMiB: 4096, diskGiB: 20 }, min)).toBe("At least 2 cores");
    expect(sizeProblem({ cores: 2, memoryMiB: 4100, diskGiB: 20 }, min)).toBe(
      "At least 4096 MiB of memory, in steps of 8",
    );
    expect(sizeProblem({ cores: 2, memoryMiB: 4096, diskGiB: 10 }, min)).toBe("At least 20 GiB of disk");
    expect(sizeProblem(min, min)).toBeUndefined();
  });
});

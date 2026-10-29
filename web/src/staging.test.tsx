import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { applyStaged, fakeServer, type Route } from "./fake-server";
import { merged } from "./Staging";
import type { Cluster, Effect, Plan } from "./types";
import { record } from "./values";

const cluster: Cluster = {
  namespace: "clusters",
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
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { headlamp: { enabled: false } },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

const path = "/api/clusters/clusters/web";

// planned answers each plan as the server would: the cluster with the staged variables, and what
// each of them replaces.
const planned: Route = (body) => {
  const variables = record(record(body)["variables"]);
  const effects: Record<string, Effect> = { headlamp: "none", monitoring: "workers" };
  const plan: Plan = {
    cluster: { ...cluster, variables: { ...cluster.variables, ...variables } },
    changes: Object.keys(variables).map((setting) => ({ setting, effect: effects[setting] ?? "all" })),
  };
  return { body: plan };
};

describe("staged changes", () => {
  it("wait in the bar until their plan is reviewed and applied together", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      [`GET ${path}`]: { body: cluster },
      [`POST ${path}/plan`]: planned,
      [`PATCH ${path}`]: { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit Headlamp" }));
    await user.click(within(screen.getByRole("dialog", { name: "Edit Headlamp" })).getByLabelText("Install Headlamp"));
    await user.click(
      within(screen.getByRole("dialog", { name: "Edit Headlamp" })).getByRole("button", { name: "Save" }),
    );
    const bar = within(await screen.findByRole("region", { name: "Staged changes" }));
    expect(bar.getByText("1 change not applied")).toBeInTheDocument();
    expect(bar.getByText("Replaces no machine")).toBeInTheDocument();
    expect(screen.getByRole("row", { name: /^Headlamp/u })).toHaveTextContent(/^HeadlampOffStaged: On/u);

    await user.click(screen.getByRole("button", { name: "Edit monitoring" }));
    const monitoring = within(screen.getByRole("dialog", { name: "Edit monitoring" }));
    await user.click(monitoring.getByLabelText("Install monitoring"));
    await user.click(monitoring.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(bar.getByText("2 changes not applied")).toBeInTheDocument());
    expect(bar.getByText("Replaces workers one by one")).toBeInTheDocument();
    expect(calls.map((c) => c.key)).not.toContain(`PATCH ${path}`);

    await user.click(bar.getByRole("button", { name: "Review plan" }));
    const plan = within(screen.getByRole("dialog", { name: "Review plan" }));
    expect(plan.getByRole("row", { name: /^Headlamp/u })).toHaveTextContent("HeadlampOffOnReplaces no machine");
    expect(plan.getByRole("row", { name: /^Monitoring/u })).toHaveTextContent(
      "MonitoringOffOnReplaces workers one by one",
    );
    await user.click(plan.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(screen.queryByRole("region", { name: "Staged changes" })).not.toBeInTheDocument());
    expect(calls.filter((c) => c.key === `PATCH ${path}`).map((c) => c.body)).toStrictEqual([
      { variables: { headlamp: { enabled: true }, monitoring: { enabled: true } } },
    ]);
    const against = (key: string): (string | null)[] =>
      calls.filter((c) => c.key === key).map((c) => c.headers.get("X-Kubehenge-Generation"));
    expect([...against(`POST ${path}/plan`), ...against(`PATCH ${path}`)]).toStrictEqual(["1", "1", "1"]);
  });

  it("are dropped, and the cluster read again, once someone else changed it meanwhile", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    let shown = cluster;
    const routes: Record<string, Route> = {
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      [`GET ${path}`]: () => ({ body: shown }),
      [`POST ${path}/plan`]: planned,
      [`PATCH ${path}`]: {
        status: 412,
        body: { error: "the cluster changed meanwhile; reload and try again" },
      },
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit Headlamp" }));
    await user.click(within(screen.getByRole("dialog", { name: "Edit Headlamp" })).getByLabelText("Install Headlamp"));
    await user.click(
      within(screen.getByRole("dialog", { name: "Edit Headlamp" })).getByRole("button", { name: "Save" }),
    );
    await screen.findByRole("region", { name: "Staged changes" });
    shown = { ...cluster, generation: 2, variables: { headlamp: { enabled: true } } };
    const reads = calls.filter((c) => c.key === `GET ${path}`).length;
    await applyStaged(user);

    await expect(screen.findByRole("alert")).resolves.toHaveTextContent(
      "Someone changed the cluster meanwhile, so the staged changes are gone.",
    );
    expect(screen.queryByRole("region", { name: "Staged changes" })).not.toBeInTheDocument();
    await waitFor(() => expect(calls.filter((c) => c.key === `GET ${path}`).length).toBeGreaterThan(reads));
    await waitFor(() => expect(screen.getByRole("row", { name: /^Headlamp/u })).toHaveTextContent(/On/u));

    // What is staged next is made against the cluster as it is now.
    await user.click(screen.getByRole("button", { name: "Edit monitoring" }));
    const monitoring = within(screen.getByRole("dialog", { name: "Edit monitoring" }));
    await user.click(monitoring.getByLabelText("Install monitoring"));
    await user.click(monitoring.getByRole("button", { name: "Save" }));
    await screen.findByRole("region", { name: "Staged changes" });
    expect(calls.findLast((c) => c.key === `POST ${path}/plan`)?.headers.get("X-Kubehenge-Generation")).toBe("2");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("are dropped by Discard without a change made", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      [`GET ${path}`]: { body: cluster },
      [`POST ${path}/plan`]: planned,
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit Headlamp" }));
    await user.click(within(screen.getByRole("dialog", { name: "Edit Headlamp" })).getByLabelText("Install Headlamp"));
    await user.click(
      within(screen.getByRole("dialog", { name: "Edit Headlamp" })).getByRole("button", { name: "Save" }),
    );
    const bar = within(await screen.findByRole("region", { name: "Staged changes" }));
    await user.click(bar.getByRole("button", { name: "Discard" }));
    expect(screen.queryByRole("region", { name: "Staged changes" })).not.toBeInTheDocument();
    expect(screen.getByRole("row", { name: /^Headlamp/u })).not.toHaveTextContent(/Staged/u);
    expect(calls.map((c) => c.key)).not.toContain(`PATCH ${path}`);
  });

  it("merge, a later change of a setting winning", () => {
    expect(
      merged(
        { version: "v1", workerPools: { a: 1 }, variables: { logs: 1, headlamp: 1 } },
        { workerPools: { b: 2 }, variables: { logs: 2 }, autoscaling: { a: null } },
      ),
    ).toStrictEqual({
      version: "v1",
      workerPools: { a: 1, b: 2 },
      autoscaling: { a: null },
      variables: { logs: 2, headlamp: 1 },
    });
  });
});

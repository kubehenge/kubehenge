import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fieldProblems, newForm, request, summary } from "./create-form";
import { etcdQuotaOf, etcdQuotaProblem, etcdText } from "./EtcdSize";
import { applyStaged, fakeServer } from "./fake-server";
import { fromCluster } from "./start-from";
import type { Cluster, ClusterClass, EtcdSizes } from "./types";

const rke2: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  variables: [
    { name: "storage", required: true, schema: { type: "string" } },
    {
      name: "etcd",
      required: false,
      schema: { type: "object", properties: { quotaGiB: { type: "integer", minimum: 2, maximum: 8, default: 8 } } },
    },
  ],
};

const gib = 1024 ** 3;
const read = new Date(Date.now() - 5 * 60_000).toISOString();

const sizes: EtcdSizes = {
  read,
  quotaBytes: 8 * gib,
  warning: "etcd member web-cp-b is at 88% of its limit",
  members: [
    { name: "web-cp-a", leader: true, dbSize: 5 * gib, dbSizeInUse: gib, quotaPercent: 63 },
    { name: "web-cp-b", dbSize: 7 * gib, dbSizeInUse: 6 * gib, quotaPercent: 88 },
    { name: "web-cp-c", dbSize: gib, dbSizeInUse: gib / 2, learner: true, quotaPercent: 13 },
  ],
};

const cluster: Cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5+rke2r1",
  generation: 1,
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 3, ready: 3, available: 3, upToDate: 3 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { etcd: { quotaGiB: 6 } },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
  etcd: sizes,
};

const signedIn = { "GET /api/session": { body: { user: "dev@pve", csrf: "c", canTrust: true } } };

describe("etcd limit", () => {
  it("reads the variable with the chart's default and refuses what the class would", () => {
    expect(etcdQuotaOf({ quotaGiB: 5 })).toBe(5);
    expect(etcdQuotaOf(undefined)).toBe(8);
    expect(etcdText(6)).toBe("6 GiB database limit");
    expect(etcdQuotaProblem("4")).toBeUndefined();
    for (const bad of ["", "1", "9", "2.5", "x"]) {
      expect(etcdQuotaProblem(bad)).toBe("A whole number from 2 to 8");
    }
  });

  it("is a field of the wizard's Machines step, sent only when it differs from the default", () => {
    const form = { ...newForm(), name: "web", etcdQuotaGiB: 4 };
    expect(fieldProblems("Machines", { ...form, storage: "local-lvm" }, rke2)["etcdQuotaGiB"]).toBeUndefined();
    expect(fieldProblems("Machines", { ...form, etcdQuotaGiB: 9 }, rke2)["etcdQuotaGiB"]).toBe(
      "A whole number from 2 to 8",
    );
    expect(fieldProblems("Machines", { ...form, etcdQuotaGiB: Number.NaN }, rke2)["etcdQuotaGiB"]).toBe(
      "A whole number from 2 to 8",
    );
    expect(request(form, rke2).variables["etcd"]).toStrictEqual({ quotaGiB: 4 });
    expect(request({ ...form, etcdQuotaGiB: 8 }, rke2).variables).not.toHaveProperty("etcd");
    expect(summary(form, rke2)).toContainEqual(["etcd", "4 GiB database limit"]);
    expect(fromCluster(cluster, { name: "copy", namespace: "", className: rke2.name, version: "" }).etcdQuotaGiB).toBe(
      6,
    );
  });

  it("lists each member's database on the cluster page and defragments them on request", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/backups");
    let now = cluster;
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": () => ({ body: now }),
      "GET /api/clusters/clusters/web/backups": {
        body: { enabled: false, reachable: true, backups: [], restores: [], canBackUp: true, canRestore: true },
      },
      "GET /api/clusters/clusters/web/etcd-snapshots": {
        body: {
          enabled: false,
          snapshots: [],
          restoring: false,
          requested: false,
          canSnapshot: true,
          canRestore: true,
        },
      },
      "POST /api/clusters/clusters/web/etcd-defrag": () => {
        now = { ...cluster, etcd: { ...sizes, requested: true } };
        return { status: 202, body: { by: "dev@pve", at: read } };
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(screen.findByRole("heading", { name: "etcd size" })).resolves.toBeInTheDocument();
    // The warning and the table give the one share the server computed.
    expect(screen.getByText("etcd member web-cp-b is at 88% of its limit")).toBeInTheDocument();
    const leader = screen.getByRole("row", { name: /web-cp-a/u });
    expect(leader).toHaveTextContent("web-cp-a (leader)");
    expect(leader).toHaveTextContent("5.0 GiB");
    expect(screen.getByRole("row", { name: /web-cp-b/u })).toHaveTextContent("88% of the limit");
    expect(screen.getByRole("row", { name: /web-cp-c/u })).toHaveTextContent("Learner");

    await user.click(screen.getByRole("button", { name: "Defragment now" }));
    await waitFor(() => expect(calls.map((c) => c.key)).toContain("POST /api/clusters/clusters/web/etcd-defrag"));
    await expect(screen.findByRole("button", { name: "Defragmenting…" })).resolves.toBeDisabled();
  });

  it("is raised in a cluster's options, and refused below what it is, only once typed", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": { body: cluster },
      "GET /api/classes": { body: [rke2] },
      "PATCH /api/clusters/clusters/web": { body: { ...cluster, variables: { etcd: { quotaGiB: 7 } } } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(screen.findByText("6 GiB database limit")).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Edit the etcd database limit" }));
    const dialog = screen.getByRole("dialog", { name: "Edit etcd" });
    const field = within(dialog).getByLabelText("Database limit (GiB)");
    expect(within(dialog).queryByText(/Not lower than/u)).not.toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();

    await user.clear(field);
    await user.type(field, "4");
    expect(within(dialog).getByText("Not lower than 6")).toBeInTheDocument();
    await user.clear(field);
    await user.type(field, "7");
    expect(within(dialog).queryByText("Not lower than 6")).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { etcd: { quotaGiB: 7 } },
    });
  });
});

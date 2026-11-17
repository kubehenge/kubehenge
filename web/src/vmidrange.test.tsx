import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fieldProblems, newForm, request, summary } from "./create-form";
import { applyStaged, fakeServer } from "./fake-server";
import type { ClusterClass } from "./types";
import { VMIDRangeStep, vmIDRangeOf, vmIDRangeProblem, vmIDRangeText } from "./VmidRange";

const cluster = {
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
  variables: { vmIDRange: { start: 10_000, end: 10_099 } },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

const names = ["controlPlaneEndpoint", "network", "storage", "controlPlaneMachine", "workerMachine", "vmIDRange"];

const cls: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  annotations: {},
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  variables: names.map((name) => ({ name, required: false, schema: {} })),
};

const sited: ClusterClass = { ...cls, annotations: { "kubehenge.dev/vm-id-range": "20000-20999" } };

describe("the create wizard's VM ID range", () => {
  it("sends the range only when one is typed, and only for a class that defines it", () => {
    const form = { ...newForm(), name: "web", storage: "tank" };
    expect(request(form, cls).variables).not.toHaveProperty("vmIDRange");
    expect(request({ ...form, vmIDRange: "10000-10999" }, cls).variables).toMatchObject({
      vmIDRange: { start: 10_000, end: 10_999 },
    });
    const without = { ...cls, variables: cls.variables.filter((v) => v.name !== "vmIDRange") };
    expect(request({ ...form, vmIDRange: "10000-10999" }, without).variables).not.toHaveProperty("vmIDRange");
  });

  it("holds the step back for a range the server refuses, the site's included", () => {
    // Three control planes and one worker are four nodes, with a spare for the control plane and the pool.
    const form = { ...newForm(), storage: "tank", controlPlaneReplicas: 3, workerReplicas: 1 };
    const problem = (range: string, c: ClusterClass = cls): string | undefined =>
      fieldProblems("Machines", { ...form, vmIDRange: range }, c).vmIDRange;
    expect(problem("")).toBeUndefined();
    expect(problem("10000-10005")).toBeUndefined();
    expect(problem("10000-10004")).toBe("Holds 5 IDs. Nodes and their upgrades need 6");
    expect(problem("2000-1000")).toBe("The start is above the end");
    expect(problem("99-1000")).toBe("IDs run from 100 to 999999999");
    expect(problem("ten")).toBe("Write it like 100000-100999");
    const narrow: ClusterClass = { ...cls, annotations: { "kubehenge.dev/vm-id-range": "20000-20002" } };
    expect(problem("", narrow)).toBe("Holds 3 IDs. Nodes and their upgrades need 6");
  });

  it("says the range, the site's or the next free ID in the summary", () => {
    const form = { ...newForm(), name: "web", storage: "tank" };
    const row = (c: ClusterClass, range: string): string | undefined =>
      summary({ ...form, vmIDRange: range }, c).find(([label]) => label === "VM ID range")?.[1];
    expect(row(cls, "")).toBe("Next free ID");
    expect(row(sited, "")).toBe("20000-20999 (site)");
    expect(row(sited, "10000-10999")).toBe("10000-10999");
  });
});

describe("the wizard's VM ID range field", () => {
  it("shows the field with the site's range as its hint, only for a class that has the variable", async () => {
    const user = userEvent.setup({ delay: null });
    const bare = { ...cls, variables: cls.variables.filter((v) => v.name !== "vmIDRange") };
    const typed: string[] = [];
    const onChange = (r: string): void => {
      typed.push(r);
    };
    const { rerender } = render(<VMIDRangeStep cls={bare} value="" onChange={onChange} problem={undefined} />);
    expect(screen.queryByLabelText("VM ID range")).not.toBeInTheDocument();
    rerender(<VMIDRangeStep cls={sited} value="" onChange={onChange} problem="Holds 3 IDs" />);
    const field = screen.getByLabelText("VM ID range");
    expect(field.closest(".field")).toHaveTextContent("Empty takes the site's, 20000-20999");
    expect(field.closest(".field")).toHaveTextContent("Holds 3 IDs");
    await user.type(field, "7");
    expect(typed).toStrictEqual(["7"]);
    rerender(<VMIDRangeStep cls={cls} value="" onChange={onChange} problem={undefined} />);
    expect(screen.getByLabelText("VM ID range").closest(".field")).toHaveTextContent("Empty takes the next free ID");
  });
});

describe("the VM ID range", () => {
  it("reads and says the range", () => {
    expect(vmIDRangeOf(undefined)).toBe("");
    expect(vmIDRangeOf({ start: 100, end: 200 })).toBe("100-200");
    expect(vmIDRangeText("", "")).toBe("Next free ID");
  });

  it("accepts a range inside Proxmox VE's limits that holds the nodes and refuses the rest", () => {
    for (const ok of ["", "100-100", "100-999999999", "10000-10999"]) {
      expect(vmIDRangeProblem(ok)).toBeUndefined();
    }
    for (const bad of ["1000", "a-b", "200-100", "99-200", "100-1000000000", "-5-10"]) {
      expect(vmIDRangeProblem(bad)).toBeDefined();
    }
    expect(vmIDRangeProblem("100-105", 6)).toBeUndefined();
    expect(vmIDRangeProblem("100-105", 7)).toBe("Holds 6 IDs. Nodes and their upgrades need 7");
  });

  it("is changed from the options with the range the cluster sends", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const row = await screen.findByRole("rowheader", { name: "VM ID range" });
    expect(row.closest("tr")).toHaveTextContent("10000-10099");
    await user.click(screen.getByRole("button", { name: "Edit the VM ID range" }));
    const dialog = screen.getByRole("dialog", { name: "Edit VM ID range" });
    const field = within(dialog).getByLabelText("VM ID range");
    const save = within(dialog).getByRole("button", { name: "Add to plan" });
    expect(save).toBeDisabled();
    // Three control planes and one worker are four nodes, with a spare for the control plane and the pool.
    await user.clear(field);
    await user.type(field, "20000-20003");
    expect(save).toBeDisabled();
    expect(within(dialog).getByText("Holds 4 IDs. Nodes and their upgrades need 6")).toBeInTheDocument();
    await user.clear(field);
    await user.type(field, "20000-20999");
    await user.click(save);
    await applyStaged(user);
    await waitFor(() =>
      expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
        variables: { vmIDRange: { start: 20_000, end: 20_999 } },
      }),
    );
  });

  it("clears the range from the options, as kubectl can", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Edit the VM ID range" }));
    const dialog = screen.getByRole("dialog", { name: "Edit VM ID range" });
    await user.clear(within(dialog).getByLabelText("VM ID range"));
    await user.click(within(dialog).getByRole("button", { name: "Add to plan" }));
    await applyStaged(user);
    await waitFor(() =>
      expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
        variables: { vmIDRange: null },
      }),
    );
  });
});

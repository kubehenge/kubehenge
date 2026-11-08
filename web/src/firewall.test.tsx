import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { JSX } from "react";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { fieldProblems, newForm, request, summary, type FieldKey, type Form } from "./create-form";
import { NetworkStep } from "./CreateSteps";
import { applyStaged, fakeServer } from "./fake-server";
import { securityGroupsProblem, securityGroupsText } from "./Firewall";
import type { Cluster, ClusterClass } from "./types";

const withGroups: ClusterClass = {
  name: "kubehenge-k3s",
  distribution: "k3s",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+k3s\d+$` },
  variables: [
    { name: "network", required: true, schema: { type: "object" } },
    { name: "securityGroups", required: false, schema: { type: "array" } },
  ],
};
const withoutGroups: ClusterClass = { ...withGroups, variables: withGroups.variables.slice(0, 1) };

const filled: Form = { ...newForm(), bridge: "vmbr0", gateway: "10.0.0.1", dnsServers: "10.0.0.1" };

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
  variables: {},
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

describe("a new cluster's firewall security groups", () => {
  it("are asked for where the class has them", async () => {
    const set = vi.fn<(k: keyof Form, v: unknown) => void>();
    const noProblems = new Map<FieldKey, string>();
    const step = (cls: ClusterClass): JSX.Element => (
      <NetworkStep form={newForm()} set={set} shown={(k) => noProblems.get(k)} cls={cls} bridgeOptions={[]} />
    );
    const user = userEvent.setup({ delay: null });
    const { rerender } = render(step(withGroups));

    await user.type(screen.getByLabelText("Firewall security groups"), "k");
    expect(set).toHaveBeenLastCalledWith("securityGroups", "k");

    rerender(step(withoutGroups));
    expect(screen.queryByLabelText("Firewall security groups")).not.toBeInTheDocument();
  });

  it("are sent and summed up only when entered", () => {
    expect(request(filled, withGroups).variables).not.toHaveProperty("securityGroups");
    const form = { ...filled, securityGroups: " k8s, corp-base " };
    expect(request(form, withGroups).variables["securityGroups"]).toStrictEqual(["k8s", "corp-base"]);
    expect(request(form, withoutGroups).variables).not.toHaveProperty("securityGroups");
    expect(Object.fromEntries(summary(form, withGroups))).toMatchObject({ "Security groups": "k8s, corp-base" });
    expect(summary(filled, withGroups).map(([label]) => label)).not.toContain("Security groups");
  });

  it("follow Proxmox VE's names", () => {
    for (const ok of ["", "k8s", "corp_base, k8s-nodes", "abcdefghijklmnopqr"]) {
      expect(securityGroupsProblem(ok)).toBeUndefined();
      expect(fieldProblems("Network", { ...filled, securityGroups: ok }, withGroups)).not.toHaveProperty(
        "securityGroups",
      );
    }
    for (const bad of ["1k8s", "k", "abcdefghijklmnopqrs", "k8s.nodes", "k8s nodes;x"]) {
      expect(securityGroupsProblem(bad)).toBe("Names of 2 to 18 letters, digits, - and _, starting with a letter");
    }
    expect(securityGroupsProblem("k8s, k8s")).toBe("Each group once");
    expect(fieldProblems("Network", { ...filled, securityGroups: "1k8s" }, withGroups)["securityGroups"]).toBe(
      "Names of 2 to 18 letters, digits, - and _, starting with a letter",
    );
  });
});

describe("a cluster's firewall security groups", () => {
  it("read Off until set, refuse a bad name and save the groups", async () => {
    expect(securityGroupsText("")).toBe("Off");
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit the firewall security groups" }));
    const dialog = screen.getByRole("dialog", { name: "Edit firewall security groups" });
    const save = within(dialog).getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    await user.type(within(dialog).getByLabelText("Firewall security groups"), "k8s, 1bad");
    await user.click(save);
    expect(within(dialog).getByText(/starting with a letter/u)).toBeInTheDocument();
    expect(calls.filter((c) => c.key === "PATCH /api/clusters/clusters/web")).toHaveLength(0);
    await user.clear(within(dialog).getByLabelText("Firewall security groups"));
    await user.type(within(dialog).getByLabelText("Firewall security groups"), "k8s, corp-base");
    await user.click(save);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { securityGroups: ["k8s", "corp-base"] },
    });
  });

  it("list the saved groups and clear the variable to turn the firewall off", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const held = { ...cluster, variables: { securityGroups: ["k8s", "corp-base"] } };
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: held },
      "PATCH /api/clusters/clusters/web": { body: held },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(screen.findByRole("cell", { name: "k8s, corp-base" })).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Edit the firewall security groups" }));
    const dialog = screen.getByRole("dialog", { name: "Edit firewall security groups" });
    expect(within(dialog).getByLabelText("Firewall security groups")).toHaveValue("k8s, corp-base");
    await user.clear(within(dialog).getByLabelText("Firewall security groups"));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { securityGroups: null },
    });
  });
});

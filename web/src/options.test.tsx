import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fakeServer } from "./fake-server";

const cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-k3s",
  distribution: "k3s",
  version: "v1.36.5+k3s1",
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { headlamp: { enabled: false }, oidc: [] },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

describe("a cluster's options", () => {
  it("save only a change", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit Headlamp" }));
    let dialog = screen.getByRole("dialog", { name: "Headlamp" });
    const install = within(dialog).getByLabelText("Install Headlamp");
    expect(install).toHaveFocus();
    expect(install).toHaveAccessibleDescription(/inside the cluster/u);
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.click(install);
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { headlamp: { enabled: true } },
    });

    await user.click(screen.getByRole("button", { name: "Edit monitoring" }));
    dialog = screen.getByRole("dialog", { name: "Monitoring" });
    await user.click(within(dialog).getByLabelText("Install monitoring"));
    await user.type(within(dialog).getByLabelText("Grafana host name"), "grafana.example.org");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { monitoring: { enabled: true, hostname: "grafana.example.org" } },
    });

    expect(screen.getByRole("row", { name: /Pod security/u })).toHaveTextContent("Not enforced");
    await user.click(screen.getByRole("button", { name: "Edit pod security" }));
    dialog = screen.getByRole("dialog", { name: "Pod security" });
    const admission = within(dialog).getByLabelText("Pod Security Admission");
    expect(admission).toHaveAccessibleDescription(/replaces the control-plane nodes/u);
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.selectOptions(admission, "baseline");
    await user.selectOptions(within(dialog).getByLabelText("Kyverno"), "restricted");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { podSecurity: { admission: "baseline", kyverno: "restricted" } },
    });

    expect(screen.getByRole("row", { name: /Istio/u })).toHaveTextContent("Off");
    await user.click(screen.getByRole("button", { name: "Edit Istio" }));
    dialog = screen.getByRole("dialog", { name: "Istio" });
    const istio = within(dialog).getByLabelText("Install Istio");
    expect(istio).toHaveAccessibleDescription(/takes every namespace and pod out of the mesh/u);
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.click(istio);
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { istio: { enabled: true } },
    });

    await user.click(screen.getByRole("button", { name: "Edit Rancher" }));
    dialog = screen.getByRole("dialog", { name: "Rancher" });
    const register = within(dialog).getByLabelText("Register with Rancher");
    expect(register).toHaveAccessibleDescription(/removes Rancher's agent/u);
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.click(register);
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeEnabled();
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));

    await user.click(screen.getByRole("button", { name: "Edit identity providers" }));
    dialog = screen.getByRole("dialog", { name: "Identity providers" });
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
  });

  it("hold pods to restricted under CIS hardening, which cannot change", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const hardened = {
      ...cluster,
      variables: { ...cluster.variables, cis: true, podSecurity: { kyverno: "baseline" } },
    };
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: hardened },
      "PATCH /api/clusters/clusters/web": { body: hardened },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    const row = await screen.findByRole("row", { name: /CIS hardening/u });
    expect(row).toHaveTextContent("On");
    expect(within(row).queryByRole("button")).not.toBeInTheDocument();
    expect(screen.getByRole("row", { name: /Pod security/u })).toHaveTextContent(
      "Restricted by Pod Security Admission, Baseline by Kyverno",
    );
    await user.click(screen.getByRole("button", { name: "Edit pod security" }));
    const dialog = screen.getByRole("dialog", { name: "Pod security" });
    const admission = within(dialog).getByLabelText("Pod Security Admission");
    expect(admission).toBeDisabled();
    expect(admission).toHaveValue("restricted");
    await user.selectOptions(within(dialog).getByLabelText("Kyverno"), "restricted");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { podSecurity: { admission: "restricted", kyverno: "restricted" } },
    });
  });

  it("switch the load balancer, keeping its addresses while it is off", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const onNetwork = {
      ...cluster,
      variables: {
        ...cluster.variables,
        controlPlaneEndpoint: "10.0.0.5",
        network: { bridge: "vmbr0", addresses: ["10.0.0.10-10.0.0.20"], prefix: 24, gateway: "10.0.0.1" },
      },
    };
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: onNetwork },
      "PATCH /api/clusters/clusters/web": { body: onNetwork },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    const row = await screen.findByRole("row", { name: /Load balancer/u });
    expect(row).toHaveTextContent("k3s's own, on the nodes' addresses");
    await user.click(screen.getByRole("button", { name: "Edit the load balancer" }));
    const dialog = screen.getByRole("dialog", { name: "Load balancer" });
    const save = within(dialog).getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    await user.selectOptions(within(dialog).getByLabelText("Load balancer"), "metallb");
    const addresses = within(dialog).getByLabelText("Load-balancer addresses");
    await user.type(addresses, "10.0.0.15-10.0.0.30");
    expect(addresses).toHaveAccessibleDescription(/10.0.0.15-10.0.0.30 overlaps the node addresses/u);
    expect(save).toBeDisabled();
    await user.clear(addresses);
    await user.type(addresses, "10.0.0.100-10.0.0.120");
    await user.click(save);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { loadBalancer: { enabled: true, implementation: "metallb", addresses: ["10.0.0.100-10.0.0.120"] } },
    });
  });

  it("switch the load balancer off, keeping what it held", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const balanced = {
      ...cluster,
      variables: {
        ...cluster.variables,
        loadBalancer: { enabled: true, implementation: "kube-vip", addresses: ["10.0.0.100-10.0.0.120"] },
      },
    };
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: balanced },
      "PATCH /api/clusters/clusters/web": { body: balanced },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    const row = await screen.findByRole("row", { name: /Load balancer/u });
    expect(row).toHaveTextContent("kube-vip, on 10.0.0.100-10.0.0.120");
    await user.click(screen.getByRole("button", { name: "Edit the load balancer" }));
    const dialog = screen.getByRole("dialog", { name: "Load balancer" });
    await user.selectOptions(within(dialog).getByLabelText("Load balancer"), "");
    expect(within(dialog).queryByLabelText("Load-balancer addresses")).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { loadBalancer: { enabled: false, implementation: "kube-vip", addresses: ["10.0.0.100-10.0.0.120"] } },
    });
  });

  it("keep logs only beside monitoring", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(screen.findByRole("row", { name: /Logs/u })).resolves.toHaveTextContent("Off");
    await user.click(screen.getByRole("button", { name: "Edit logs" }));
    const dialog = screen.getByRole("dialog", { name: "Logs" });
    const keep = within(dialog).getByLabelText("Keep logs");
    expect(keep).toBeDisabled();
    expect(keep).toHaveAccessibleDescription(/need monitoring on/u);
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(calls.map((c) => c.key)).not.toContain("PATCH /api/clusters/clusters/web");
  });

  it("change how logs are collected and kept, the volume only growing", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const logged = {
      ...cluster,
      variables: {
        ...cluster.variables,
        monitoring: { enabled: true },
        logs: { enabled: true, collector: "node", retentionDays: 7, sizeGiB: 10 },
      },
    };
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: logged },
      "PATCH /api/clusters/clusters/web": { body: logged },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(screen.findByRole("row", { name: /Logs/u })).resolves.toHaveTextContent(
      "On, collected on every node, 7 days in 10 GiB",
    );
    await user.click(screen.getByRole("button", { name: "Edit logs" }));
    const dialog = screen.getByRole("dialog", { name: "Logs" });
    const save = within(dialog).getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    const node = within(dialog).getByLabelText("On every node (recommended)");
    expect(node).toBeChecked();
    expect(node).toHaveAccessibleDescription(/privileged logging-node namespace/u);
    const api = within(dialog).getByLabelText("Through the Kubernetes API");
    expect(api).toHaveAccessibleDescription(/no journals and no audit log/u);
    await user.click(api);
    const size = within(dialog).getByLabelText("Loki's volume (GiB)");
    await user.clear(size);
    await user.type(size, "5");
    expect(size).toHaveAccessibleDescription(/only grows: at least 10 GiB/u);
    expect(save).toBeDisabled();
    await user.clear(size);
    await user.type(size, "20");
    await user.click(save);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { logs: { enabled: true, collector: "api", retentionDays: 7, sizeGiB: 20 } },
    });
  });

  it("switch logs off with monitoring", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const logged = {
      ...cluster,
      variables: {
        ...cluster.variables,
        monitoring: { enabled: true },
        logs: { enabled: true, collector: "node", retentionDays: 7, sizeGiB: 10 },
      },
    };
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: logged },
      "PATCH /api/clusters/clusters/web": { body: logged },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit monitoring" }));
    const dialog = screen.getByRole("dialog", { name: "Monitoring" });
    expect(within(dialog).queryByRole("status")).not.toBeInTheDocument();
    await user.click(within(dialog).getByLabelText("Install monitoring"));
    expect(within(dialog).getByRole("status")).toHaveTextContent(/Logs are switched off with monitoring/u);
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: {
        monitoring: { enabled: false },
        logs: { enabled: false, collector: "node", retentionDays: 7, sizeGiB: 10 },
      },
    });
  });
});

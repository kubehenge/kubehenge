import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { applyStaged, fakeServer } from "./fake-server";

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
    let dialog = screen.getByRole("dialog", { name: "Edit Headlamp" });
    const install = within(dialog).getByLabelText("Install Headlamp");
    expect(install).toHaveFocus();
    expect(within(dialog).getByRole("button", { name: "About Headlamp" })).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.click(install);
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { headlamp: { enabled: true } },
    });

    await user.click(screen.getByRole("button", { name: "Edit monitoring" }));
    dialog = screen.getByRole("dialog", { name: "Edit monitoring" });
    await user.click(within(dialog).getByLabelText("Install monitoring"));
    await user.type(within(dialog).getByLabelText("Grafana host name"), "grafana.example.org");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { monitoring: { enabled: true, hostname: "grafana.example.org" } },
    });

    expect(screen.getByRole("row", { name: /Pod security/u })).toHaveTextContent("Not enforced");
    await user.click(screen.getByRole("button", { name: "Edit pod security" }));
    dialog = screen.getByRole("dialog", { name: "Edit pod security" });
    const admission = within(dialog).getByLabelText("Pod Security Admission");
    expect(within(dialog).getByText(/replaces the control-plane nodes/u)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.selectOptions(admission, "baseline");
    await user.selectOptions(within(dialog).getByLabelText("Kyverno"), "restricted");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { podSecurity: { admission: "baseline", kyverno: "restricted" } },
    });

    expect(screen.getByRole("row", { name: /Istio/u })).toHaveTextContent("Off");
    await user.click(screen.getByRole("button", { name: "Edit Istio" }));
    dialog = screen.getByRole("dialog", { name: "Edit Istio" });
    const istio = within(dialog).getByLabelText("Install Istio");
    expect(istio).toHaveAccessibleDescription(/dataplane-mode=ambient/u);
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.click(istio);
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { istio: { enabled: true } },
    });

    await user.click(screen.getByRole("button", { name: "Edit Rancher" }));
    dialog = screen.getByRole("dialog", { name: "Edit Rancher" });
    const register = within(dialog).getByLabelText("Register with Rancher");
    expect(register).toHaveAccessibleDescription(/Joins the Rancher/u);
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.click(register);
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeEnabled();
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));

    await user.click(screen.getByRole("button", { name: "Edit identity providers" }));
    dialog = screen.getByRole("dialog", { name: "Edit identity providers" });
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
    expect(screen.getByRole("row", { name: /Pod security/u })).toHaveTextContent("Restricted, Kyverno baseline");
    await user.click(screen.getByRole("button", { name: "Edit pod security" }));
    const dialog = screen.getByRole("dialog", { name: "Edit pod security" });
    const admission = within(dialog).getByLabelText("Pod Security Admission");
    expect(admission).toBeDisabled();
    expect(admission).toHaveValue("restricted");
    await user.selectOptions(within(dialog).getByLabelText("Kyverno"), "restricted");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
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
    expect(row).toHaveTextContent("k3s's own");
    await user.click(screen.getByRole("button", { name: "Edit the load balancer" }));
    const dialog = screen.getByRole("dialog", { name: "Edit load balancer" });
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
    await applyStaged(user);
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
    const dialog = screen.getByRole("dialog", { name: "Edit load balancer" });
    await user.selectOptions(within(dialog).getByLabelText("Load balancer"), "");
    expect(within(dialog).queryByLabelText("Load-balancer addresses")).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
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
    const dialog = screen.getByRole("dialog", { name: "Edit logs" });
    const keep = within(dialog).getByLabelText("Keep logs");
    expect(keep).toBeDisabled();
    expect(keep).toHaveAccessibleDescription(/Needs monitoring on/u);
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
      "On, every node, 7 days, 10 GiB",
    );
    await user.click(screen.getByRole("button", { name: "Edit logs" }));
    const dialog = screen.getByRole("dialog", { name: "Edit logs" });
    const save = within(dialog).getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    const node = within(dialog).getByLabelText("On every node (recommended)");
    expect(node).toBeChecked();
    expect(within(dialog).getByText(/in a privileged namespace/u)).toBeInTheDocument();
    const api = within(dialog).getByLabelText("Through the Kubernetes API");
    expect(within(dialog).getByText(/Misses journals, the audit log/u)).toBeInTheDocument();
    await user.click(api);
    const size = within(dialog).getByLabelText("Volume (GiB)");
    await user.clear(size);
    await user.type(size, "5");
    expect(size).toHaveAccessibleDescription(/can't shrink. Enter 10 or more/u);
    expect(save).toBeDisabled();
    await user.clear(size);
    await user.type(size, "20");
    await user.click(save);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
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
    const dialog = screen.getByRole("dialog", { name: "Edit monitoring" });
    expect(within(dialog).queryByRole("status")).not.toBeInTheDocument();
    await user.click(within(dialog).getByLabelText("Install monitoring"));
    expect(within(dialog).getByRole("status")).toHaveTextContent(/Logs switch off too/u);
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: {
        monitoring: { enabled: false },
        logs: { enabled: false, collector: "node", retentionDays: 7, sizeGiB: 10 },
      },
    });
  });

  it("switch Headlamp's view access on and off with its host name kept", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    let shown = {
      ...cluster,
      variables: { headlamp: { enabled: true, hostname: "headlamp.example.org", access: "token" }, oidc: [] },
    };
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": () => ({ body: shown }),
      "PATCH /api/clusters/clusters/web": () => ({ body: shown }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(screen.findByRole("row", { name: /Headlamp/u })).resolves.toHaveTextContent(
      /On, at headlamp\.example\.orgEdit$/u,
    );
    await user.click(screen.getByRole("button", { name: "Edit Headlamp" }));
    let dialog = screen.getByRole("dialog", { name: "Edit Headlamp" });
    const view = within(dialog).getByLabelText("Read-only without sign-in");
    expect(view).not.toBeChecked();
    expect(view).toHaveAccessibleDescription(/Secrets excepted/u);
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.click(view);
    shown = {
      ...cluster,
      variables: { headlamp: { enabled: true, hostname: "headlamp.example.org", access: "view" }, oidc: [] },
    };
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { headlamp: { enabled: true, hostname: "headlamp.example.org", access: "view" } },
    });
    await waitFor(() =>
      expect(screen.getByRole("row", { name: /Headlamp/u })).toHaveTextContent(
        "On, at headlamp.example.org, read-only for everyone",
      ),
    );

    await user.click(screen.getByRole("button", { name: "Edit Headlamp" }));
    dialog = screen.getByRole("dialog", { name: "Edit Headlamp" });
    await user.click(within(dialog).getByLabelText("Read-only without sign-in"));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { headlamp: { enabled: true, hostname: "headlamp.example.org" } },
    });
  });
});

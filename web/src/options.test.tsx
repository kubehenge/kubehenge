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
});

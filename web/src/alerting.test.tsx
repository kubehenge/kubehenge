import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { alertingOf, alertingPurposes, alertingVariable, newAlerting, pendingAlerting } from "./Alerting";
import { App } from "./App";
import { applyStaged, fakeServer } from "./fake-server";

const everything = {
  ...newAlerting(),
  email: {
    on: true,
    smarthost: "mail.example.org:587",
    from: "alerts@example.org",
    to: "ops@example.org, oncall@example.org",
    username: "alerts",
    requireTLS: false,
  },
  webhook: true,
  slack: { on: true, channel: "#ops" },
  teams: true,
  remoteWrite: { on: true, url: "https://mimir.example.org/push", auth: "basic" as const, username: "tenant" },
  external: {
    on: true,
    urls: "https://am.example.org\nhttps://am2.example.org:9093",
    auth: "bearer" as const,
    username: "",
  },
};

const variable = {
  alertReceivers: [
    {
      type: "email",
      smarthost: "mail.example.org:587",
      from: "alerts@example.org",
      to: ["ops@example.org", "oncall@example.org"],
      username: "alerts",
      requireTLS: false,
    },
    { type: "webhook" },
    { type: "slack", channel: "#ops" },
    { type: "teams" },
  ],
  remoteWrite: { url: "https://mimir.example.org/push", auth: "basic", username: "tenant" },
  externalAlertmanagers: { urls: ["https://am.example.org", "https://am2.example.org:9093"], auth: "bearer" },
};

describe("monitoring's alerting settings", () => {
  it("make the monitoring variable and are read back from it", () => {
    expect(alertingVariable(everything)).toStrictEqual(variable);
    expect(alertingOf({ enabled: true, ...variable })).toStrictEqual(everything);
    expect(alertingVariable(newAlerting())).toStrictEqual({});
    expect(alertingOf({ enabled: true })).toStrictEqual(newAlerting());
  });

  it("send a user name only with basic sign-in", () => {
    const a = {
      ...newAlerting(),
      remoteWrite: { on: true, url: "https://m.example", auth: "bearer" as const, username: "x" },
    };
    expect(alertingVariable(a)).toStrictEqual({ remoteWrite: { url: "https://m.example", auth: "bearer" } });
  });

  it("need a credential for each setting that has a secret, and only then", () => {
    expect(alertingPurposes(newAlerting())).toStrictEqual([]);
    expect(alertingPurposes(everything)).toStrictEqual([
      "alerts-email",
      "alerts-webhook",
      "alerts-slack",
      "alerts-teams",
      "remote-write",
      "external-alertmanager",
    ]);
    const unsigned = { ...newAlerting(), email: { ...everything.email, username: "" } };
    expect(alertingPurposes(unsigned)).toStrictEqual([]);
  });

  it("store the credentials typed, and none that is incomplete", () => {
    const pending = pendingAlerting(everything, {
      "alerts-webhook": { url: "https://hook.example" },
      "alerts-slack": { url: "" },
      "remote-write": { secret: "s" },
    });
    expect(pending.map((p) => p.purpose)).toStrictEqual(["alerts-webhook", "remote-write"]);
    expect(pending[0]?.values).toStrictEqual({ url: "https://hook.example" });
  });
});

const cluster = {
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
  variables: { monitoring: { enabled: true, hostname: "grafana.example.org", tlsSecret: "grafana-tls" }, oidc: [] },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

describe("a cluster's monitoring options", () => {
  it("add a receiver and keep the rest of the variable, with the secret stored apart", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "GET /api/clusters/clusters/web/credentials/alerts-slack": {
        body: { purpose: "alerts-slack", set: false, canChange: true },
      },
      "PUT /api/clusters/clusters/web/credentials/alerts-slack": {
        body: { purpose: "alerts-slack", set: true, canChange: true },
      },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit monitoring" }));
    const dialog = screen.getByRole("dialog", { name: "Edit monitoring" });
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.click(within(dialog).getByLabelText("Slack"));
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeEnabled();
    await user.type(within(dialog).getByLabelText("Channel"), "#ops");
    // The secret goes to the credential store at once, never into the variable.
    await user.click(await within(dialog).findByRole("button", { name: "Set…" }));
    await user.type(within(dialog).getByLabelText("Webhook URL"), "https://hooks.slack.example/T0/B0/secret");
    await user.click(within(dialog).getByRole("button", { name: "Store" }));
    await waitFor(() =>
      expect(
        calls.find((c) => c.key === "PUT /api/clusters/clusters/web/credentials/alerts-slack")?.body,
      ).toStrictEqual({
        values: { url: "https://hooks.slack.example/T0/B0/secret" },
      }),
    );
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    const body = calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body;
    expect(body).toStrictEqual({
      variables: {
        monitoring: {
          enabled: true,
          hostname: "grafana.example.org",
          tlsSecret: "grafana-tls",
          alertReceivers: [{ type: "slack", channel: "#ops" }],
        },
      },
    });
    expect(JSON.stringify(body)).not.toContain("secret");
  });

  it("say what stays when monitoring goes", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit monitoring" }));
    const dialog = screen.getByRole("dialog", { name: "Edit monitoring" });
    expect(within(dialog).queryByRole("status")).not.toBeInTheDocument();
    await user.click(within(dialog).getByLabelText("Install monitoring"));
    expect(within(dialog).getByRole("status")).toHaveTextContent("The volumes of Prometheus and Alertmanager stay.");
  });
});

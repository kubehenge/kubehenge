import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { ApplianceUpgrades } from "./ApplianceUpgrades";
import { fakeServer, type Answer, type Route } from "./fake-server";
import { StatePassphrase } from "./ui";

const saved = {
  nodes: ["pve1"],
  imageDatastore: "local",
  vmDatastore: "local-lvm",
  bridge: "vmbr0",
  nodeAddresses: ["192.0.2.11/24"],
  apiAddress: "192.0.2.10",
  gateway: "192.0.2.1",
  dnsServers: ["192.0.2.1"],
  workloadDatastores: ["local-lvm"],
  workloadBridges: ["vmbr0"],
  bootstrapDatastore: "local",
};

const behind = {
  applied: "0.1.0",
  carried: "0.2.0",
  behind: true,
  changes: [
    { component: "talos", from: "v1.14.4", to: "v1.15.0" },
    { component: "cluster_api", from: "v1.14.3", to: "v1.15.0" },
  ],
};

const administrationPage = (setup: unknown): Record<string, Route> => ({
  "GET /api/session": { body: { user: "root@pam", csrf: "c1" } },
  "GET /api/setup": { body: setup },
  "GET /api/appliance/upgrades": {
    body: { version: "0.2.0", available: [], settings: { policy: "off", hour: 3, timeZone: "UTC" } },
  },
  "GET /api/templates": { body: { templates: [] } },
  "GET /api/runs": { body: [] },
  "GET /api/clusters": { body: [] },
  "GET /api/fleet/health": { body: [] },
});

describe("the management cluster behind the appliance", () => {
  it("says so and upgrades it with the passphrase, saying what changes", async () => {
    window.history.replaceState(null, "", "/administration");
    const calls = fakeServer({
      ...administrationPage({
        appliance: true,
        ready: true,
        allowed: true,
        passphrase: true,
        settings: saved,
        upgrade: behind,
      }),
      "POST /api/setup": {
        status: 202,
        body: { user: "root@pam", kind: "apply", started: new Date().toISOString(), state: "running", log: [] },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(
      screen.findByText("Management cluster upgrade: 0.1.0 to 0.2.0", { exact: false }),
    ).resolves.toBeInTheDocument();
    expect(screen.getByText("Kubehenge 0.1.0")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Upgrade…" }));
    const dialog = within(screen.getByRole("dialog", { name: "Upgrade management cluster" }));
    expect(dialog.getByRole("row", { name: "Component 0.1.0 0.2.0" })).toBeInTheDocument();
    const talos = dialog.getByRole("row", { name: "Talos v1.14.4 v1.15.0" });
    expect(talos).toBeInTheDocument();
    expect(dialog.getByRole("row", { name: "Cluster API v1.14.3 v1.15.0" })).toBeInTheDocument();
    expect(dialog.getByRole("button", { name: "Upgrade" })).toBeDisabled();

    await user.type(dialog.getByLabelText("State passphrase"), "correct horse battery staple");
    await user.click(dialog.getByRole("button", { name: "Upgrade" }));
    await waitFor(() =>
      expect(calls.find((c) => c.key === "POST /api/setup")?.body).toStrictEqual({
        settings: saved,
        passphrase: "correct horse battery staple",
      }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("says nothing while it runs what this appliance carries", async () => {
    window.history.replaceState(null, "", "/administration");
    fakeServer(
      administrationPage({
        appliance: true,
        ready: true,
        allowed: true,
        settings: saved,
        upgrade: { applied: "0.2.0", carried: "0.2.0", behind: false },
      }),
    );
    render(<App />);
    await expect(screen.findByRole("heading", { name: "Management cluster" })).resolves.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Upgrade…" })).not.toBeInTheDocument();
  });

  it("tells of a failed automatic upgrade, and that it is tried again", async () => {
    window.history.replaceState(null, "", "/administration");
    const job = {
      user: "kubehenge-upgrader@pve",
      automatic: true,
      kind: "apply",
      started: "2026-10-14T04:00:00Z",
      finished: "2026-10-14T04:12:00Z",
      state: "failed",
      error: "tofu apply: exit status 1",
      log: ["Error: no such node"],
    };
    fakeServer(
      administrationPage({ appliance: true, ready: true, allowed: true, settings: saved, upgrade: behind, job }),
    );
    render(<App />);
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/Automatic upgrade of .* failed\. It retries at the next upgrade hour\./u);
    expect(alert).toHaveTextContent("tofu apply: exit status 1");
    // The notice at the top of the page says so too.
    expect(
      screen.getByText(/Management cluster upgrade: 0\.1\.0 to 0\.2\.0\. Automatic upgrade failed\./u),
    ).toBeInTheDocument();
    // Upgrading by hand stays open meanwhile.
    expect(screen.getByRole("button", { name: "Upgrade…" })).toBeInTheDocument();
  });
});

const upgrades = (management: unknown, policy = "off"): Answer => ({
  body: { version: "0.2.0", available: [], settings: { policy, hour: 3, timeZone: "UTC" }, management },
});

describe("automatic upgrades of the management cluster", () => {
  it("switch on with the state passphrase inline, and off again", async () => {
    const browser = new Intl.DateTimeFormat().resolvedOptions().timeZone;
    const off = { possible: true, on: false, user: "kubehenge-upgrader@pve" };
    const on = { ...off, on: true, by: "root@pam", since: "2026-10-22T09:00:00Z", imageStorage: "local" };
    const routes: Record<string, Route> = {
      "GET /api/appliance/upgrades": upgrades(off),
      "PUT /api/appliance/upgrades/management": () => {
        routes["GET /api/appliance/upgrades"] = upgrades(on);
        return { body: on };
      },
      "DELETE /api/appliance/upgrades/management": () => {
        routes["GET /api/appliance/upgrades"] = upgrades(off);
        return { body: off };
      },
      "PUT /api/appliance/upgrades/settings": (settings) => ({ body: settings }),
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    render(<ApplianceUpgrades />);

    const select = await screen.findByLabelText("Management cluster");
    expect(select).toHaveValue("off");
    expect(screen.queryByLabelText("Hour")).not.toBeInTheDocument();

    // Taken back before switching on, nothing is kept.
    await user.selectOptions(select, "on");
    expect(screen.getByRole("button", { name: "Switch on" })).toBeDisabled();
    await user.selectOptions(select, "off");
    expect(screen.queryByLabelText("State passphrase")).not.toBeInTheDocument();
    expect(calls.map((c) => c.key)).not.toContain("PUT /api/appliance/upgrades/management");

    await user.selectOptions(select, "on");
    await user.type(screen.getByLabelText("State passphrase"), "correct horse battery staple");
    await user.click(screen.getByRole("button", { name: "Switch on" }));
    await waitFor(() =>
      expect(calls.find((c) => c.key === "PUT /api/appliance/upgrades/management")?.body).toStrictEqual({
        passphrase: "correct horse battery staple",
      }),
    );
    // Nothing else was timed, so the hour starts out in the browser's time zone.
    await waitFor(() =>
      expect(calls.find((c) => c.key === "PUT /api/appliance/upgrades/settings")?.body).toStrictEqual({
        policy: "off",
        hour: 3,
        timeZone: browser,
      }),
    );
    await waitFor(() => expect(screen.getByLabelText("Management cluster")).toHaveValue("on"));
    expect(screen.getByText(/Switched on .* by root@pam/u)).toBeInTheDocument();
    expect(screen.getByText("Also writes VM templates on storage local")).toBeInTheDocument();
    expect(screen.queryByLabelText("State passphrase")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Hour")).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText("Management cluster"), "off");
    await waitFor(() => expect(calls.map((c) => c.key)).toContain("DELETE /api/appliance/upgrades/management"));
    await waitFor(() => expect(screen.getByLabelText("Management cluster")).toHaveValue("off"));
  });

  it("switch on at once where the state needs no passphrase", async () => {
    const calls = fakeServer({
      "GET /api/appliance/upgrades": upgrades({ possible: true, on: false, user: "kubehenge-upgrader@pve" }, "patch"),
      "PUT /api/appliance/upgrades/management": { body: { possible: true, on: true, user: "kubehenge-upgrader@pve" } },
    });
    const user = userEvent.setup({ delay: null });
    render(
      <StatePassphrase value={false}>
        <ApplianceUpgrades />
      </StatePassphrase>,
    );
    await user.selectOptions(await screen.findByLabelText("Management cluster"), "on");
    await waitFor(() =>
      expect(calls.find((c) => c.key === "PUT /api/appliance/upgrades/management")?.body).toStrictEqual({
        passphrase: "",
      }),
    );
    expect(screen.queryByLabelText("State passphrase")).not.toBeInTheDocument();
  });

  it("show a wrong passphrase", async () => {
    fakeServer({
      "GET /api/appliance/upgrades": upgrades({ possible: true, on: false, user: "kubehenge-upgrader@pve" }, "patch"),
      "PUT /api/appliance/upgrades/management": {
        status: 403,
        body: { error: "the passphrase does not open the management cluster's state" },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<ApplianceUpgrades />);
    await user.selectOptions(await screen.findByLabelText("Management cluster"), "on");
    await user.type(screen.getByLabelText("State passphrase"), "the wrong passphrase entirely");
    await user.click(screen.getByRole("button", { name: "Switch on" }));
    await expect(
      screen.findByText("the passphrase does not open the management cluster's state", { exact: false }),
    ).resolves.toBeInTheDocument();
  });

  it("say why an apply could not switch them on", async () => {
    const failed =
      "Off: kubehenge-upgrader@pve could not be made (forbidden). Apply again as a Proxmox VE administrator.";
    fakeServer({
      "GET /api/appliance/upgrades": upgrades({ possible: true, on: false, user: "kubehenge-upgrader@pve", failed }),
    });
    render(<ApplianceUpgrades />);
    await expect(screen.findByText(failed)).resolves.toBeInTheDocument();
  });

  it("renew in one click when a restore left their key behind", async () => {
    const calls = fakeServer({
      "GET /api/appliance/upgrades": upgrades(
        {
          possible: true,
          on: true,
          by: "root@pam",
          since: "2026-10-22T09:00:00Z",
          user: "kubehenge-upgrader@pve",
          keyLost: true,
        },
        "patch",
      ),
      "PUT /api/appliance/upgrades/management": { body: { possible: true, on: true, user: "kubehenge-upgrader@pve" } },
    });
    const user = userEvent.setup({ delay: null });
    render(
      <StatePassphrase value={false}>
        <ApplianceUpgrades />
      </StatePassphrase>,
    );
    await expect(screen.findByText("Its key is gone, as after a restore. Renew it.")).resolves.toBeInTheDocument();
    expect(screen.getByLabelText("Management cluster")).toHaveValue("on");
    await user.click(screen.getByRole("button", { name: "Renew" }));
    await waitFor(() => expect(calls.map((c) => c.key)).toContain("PUT /api/appliance/upgrades/management"));
  });

  it("are not offered before there is a management cluster", async () => {
    fakeServer({
      "GET /api/appliance/upgrades": upgrades({ possible: false, on: false, user: "kubehenge-upgrader@pve" }),
    });
    render(<ApplianceUpgrades />);
    await expect(screen.findByLabelText("Automatic upgrades")).resolves.toBeInTheDocument();
    expect(screen.queryByLabelText("Management cluster")).not.toBeInTheDocument();
  });
});

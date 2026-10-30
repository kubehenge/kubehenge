import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { describe as describePolicy, windowProblems } from "./ClusterUpgrades";
import { applyStaged, fakeServer, type Answer } from "./fake-server";

const cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5+rke2r1",
  generation: 1,
  created: new Date().toISOString(),
  autoUpgraded: "v1.36.2+rke2r1 to v1.36.5+rke2r1 at 2026-10-13T03:00:00Z",
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: {},
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

const built = (iso: string): string => new Date(iso).toLocaleString();

describe("cluster upgrades", () => {
  it("describes a policy", () => {
    expect(describePolicy({})).toBe("Off");
    expect(describePolicy({ mode: "patch" })).toBe("Patch releases, daily 02:00 for 4 h UTC");
    expect(
      describePolicy({
        mode: "minor",
        window: { weekdays: ["Sat"], monthDays: [1], start: "22:00", durationMinutes: 90, timeZone: "Europe/Berlin" },
      }),
    ).toBe("Patch and minor releases, Sat and day 1 22:00 for 1.5 h Europe/Berlin");
    expect(describePolicy({ mode: "off" }, true)).toBe("Node images, daily 02:00 for 4 h UTC");
    expect(describePolicy({ mode: "patch" }, true)).toBe("Patch releases and node images, daily 02:00 for 4 h UTC");
    expect(describePolicy({ mode: "patch" }, true, "talos")).toBe(
      "Kubernetes patch releases and Talos patch releases, daily 02:00 for 4 h UTC",
    );
  });

  it("switches the node image refresh off, and keeps a window while it is on", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const sent = (): unknown => calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body;
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await expect(screen.findByText("Node images, daily 02:00 for 4 h UTC")).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Edit automatic upgrades" }));
    const dialog = screen.getByRole("dialog", { name: "Edit automatic upgrades" });
    // Upgrades are off, but the nodes still move in the window.
    expect(within(dialog).getByLabelText("From")).toHaveValue("02:00");
    await user.click(within(dialog).getByLabelText("Update node images"));
    expect(within(dialog).queryByLabelText("From")).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await applyStaged(user);
    await waitFor(() => expect(sent()).toStrictEqual({ variables: { upgrades: { mode: "off", nodeImages: false } } }));
  });

  it("moves Talos nodes to new Talos patch releases in the window", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const talos = { ...cluster, class: "kubehenge-talos", distribution: "talos", version: "v1.36.5" };
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: talos },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await expect(screen.findByText("Talos patch releases, daily 02:00 for 4 h UTC")).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Edit automatic upgrades" }));
    const dialog = screen.getByRole("dialog", { name: "Edit automatic upgrades" });
    expect(within(dialog).queryByLabelText("Update node images")).not.toBeInTheDocument();
    expect(within(dialog).getByLabelText("Apply Talos patch releases")).toBeChecked();
    expect(within(dialog).getByText(/Upgrades the nodes in place/u)).toBeInTheDocument();
    expect(within(dialog).getByLabelText("From")).toHaveValue("02:00");
  });

  it("upgrades a Talos cluster's nodes one minor version at a time, as the server allows", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const talos = {
      ...cluster,
      class: "kubehenge-talos",
      distribution: "talos",
      version: "v1.36.5",
      variables: { talosVersion: "v1.14.2" },
      talos: { version: "v1.14.2" },
    };
    let answer: Answer = { status: 422, body: { error: "talos v1.15.3 does not run kubernetes v1.36.5" } };
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: talos },
      "GET /api/talos/releases": {
        body: {
          templates: ["v1.14.2"],
          releases: ["v1.16.0", "v1.15.3", "v1.14.4"],
          default: "v1.14.4",
          kubernetes: { "v1.15.3": ["1.35", "1.36"] },
        },
      },
      "PATCH /api/clusters/clusters/web": () => answer,
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const row = within(await screen.findByRole("row", { name: /^Talos release v1\.14\.2/u }));
    await user.click(row.getByRole("button", { name: "Upgrade Talos" }));
    const dialog = within(screen.getByRole("dialog", { name: "Upgrade Talos on web" }));
    expect(dialog.getByText("Reboots the nodes one at a time, control planes first.")).toBeInTheDocument();
    // A newer patch of its minor version or the next minor's newest; not two minors on.
    await waitFor(() =>
      expect(
        within(dialog.getByLabelText("Talos release"))
          .getAllByRole("option")
          .map((o) => o.textContent),
      ).toStrictEqual(["v1.14.2 (running)", "v1.15.3", "v1.14.4"]),
    );
    await user.selectOptions(dialog.getByLabelText("Talos release"), "v1.15.3");
    await user.click(dialog.getByRole("button", { name: "Upgrade" }));
    await expect(dialog.findByRole("alert")).resolves.toHaveTextContent(
      "Talos v1.15.3 does not run kubernetes v1.36.5.",
    );
    answer = { status: 200, body: talos };
    await user.click(dialog.getByRole("button", { name: "Upgrade" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    // The refused release was planned, never sent.
    expect(calls.filter((c) => c.key === "PATCH /api/clusters/clusters/web").map((c) => c.body)).toStrictEqual([
      { variables: { talosVersion: "v1.15.3" } },
    ]);
  });

  it("shows which template builds the nodes run and when they move on", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": {
        body: {
          ...cluster,
          nodeImage: {
            refreshes: true,
            since: "2026-10-09T00:00:00Z",
            builds: [
              {
                template: "kubehenge-rke2-almalinux-10",
                build: "build-202610080000",
                built: "2026-10-08T00:00:00Z",
              },
            ],
            last: "build-202610010000 to build-202610080000 at 2026-10-09T02:10:00Z",
            next: { build: "build-202610150000", due: "2026-10-17T02:00:00Z" },
          },
        },
      },
    });
    render(<App />);
    await expect(
      screen.findByText(`kubehenge-rke2-almalinux-10, build ${built("2026-10-08T00:00:00Z")}`),
    ).resolves.toBeInTheDocument();
    expect(
      screen.getByText(`${built("2026-10-17T02:00:00Z")}, to build ${built("2026-10-15T00:00:00Z")}`),
    ).toBeInTheDocument();
    expect(
      screen.getByText((text) => text.startsWith(`${built("2026-10-09T02:10:00Z")}, from build`)),
    ).toBeInTheDocument();
  });

  it("sets a policy with a window", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const sent = (): unknown => calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body;
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await expect(screen.findByText(/v1.36.2\+rke2r1 to v1.36.5\+rke2r1, /u)).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("link", { name: "Options" }));
    expect(document.title).toBe("web: Options · Kubehenge");
    await user.click(screen.getByRole("button", { name: "Edit automatic upgrades" }));
    const dialog = screen.getByRole("dialog", { name: "Edit automatic upgrades" });
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.selectOptions(within(dialog).getByLabelText("Upgrade automatically"), "minor");
    await user.click(within(dialog).getByLabelText("Sat"));
    await user.click(within(dialog).getByLabelText("Mon"));
    await user.selectOptions(within(dialog).getByLabelText("Time zone"), "Europe/Berlin");
    await user.type(within(dialog).getByLabelText("Days of the month"), "32");
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    expect(within(dialog).getByLabelText("Days of the month")).toHaveAttribute("aria-invalid", "true");
    await user.clear(within(dialog).getByLabelText("Days of the month"));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await applyStaged(user);
    await waitFor(() =>
      expect(sent()).toStrictEqual({
        // Weekdays in calendar order, whatever order they were ticked in.
        variables: {
          upgrades: {
            mode: "minor",
            window: { start: "02:00", durationMinutes: 240, timeZone: "Europe/Berlin", weekdays: ["Mon", "Sat"] },
          },
        },
      }),
    );
  });

  it("checks a window as the class will", () => {
    expect(windowProblems("1, 15", "4")).toStrictEqual({});
    expect(windowProblems("0", "4").monthDays).toBeDefined();
    expect(windowProblems("x", "4").monthDays).toBeDefined();
    expect(windowProblems("", "0").hours).toBeDefined();
    expect(windowProblems("", "").hours).toBeDefined();
    expect(windowProblems("", "1.25").hours).toBeDefined();
    expect(windowProblems("", "1.5")).toStrictEqual({});
  });
});

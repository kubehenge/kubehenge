import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { bundleName } from "./ClusterDetail";
import { fakeServer, reads, type Route } from "./fake-server";
import { Machines } from "./Machines";
import type { Cluster } from "./types";

const web: Cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5+rke2r1",
  generation: 1,
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioning",
  available: false,
  conditions: [],
  controlPlane: { desired: 1, ready: 0, available: 0, upToDate: 0 },
  workers: { desired: 0, ready: 0, available: 0, upToDate: 0 },
  workerPools: [],
  variables: {},
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [
    {
      name: "web-cp-abc",
      role: "control-plane",
      version: "v1.36.5+rke2r1",
      phase: "Provisioned",
      ready: false,
      waiting: "Node not joined",
      created: new Date().toISOString(),
    },
  ],
};

const bootLogPath = "/api/clusters/clusters/web/machines/web-cp-abc/boot-log";

describe("diagnosing a cluster", () => {
  it("names a bundle as the command line does", () => {
    expect(bundleName("clusters", "web", new Date("2026-10-28T12:34:56.789Z"))).toBe(
      "clusters-web-20261028T123456Z.tar.gz",
    );
  });

  it("downloads the support bundle for those who may take the kubeconfig", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: web },
      "GET /api/clusters/clusters/web/support-bundle": { text: "bundle" },
    });
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockReturnValue(undefined);
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Support bundle" }));
    await waitFor(() => expect(click).toHaveBeenCalledWith());
    expect(reads(calls, "GET /api/clusters/clusters/web/support-bundle")).toBe(1);
  });

  it("offers neither the bundle nor boot logs to those who may not take the kubeconfig", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/machines");
    fakeServer({
      "GET /api/session": { body: { user: "viewer@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: { ...web, allowed: { change: false, kubeconfig: false } } },
    });
    render(<App />);
    await expect(screen.findByRole("row", { name: /web-cp-abc/u })).resolves.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Support bundle" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Boot log of web-cp-abc" })).not.toBeInTheDocument();
  });

  it("asks for a machine's boot log and shows it once the controller has read it", async () => {
    const routes: Record<string, Route> = {
      [`POST ${bootLogPath}`]: { status: 202, body: { asked: true } },
      [`GET ${bootLogPath}`]: { body: { asked: true } },
    };
    const calls = fakeServer(routes);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ delay: null, advanceTimers: vi.advanceTimersByTime });
    render(<Machines cluster={web} />);
    await user.click(screen.getByRole("button", { name: "Boot log of web-cp-abc" }));
    const dialog = within(await screen.findByRole("dialog", { name: "Boot log of web-cp-abc" }));
    await expect(dialog.findByText("Reading…")).resolves.toBeInTheDocument();
    expect(reads(calls, `POST ${bootLogPath}`)).toBe(1);
    expect(dialog.getByRole("button", { name: "Read again" })).toBeDisabled();

    routes[`GET ${bootLogPath}`] = {
      body: {
        asked: false,
        log: {
          read: "2026-10-28T12:00:00Z",
          sections: [
            { name: "cloud-init-output.log", lines: ["Cloud-init finished"] },
            { name: "journal of rke2-server", error: "the unit is not there" },
          ],
        },
      },
    };
    await act(() => vi.advanceTimersByTimeAsync(5000));
    await expect(dialog.findByLabelText("Log of cloud-init-output.log")).resolves.toHaveTextContent(
      "Cloud-init finished",
    );
    expect(within(dialog.getByRole("region", { name: "journal of rke2-server" })).getByRole("alert")).toHaveTextContent(
      "The unit is not there.",
    );
    expect(dialog.queryByText("Reading…")).not.toBeInTheDocument();
    await user.click(dialog.getByRole("button", { name: "Read again" }));
    await waitFor(() => expect(reads(calls, `POST ${bootLogPath}`)).toBe(2));
  });
});

import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState, type JSX } from "react";
import { describe, expect, it, vi } from "vitest";

import { PoliciesStep } from "./AddOnsStep";
import { App } from "./App";
import { newForm, request, summary, withBackupChoices, withSiteZone, type Form } from "./create-form";
import { fakeServer, type Route } from "./fake-server";
import type { ClusterClass } from "./types";

const noProblem = vi.fn<() => string | undefined>();
const object = { type: "object" };
const classOf = (zone: string): ClusterClass => ({
  name: "kubehenge-kubeadm",
  distribution: "kubeadm",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+$` },
  annotations: {
    "kubehenge.dev/time-zone": zone,
    "kubehenge.dev/backup-targets": JSON.stringify([{ name: "store", bucket: "backups" }]),
  },
  variables: ["etcdSnapshots", "backups", "upgrades"].map((name) => ({ name, required: false, schema: object })),
});

describe("the site time zone in the wizard", () => {
  it("is where new upgrade windows start, in the hint, the request and the summary", () => {
    const cls = classOf("Asia/Tokyo");
    const form: Form = withBackupChoices(
      { ...withSiteZone(newForm(), "Asia/Tokyo"), name: "web", className: cls.name },
      cls,
      true,
    );
    expect(JSON.stringify(request(form, cls).variables["upgrades"])).toContain('"timeZone":"Asia/Tokyo"');
    expect(Object.fromEntries(summary(form, cls))["Automatic upgrades"]).toContain("Asia/Tokyo");

    function Step(): JSX.Element {
      const [entered, setEntered] = useState<Form>(form);
      return (
        <PoliciesStep
          form={entered}
          set={(k, v) => setEntered((f) => ({ ...f, [k]: v }))}
          shown={noProblem}
          cls={cls}
          canTrust
          namespace={undefined}
        />
      );
    }
    render(<Step />);
    expect(screen.getByText("Upgrades run daily 02:00 to 06:00, Asia/Tokyo time")).toBeInTheDocument();
  });

  it("is where a new cluster's schedules start once the classes say so", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    const user = userEvent.setup({ delay: null });
    const routes: Record<string, Route> = {
      "GET /api/session": { body: { user: "root@pam", csrf: "c1", canCreate: true, canTrust: true } },
      "GET /api/setup": { body: { appliance: true, ready: true, allowed: true, backup: true } },
      "GET /api/clusters": { body: [] },
      "GET /api/fleet/health": { body: [] },
      "GET /api/runs": { body: [] },
      "GET /api/classes": {
        body: [
          {
            ...classOf("Asia/Tokyo"),
            variables: [
              { name: "controlPlaneEndpoint", required: true, schema: { type: "string" } },
              { name: "network", required: true, schema: object },
              { name: "storage", required: true, schema: { type: "string" } },
              ...classOf("").variables,
            ],
          },
        ],
      },
      "GET /api/versions/kubeadm": { body: { versions: ["v1.36.1"], stable: "v1.36.1" } },
      "GET /api/template-builds": { body: { allowed: true, requested: [], building: [] } },
      "GET /api/proxmox/ha-rules": { body: { readable: true, rules: [] } },
      "GET /api/proxmox/datacenter-firewall": { body: { state: "off" } },
      "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true }] },
      "GET /api/proxmox/storages": {
        body: [
          {
            storage: "local-lvm",
            node: "pve1",
            shared: false,
            content: ["images", "iso", "import"],
            avail: 1 << 30,
            total: 2 << 30,
            usable: true,
          },
        ],
      },
      "GET /api/proxmox/nodes/pve1/bridges": {
        body: [
          { bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true, cidr: "192.0.2.5/24", gateway: "192.0.2.1" },
        ],
      },
      "GET /api/proxmox/nodes/pve1/dns": { body: { servers: ["192.0.2.53"], search: "" } },
      "POST /api/proxmox/nodes/pve1/bridges/vmbr0/free": {
        body: { api: "192.0.2.30", nodes: "192.0.2.31-192.0.2.33" },
      },
    };
    fakeServer(routes);
    render(<App />);
    await user.type(await screen.findByLabelText("Name"), "lab");
    await waitFor(() => expect(screen.getByRole("option", { name: /kubeadm/iu })).toBeInTheDocument());
    await user.selectOptions(screen.getByLabelText("Distribution"), "kubehenge-kubeadm");
    await user.type(screen.getByLabelText("Kubernetes version"), "v1.36.1");
    await user.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() => expect(screen.getByLabelText("Bridge")).toHaveValue("vmbr0"));
    await user.clear(screen.getByLabelText("Gateway"));
    await user.type(screen.getByLabelText("Gateway"), "192.0.2.1");
    await user.clear(screen.getByLabelText("DNS servers"));
    await user.type(screen.getByLabelText("DNS servers"), "192.0.2.53");
    await user.type(screen.getByLabelText("API address"), "192.0.2.30");
    await user.type(screen.getByLabelText("Node addresses"), "192.0.2.31-192.0.2.39");
    await user.click(screen.getByRole("button", { name: "Next" }));
    await user.click(screen.getByRole("button", { name: "Next" }));
    await expect(screen.findByLabelText("Take etcd snapshots")).resolves.toBeInTheDocument();
    for (const zone of screen.getAllByLabelText("Time zone")) {
      expect(zone).toHaveValue("Asia/Tokyo");
    }
    await user.click(screen.getByRole("button", { name: "Next" }));
    await expect(screen.findByText("Upgrades run daily 02:00 to 06:00, Asia/Tokyo time")).resolves.toBeInTheDocument();
  });
});

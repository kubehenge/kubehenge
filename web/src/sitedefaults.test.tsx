import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState, type JSX } from "react";
import { afterEach, describe, expect, it } from "vitest";

import { PoliciesStep } from "./AddOnsStep";
import { App } from "./App";
import { newForm, request, summary, withBackupChoices, type Form } from "./create-form";
import { fakeServer, type Route } from "./fake-server";
import { AdvancedFields, formOf, type SetupForm } from "./Setup";
import { fromCluster } from "./start-from";
import type { Cluster, ClusterClass } from "./types";
import { setSiteZone } from "./ui";

afterEach(() => {
  setSiteZone("");
});

const pem = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----";

function Fields(): JSX.Element {
  const [form, setForm] = useState<SetupForm>(formOf());
  return (
    <AdvancedFields
      form={form}
      set={(k, v) => setForm((f) => ({ ...f, [k]: v }))}
      shown={() => undefined}
      fixed={false}
      open
      warnings={[]}
      onOpen={() => undefined}
    />
  );
}

describe("the site settings in Setup", () => {
  it("say, before saving, how many clusters take the site CA and when", async () => {
    const user = userEvent.setup({ delay: null });
    const calls = fakeServer({
      "POST /api/setup/site-ca-roll": { body: { inherits: 3, now: 2, windows: 1, waiting: 0 } },
    });
    render(<Fields />);
    await user.click(screen.getByLabelText("Site CA"));
    await user.paste(pem);
    await expect(
      screen.findByText("Replaces the nodes of 3 clusters. 2 start now, 1 waits for its window."),
    ).resolves.toBeInTheDocument();
    expect(calls.find((c) => c.key === "POST /api/setup/site-ca-roll")?.body).toStrictEqual({ siteCA: `${pem}\n` });
  });

  it("asks nothing and says nothing for text that is no certificate", async () => {
    const user = userEvent.setup({ delay: null });
    const calls = fakeServer({});
    render(<Fields />);
    await user.click(screen.getByLabelText("Site CA"));
    await user.paste("junk");
    expect(calls).toHaveLength(0);
    expect(screen.queryByText(/Replaces the nodes/u)).not.toBeInTheDocument();
  });

  it("take a time zone", async () => {
    const user = userEvent.setup({ delay: null });
    render(<Fields />);
    await user.type(screen.getByLabelText("Time zone"), "Europe/Zurich");
    expect(screen.getByLabelText("Time zone")).toHaveValue("Europe/Zurich");
  });
});

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
    setSiteZone("Asia/Tokyo");
    const cls = classOf("Asia/Tokyo");
    const form: Form = withBackupChoices({ ...newForm(), name: "web", className: cls.name }, cls, true);
    expect(JSON.stringify(request(form, cls).variables["upgrades"])).toContain('"timeZone":"Asia/Tokyo"');
    expect(Object.fromEntries(summary(form, cls))["Automatic upgrades"]).toContain("Asia/Tokyo");

    function Step(): JSX.Element {
      const [entered, setEntered] = useState<Form>(form);
      return (
        <PoliciesStep
          form={entered}
          set={(k, v) => setEntered((f) => ({ ...f, [k]: v }))}
          shown={() => undefined}
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
    expect(await screen.findByLabelText("Take etcd snapshots")).toBeInTheDocument();
    for (const zone of screen.getAllByLabelText("Time zone")) {
      expect(zone).toHaveValue("Asia/Tokyo");
    }
    await user.click(screen.getByRole("button", { name: "Next" }));
    await expect(screen.findByText("Upgrades run daily 02:00 to 06:00, Asia/Tokyo time")).resolves.toBeInTheDocument();
  });
});

describe("a cluster copied into the wizard", () => {
  it("keeps the load balancer it had, whatever the address range says", () => {
    const source = {
      namespace: "default",
      name: "web",
      class: "kubehenge-rke2",
      distribution: "rke2",
      version: "v1.36.5",
      generation: 1,
      created: "2026-10-01T00:00:00Z",
      deleting: false,
      phase: "Provisioned",
      available: true,
      conditions: [],
      controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
      workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
      workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
      variables: {},
      pool: "/pool/kubehenge/default.web",
      allowed: { change: true, kubeconfig: true },
    } satisfies Cluster;
    expect(fromCluster(source, newForm()).loadBalancerPicked).toBeTruthy();
    expect(newForm().loadBalancerPicked).toBeFalsy();
  });
});

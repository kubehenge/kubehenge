import { render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import {
  backupsChosen,
  backupsWait,
  createRoom,
  fieldProblems,
  gatewayProblems,
  memoryNodeWarning,
  newForm,
  request,
  summary,
  withBackupChoices,
  type BridgeNetwork,
} from "./create-form";
import { fakeServer } from "./fake-server";
import type { ClusterClass, PveNode, Storage } from "./types";

const object = { type: "object" };

const classOf = (annotations: Record<string, string>, names: string[]): ClusterClass => ({
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  annotations,
  variables: names.map((name) => ({ name, required: false, schema: object })),
});

const target = { "kubehenge.dev/backup-targets": JSON.stringify([{ name: "store", bucket: "backups" }]) };
const withTarget = classOf(target, ["backups", "etcdSnapshots", "upgrades"]);
const withoutTarget = classOf({}, ["backups", "etcdSnapshots", "upgrades"]);

describe("the create wizard's defaults", () => {
  it("backs up with Velero where a target is offered, and stays off with the reason where none is", () => {
    const on = withBackupChoices({ ...newForm(), name: "web" }, withTarget, true);
    expect(backupsChosen(on, withTarget)).toBe(true);
    expect(fieldProblems("Add-ons", on, withTarget)).toStrictEqual({});
    expect(backupsWait(on, withTarget)).toBe(true);
    const off = withBackupChoices({ ...newForm(), name: "web" }, withoutTarget, true);
    expect(backupsChosen(off, withoutTarget)).toBe(false);
    expect(fieldProblems("Add-ons", off, withoutTarget)).toStrictEqual({});
    // What is sent and confirmed matches the box: no backups variable, which admission would refuse.
    expect(request(off, withoutTarget).variables).not.toHaveProperty("backups");
    expect(Object.fromEntries(summary(off, withoutTarget))["Backups"]).toBe("Off");
    expect(request(on, withTarget).variables).toHaveProperty("backups");
  });

  it("upgrades Kubernetes patch releases", () => {
    expect(newForm().upgradeMode).toBe("patch");
  });

  it("shows the firewall's state on Confirm", () => {
    const rows = Object.fromEntries(summary({ ...newForm(), name: "web" }, withTarget));
    expect(rows["Firewall"]).toBe("Off, no security groups");
    const grouped = Object.fromEntries(summary({ ...newForm(), name: "web", securityGroups: "web, db" }, withTarget));
    expect(grouped["Firewall"]).toBe("Security groups web, db");
  });
});

describe("a gateway outside the bridge's network", () => {
  const net: BridgeNetwork = { cidr: "10.20.0.11/16", gateway: "10.20.0.1", dns: [], search: "" };

  it("is marked on the gateway, and a gateway inside it is not", () => {
    const wrong = { ...newForm(), gateway: "192.168.5.1" };
    expect(gatewayProblems("Network", wrong, net)).toStrictEqual({
      gateway: "Not in the bridge's network 10.20.0.0/16",
    });
    expect(gatewayProblems("Network", { ...newForm(), gateway: "10.20.0.1" }, net)).toStrictEqual({});
    expect(gatewayProblems("Network", wrong, undefined)).toStrictEqual({});
    expect(gatewayProblems("Machines", wrong, net)).toStrictEqual({});
  });
});

describe("disks beyond the free space", () => {
  const storage = (type: string): Storage =>
    ({
      storage: "s",
      node: "pve1",
      total: 2 ** 40,
      avail: 610 * 2 ** 30,
      usable: true,
      shared: false,
      type,
    }) as Storage;
  const nodes = [{ node: "pve1", online: true, maxMemory: 2 ** 40, memory: 0 }] as PveNode[];
  const big = {
    ...newForm(),
    storage: "s",
    workerReplicas: 1,
    workerMachine: { cores: 2, memoryMiB: 2048, diskGiB: 9000 },
  };

  it("warn with the figures on thin storage, and not on thick storage, which refuses them", () => {
    expect(createRoom(big, nodes, [storage("lvmthin")]).diskWarning).toMatch(/need 8\.8 TiB, 610\.0 GiB free/u);
    expect(createRoom(big, nodes, [storage("lvm")]).diskWarning).toBeUndefined();
  });
});

describe("a machine no node could hold", () => {
  const gib = 2 ** 30;
  const nodes = [{ node: "pve1", online: true, maxMemory: 8 * gib, memory: 6 * gib }] as PveNode[];

  it("is refused on its field and does not also say the cluster waits", () => {
    const huge = { ...newForm(), workerMachine: { cores: 2, memoryMiB: 900_000, diskGiB: 20 } };
    expect(memoryNodeWarning(huge, nodes)).toBeUndefined();
    const fits = { ...newForm(), workerMachine: { cores: 2, memoryMiB: 4096, diskGiB: 20 } };
    expect(memoryNodeWarning(fits, nodes)).toMatch(/The cluster waits for room/u);
  });
});

describe("the control-plane default", () => {
  async function open(count: number, offline = 0): Promise<void> {
    window.history.replaceState(null, "", "/clusters/new");
    fakeServer({
      "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1" } }),
      "GET /api/classes": () => ({ body: [withTarget] }),
      "GET /api/versions/rke2": () => ({ body: { versions: ["v1.36.5+rke2r1"], stable: "v1.36.5+rke2r1" } }),
      "GET /api/clusters": () => ({ body: [] }),
      "GET /api/proxmox/storages": () => ({ body: [] }),
      "GET /api/proxmox/nodes": () => ({
        body: Array.from({ length: count }, (_, i) => ({ node: `pve${i + 1}`, online: i < count - offline })),
      }),
    });
    render(<App />);
    await screen.findByLabelText("Control-plane nodes");
  }

  it("is three where the Proxmox VE cluster has three nodes", async () => {
    await open(3);
    await waitFor(() => expect(screen.getByLabelText("Control-plane nodes")).toHaveValue("3"));
  });

  it("is one, marked as without high availability, where it has fewer", async () => {
    await open(2);
    await waitFor(() => expect(screen.getByLabelText("Control-plane nodes")).toHaveValue("1"));
    expect(screen.getByText("One has no high availability")).toBeInTheDocument();
  });

  it("is one where only two of three nodes are online", async () => {
    await open(3, 1);
    await waitFor(() => expect(screen.getByLabelText("Control-plane nodes")).toHaveValue("1"));
  });
});

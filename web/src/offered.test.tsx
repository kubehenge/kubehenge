import { describe, expect, it } from "vitest";

import { upgradeTargets } from "./ClusterDetail";
import { offeredBridges, offeredStorages, versionLabel } from "./create-form";

const storage = (name: string, node: string, usable = true) => ({
  storage: name,
  node,
  shared: false,
  content: ["images"],
  avail: 0,
  total: 0,
  usable,
});

describe("the wizard's choices", () => {
  it("offers the class's storages the user may use, once each", () => {
    const all = [
      storage("local-lvm", "pve1"),
      storage("local-lvm", "pve2"),
      storage("ceph", "pve1"),
      storage("fast", "pve1", false),
    ];
    expect(offeredStorages(all, undefined).map((s) => s.storage)).toStrictEqual(["local-lvm", "ceph"]);
    expect(offeredStorages(all, ["ceph", "fast"]).map((s) => s.storage)).toStrictEqual(["ceph"]);
  });

  it("offers upgrades within the minor version and to the next", () => {
    const offered = ["v1.38.0+rke2r1", "v1.37.1+rke2r1", "v1.36.5+rke2r2", "v1.35.9+rke2r1"];
    expect(upgradeTargets("v1.36.5+rke2r1", offered)).toStrictEqual(["v1.37.1+rke2r1", "v1.36.5+rke2r2"]);
    expect(upgradeTargets("v1.36.5+rke2r2", offered)).toStrictEqual(["v1.37.1+rke2r1"]);
    expect(upgradeTargets("v1.36.2", ["v1.37.1", "v1.36.5"])).toStrictEqual(["v1.37.1", "v1.36.5"]);
    expect(upgradeTargets("nonsense", offered)).toStrictEqual([]);
  });

  it("marks the stable release and those newer", () => {
    expect(versionLabel("v1.36.5+rke2r1", "v1.36.5+rke2r1")).toBe("v1.36.5+rke2r1 (stable)");
    expect(versionLabel("v1.37.1+rke2r1", "v1.36.5+rke2r1")).toBe("v1.37.1+rke2r1 (newer than stable)");
    expect(versionLabel("v1.35.9+rke2r2", "v1.36.5+rke2r1")).toBe("v1.35.9+rke2r2");
    expect(versionLabel("v1.37.1", undefined)).toBe("v1.37.1");
  });

  it("offers the class's bridges, also those no node lists to the user", () => {
    const byNode = {
      pve1: [
        { bridge: "vmbr0", node: "pve1", vlanAware: false, usable: false },
        { bridge: "vmbr9", node: "pve1", vlanAware: false, usable: true },
      ],
      pve2: [{ bridge: "vmbr0", node: "pve2", vlanAware: false, usable: true }],
    };
    expect(offeredBridges(byNode, undefined).map((b) => [b.bridge, b.usable])).toStrictEqual([
      ["vmbr0", true],
      ["vmbr9", true],
    ]);
    expect(offeredBridges(byNode, ["vmbr0", "vmbr1"]).map((b) => [b.bridge, b.usable])).toStrictEqual([
      ["vmbr0", true],
      ["vmbr1", true],
    ]);
  });
});

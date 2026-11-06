import { describe, expect, it } from "vitest";

import { bridgeNodeProblems, newForm } from "./create-form";
import type { Bridge } from "./types";

const bridge = (name: string, node: string): Bridge => ({ bridge: name, node, vlanAware: false, usable: true });

const nodes = {
  bridgesByNode: {
    pve1: [bridge("vmbr0", "pve1")],
    pve2: [bridge("vmbr0", "pve2")],
    pve3: [bridge("vmbr0", "pve3"), bridge("vmbr1", "pve3")],
  },
  online: ["pve1", "pve2", "pve3"],
};

describe("the network step and the nodes that have the bridge", () => {
  it("accepts a bridge that some node the cluster may use has", () => {
    const form = { ...newForm(), bridge: "vmbr1" };
    expect(bridgeNodeProblems("Network", form, nodes)).toStrictEqual({});
  });

  it("refuses a bridge only on nodes the cluster is kept off", () => {
    const form = {
      ...newForm(),
      bridge: "vmbr1",
      place: { placement: "nodes" as const, haRule: "", nodes: ["pve1", "pve2"] },
    };
    expect(bridgeNodeProblems("Network", form, nodes)).toStrictEqual({
      bridge: "vmbr1 is only on pve3. Allow one of those nodes or choose another bridge.",
    });
    expect(bridgeNodeProblems("Machines", form, nodes)).toStrictEqual({});
  });

  it("leaves a node whose bridges are not listed, and a name no node lists, alone", () => {
    const kept = {
      ...newForm(),
      bridge: "vmbr1",
      place: { placement: "nodes" as const, haRule: "", nodes: ["pve1", "pve4"] },
    };
    expect(bridgeNodeProblems("Network", kept, nodes)).toStrictEqual({});
    const vnet = {
      ...newForm(),
      bridge: "zone1vnet",
      place: { placement: "nodes" as const, haRule: "", nodes: ["pve1"] },
    };
    expect(bridgeNodeProblems("Network", vnet, nodes)).toStrictEqual({});
  });
});

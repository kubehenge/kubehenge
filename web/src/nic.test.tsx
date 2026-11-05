import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { JSX } from "react";
import { describe, expect, it, vi } from "vitest";

import { bridgeMtuProblems, fieldProblems, newForm, request, summary, type FieldKey, type Form } from "./create-form";
import { NetworkStep } from "./CreateSteps";
import { fromCluster } from "./start-from";
import type { Bridge, Cluster, ClusterClass } from "./types";

const noProblems = new Map<FieldKey, string>();

const withNic: ClusterClass = {
  name: "kubehenge-k3s",
  distribution: "k3s",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+k3s\d+$` },
  variables: [
    {
      name: "network",
      required: true,
      schema: {
        type: "object",
        properties: { mtu: { type: "integer", minimum: 1280, maximum: 65_520 }, queues: { type: "integer" } },
      },
    },
  ],
};
const withoutNic: ClusterClass = {
  ...withNic,
  variables: [{ name: "network", required: true, schema: { type: "object" } }],
};

const bridge = (mtu: number | undefined): Bridge => ({
  bridge: "vmbr0",
  node: "pve1",
  vlanAware: false,
  usable: true,
  ...(mtu === undefined ? {} : { mtu }),
});

const filled: Form = { ...newForm(), bridge: "vmbr0", gateway: "10.0.0.1", dnsServers: "10.0.0.1" };

const problems = (mtu: string, queues = ""): Partial<Record<FieldKey, string>> =>
  fieldProblems("Network", { ...filled, mtu, queues }, withNic);

describe("a cluster's interface options", () => {
  it("asks for the MTU and queues where the class has them", async () => {
    const set = vi.fn<(k: keyof Form, v: unknown) => void>();
    const step = (cls: ClusterClass): JSX.Element => (
      <NetworkStep form={newForm()} set={set} shown={(k) => noProblems.get(k)} cls={cls} bridgeOptions={[]} />
    );
    const user = userEvent.setup({ delay: null });
    const { rerender } = render(step(withNic));

    await user.type(screen.getByLabelText("MTU"), "9");
    expect(set).toHaveBeenLastCalledWith("mtu", "9");
    await user.type(screen.getByLabelText("Queues"), "4");
    expect(set).toHaveBeenLastCalledWith("queues", "4");

    rerender(step(withoutNic));
    expect(screen.queryByLabelText("MTU")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Queues")).not.toBeInTheDocument();
  });

  it("sends them only when entered", () => {
    expect(request(filled, withNic).variables["network"]).not.toHaveProperty("mtu");
    expect(request(filled, withNic).variables["network"]).not.toHaveProperty("queues");
    expect(request({ ...filled, mtu: "1450", queues: "2" }, withNic).variables["network"]).toMatchObject({
      mtu: 1450,
      queues: 2,
    });
    expect(Object.fromEntries(summary({ ...filled, mtu: "1450", queues: "2" }, withNic))).toMatchObject({
      MTU: "1450",
      Queues: "2",
    });
    const plain = summary(filled, withNic).map(([label]) => label);
    expect(plain).not.toContain("MTU");
    expect(plain).not.toContain("Queues");
  });

  it("refuses an MTU or queue count out of bounds", () => {
    for (const ok of ["", "1280", "1450", "9000", "65520"]) {
      expect(problems(ok)).not.toHaveProperty("mtu");
    }
    for (const bad of ["1279", "576", "1", "0", "65521", "1450.5", "jumbo", "-9000"]) {
      expect(problems(bad)["mtu"]).toBe("A whole number from 1280 to 65520");
    }
    for (const ok of ["1", "64"]) {
      expect(problems("", ok)).not.toHaveProperty("queues");
    }
    for (const bad of ["0", "65", "1.5", "many"]) {
      expect(problems("", bad)["queues"]).toBe("A whole number from 1 to 64");
    }
  });

  it("refuses an MTU above the bridge's where a node lists it", () => {
    const form = { ...filled, mtu: "9000" };
    expect(bridgeMtuProblems("Network", form, [bridge(1500)])).toStrictEqual({
      mtu: "The bridge has MTU 1500. Use 1500 or less",
    });
    expect(bridgeMtuProblems("Network", form, [bridge(9000)])).toStrictEqual({});
    expect(bridgeMtuProblems("Network", { ...form, mtu: "1450" }, [bridge(1500)])).toStrictEqual({});
    expect(bridgeMtuProblems("Network", form, [bridge(undefined)])).toStrictEqual({});
    expect(bridgeMtuProblems("Network", { ...form, bridge: "vmbr9" }, [bridge(1500)])).toStrictEqual({});
    expect(bridgeMtuProblems("Network", { ...form, mtu: "" }, [bridge(1500)])).toStrictEqual({});
    expect(bridgeMtuProblems("Machines", form, [bridge(1500)])).toStrictEqual({});
  });

  it("starts a new cluster with the options of the one it starts from", () => {
    const existing: Cluster = {
      namespace: "default",
      name: "web",
      class: "kubehenge-k3s",
      distribution: "k3s",
      version: "v1.36.5+k3s1",
      generation: 1,
      created: "2026-10-01T00:00:00Z",
      deleting: false,
      phase: "Provisioned",
      available: true,
      conditions: [],
      controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
      workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
      workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
      variables: { network: { bridge: "vmbr0", mtu: 1450, queues: 2 } },
      pool: "/pool/kubehenge/default.web",
      allowed: { change: true, kubeconfig: true },
    };
    expect(fromCluster(existing, newForm())).toMatchObject({ mtu: "1450", queues: "2" });
  });
});

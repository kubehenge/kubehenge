import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState, type JSX } from "react";
import { describe, expect, it } from "vitest";

import {
  fieldProblems,
  networkFieldsOf,
  newForm,
  request,
  summary,
  withBridgeNetwork,
  withFreeAddresses,
  type FieldKey,
  type Form,
} from "./create-form";
import { NetworkStep } from "./CreateSteps";
import { loadBalancerOf, loadBalancerVariable } from "./LoadBalancer";
import {
  addressCount,
  addressCountIn,
  addressIn,
  choiceOf,
  formatAddress,
  isIP,
  isNetwork,
  networkOf,
  overlapping,
  parseAddress,
  parseCidr,
  rangeProblem,
  reservedAddresses,
  spanOf,
  type Address,
} from "./network";
import { fromCluster, startKeys } from "./start-from";
import type { Cluster, ClusterClass } from "./types";

// address reads an address a test names, which is always one.
function address(text: string): Address {
  const a = parseAddress(text);
  if (a === undefined) {
    throw new Error(`${text} is no address`);
  }
  return a;
}

// reserved are the addresses no node of a gateway's network takes, as text.
function reserved(gateway: string, prefix: number): string[] {
  const g = address(gateway);
  return reservedAddresses(g, prefix).map((at) => formatAddress({ family: g.family, value: at }));
}

const noProblems = new Map<FieldKey, string>();

// Text that is neither an IPv4 nor an IPv6 address.
const noAddresses = [
  "",
  "2001:db8",
  "2001:db8:::1",
  "1::2::3",
  "12345::",
  "g::1",
  "1:2:3:4:5:6:7:8:9",
  "[::1]",
  "::1%eth0",
  "1.2.3",
  "256.0.0.1",
  "1:2:3:4:5:6:7::8",
];

const classWith = (network: Record<string, { type: string }>): ClusterClass => ({
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: { pattern: ".*" },
  annotations: { "kubehenge.dev/default-pods": "10.244.0.0/16", "kubehenge.dev/default-services": "10.96.0.0/12" },
  variables: [
    { name: "controlPlaneEndpoint", required: true, schema: { type: "string" } },
    { name: "network", required: true, schema: { type: "object", properties: network } },
    { name: "storage", required: true, schema: { type: "string" } },
    { name: "loadBalancer", required: false, schema: { type: "object" } },
  ],
});

// The class as the chart defines it, with the families, and an older one without.
const cls = classWith({ ipFamilies: { type: "array" } });
const legacy = classWith({});

const v4: Form = {
  ...newForm(),
  name: "web",
  className: "kubehenge-rke2",
  bridge: "vmbr0",
  endpoint: "10.0.0.20",
  addresses: "10.0.0.21-10.0.0.29",
  prefix: 24,
  gateway: "10.0.0.1",
  dnsServers: "10.0.0.53",
};

const v6: Form = {
  ...v4,
  families: "ipv6",
  endpoint: "2001:db8::20",
  addresses: "",
  gateway: "",
  addresses6: "2001:db8::21-2001:db8::29",
  prefix6: 64,
  gateway6: "2001:db8::1",
  dnsServers: "2001:db8::53",
};

const dual4: Form = {
  ...v4,
  families: "ipv4-ipv6",
  addresses6: "2001:db8::21-2001:db8::29",
  gateway6: "2001:db8::1",
  dnsServers: "10.0.0.53, 2001:db8::53",
};

const dual6: Form = { ...dual4, families: "ipv6-ipv4", endpoint: "2001:db8::20" };

const problemsOf = (form: Form, c: ClusterClass = cls): ReturnType<typeof fieldProblems> =>
  fieldProblems("Network", form, c);

const v4Network = { addresses: ["10.0.0.21-10.0.0.29"], prefix: 24, gateway: "10.0.0.1" };
const v6Network = { addresses: ["2001:db8::21-2001:db8::29"], prefix: 64, gateway: "2001:db8::1" };

describe("the submitted network variable", () => {
  it("is the same for IPv4 as before, naming the family only where the class knows it", () => {
    expect(request(v4, legacy).variables["network"]).toStrictEqual({
      bridge: "vmbr0",
      ...v4Network,
      dnsServers: ["10.0.0.53"],
    });
    expect(request(v4, cls).variables["network"]).toStrictEqual({
      bridge: "vmbr0",
      ipFamilies: ["IPv4"],
      ...v4Network,
      dnsServers: ["10.0.0.53"],
    });
  });

  it("holds the IPv6 network alone for IPv6 only", () => {
    const req = request(v6, cls);
    expect(req.variables["network"]).toStrictEqual({
      bridge: "vmbr0",
      ipFamilies: ["IPv6"],
      ipv6: v6Network,
      dnsServers: ["2001:db8::53"],
    });
    expect(req.variables["controlPlaneEndpoint"]).toBe("2001:db8::20");
  });

  it("holds both networks for dual stack, with the primary family first", () => {
    expect(request(dual4, cls).variables["network"]).toStrictEqual({
      bridge: "vmbr0",
      ipFamilies: ["IPv4", "IPv6"],
      ...v4Network,
      ipv6: v6Network,
      dnsServers: ["10.0.0.53", "2001:db8::53"],
    });
    const req = request(dual6, cls);
    expect(req.variables["network"]).toMatchObject({ ipFamilies: ["IPv6", "IPv4"] });
    expect(req.variables["controlPlaneEndpoint"]).toBe("2001:db8::20");
  });

  it("sends one pod and service range per family", () => {
    const req = request(
      { ...dual4, pods: "10.244.0.0/16, fd5e:4b1c:9a20::/56", services: "10.96.0.0/12 fd5e:4b1c:9a21::/112" },
      cls,
    );
    expect(req.pods).toStrictEqual(["10.244.0.0/16", "fd5e:4b1c:9a20::/56"]);
    expect(req.services).toStrictEqual(["10.96.0.0/12", "fd5e:4b1c:9a21::/112"]);
  });

  it("sends the router id of a BGP load balancer when one is given", () => {
    const bgp = loadBalancerVariable({
      implementation: "metallb",
      addresses: ["2001:db8:1::/120"],
      mode: "bgp",
      asn: "64512",
      peers: "2001:db8::1 64512",
      routerID: "192.0.2.1",
      authenticated: false,
    });
    expect(bgp).toStrictEqual({
      enabled: true,
      implementation: "metallb",
      addresses: ["2001:db8:1::/120"],
      mode: "bgp",
      bgp: { asn: 64_512, peers: [{ address: "2001:db8::1", asn: 64_512 }], routerID: "192.0.2.1" },
    });
    expect(loadBalancerOf(bgp).routerID).toBe("192.0.2.1");
    const without = loadBalancerVariable({ ...loadBalancerOf(bgp), routerID: "" });
    expect(without?.["bgp"]).not.toHaveProperty("routerID");
  });
});

describe("the network step's problems", () => {
  it("accepts a valid form of each family choice", () => {
    for (const form of [v4, v6, dual4, dual6]) {
      expect(problemsOf(form)).toStrictEqual({});
    }
  });

  it("refuses an IPv6 address in the IPv4 fields", () => {
    expect(
      problemsOf({ ...v4, addresses: "2001:db8::21", gateway: "2001:db8::1", dnsServers: "2001:db8::53" }),
    ).toMatchObject({
      addresses: "2001:db8::21 is an IPv6 address. Enter IPv4 addresses here",
      gateway: "Enter an IPv4 address",
      dnsServers: "DNS servers must be IPv4 addresses",
    });
  });

  it("refuses an IPv4 address in the IPv6 fields", () => {
    expect(problemsOf({ ...v6, addresses6: "10.0.0.21", gateway6: "10.0.0.1", dnsServers: "10.0.0.53" })).toMatchObject(
      {
        addresses6: "10.0.0.21 is an IPv4 address. Enter IPv6 addresses here",
        gateway6: "Enter an IPv6 address",
        dnsServers: "DNS servers must be IPv6 addresses",
      },
    );
  });

  it("asks a dual-stack cluster for a DNS server of each family", () => {
    expect(problemsOf({ ...dual4, dnsServers: "10.0.0.53" }).dnsServers).toBe("Add an IPv6 DNS server too");
    expect(problemsOf({ ...dual4, dnsServers: "2001:db8::53" }).dnsServers).toBe("Add an IPv4 DNS server too");
    expect(problemsOf({ ...dual4, dnsServers: "10.0.0.53, nonsense" }).dnsServers).toBe(
      "DNS servers must be IP addresses",
    );
  });

  it("holds the endpoint to the primary family", () => {
    expect(problemsOf({ ...dual6, endpoint: "10.0.0.20" }).endpoint).toBe("Use an IPv6 address, the primary family");
    expect(problemsOf({ ...dual4, endpoint: "2001:db8::20" }).endpoint).toBe("Use an IPv4 address, the primary family");
    expect(problemsOf({ ...v6, endpoint: "10.0.0.20" }).endpoint).toBe("Use an IPv6 address, the primary family");
    expect(problemsOf({ ...v6, endpoint: "nonsense" }).endpoint).toBe("Enter an IPv6 address");
    expect(problemsOf({ ...v6, endpoint: "2001:db8::25" }).endpoint).toBe("Use an address outside the node addresses");
    expect(problemsOf({ ...v6, endpoint: "2001:db9::20" }).endpoint).toBe("Use an address on the gateway's network");
  });

  it("refuses an IPv6 prefix past 128 and an IPv4 prefix past 32", () => {
    expect(problemsOf({ ...v6, prefix6: 129 }).prefix6).toBe("A whole number from 1 to 128");
    expect(problemsOf({ ...v6, prefix6: 0 }).prefix6).toBe("A whole number from 1 to 128");
    expect(problemsOf({ ...v6, prefix6: 128 }).prefix6).toBeUndefined();
    expect(problemsOf({ ...v4, prefix: 33 }).prefix).toBe("A whole number from 1 to 32");
  });

  it("leaves the fields of a family the cluster lacks alone", () => {
    expect(problemsOf({ ...v6, prefix: 99, gateway: "nonsense" })).toStrictEqual({});
    expect(problemsOf({ ...v4, prefix6: 999, gateway6: "nonsense", addresses6: "nonsense" })).toStrictEqual({});
  });

  it("wants one pod and service range per family, in order", () => {
    const wrong = "Enter one range per family, in order: IPv4, IPv6";
    expect(problemsOf({ ...dual4, pods: "10.244.0.0/16" }).pods).toBe(wrong);
    expect(problemsOf({ ...dual4, services: "fd5e:4b1c:9a21::/112, 10.96.0.0/12" }).services).toBe(wrong);
    expect(problemsOf({ ...dual4, pods: "10.244.0.0/16, fd5e:4b1c:9a20::/56" })).toStrictEqual({});
    expect(problemsOf({ ...dual6, pods: "fd5e:4b1c:9a20::/56, 10.244.0.0/16" })).toStrictEqual({});
    expect(problemsOf({ ...v6, pods: "10.244.0.0/16" }).pods).toBe("Enter one range per family, in order: IPv6");
    expect(problemsOf({ ...v6, pods: "fd5e:4b1c:9a20::1/56" }).pods).toBe(
      "Enter a network like fd5e:4b1c:9a20::/56, without host bits",
    );
  });
});

describe("the BGP router id", () => {
  const bgp = {
    loadBalancer: "metallb" as const,
    loadBalancerMode: "bgp" as const,
    loadBalancerAddresses: "2001:db8:1::/120",
    loadBalancerAsn: "64512",
    loadBalancerPeers: "2001:db8::1 64512",
  };

  it("is required on IPv6 only", () => {
    expect(problemsOf({ ...v6, ...bgp }).loadBalancerRouterID).toBe("IPv6 only needs a router ID like 192.0.2.1");
    expect(problemsOf({ ...v6, ...bgp, loadBalancerRouterID: "192.0.2.1" })).toStrictEqual({});
  });

  it("is optional with IPv4", () => {
    const peers = { loadBalancerAddresses: "198.51.100.0/28", loadBalancerPeers: "10.0.0.1 64512" };
    expect(problemsOf({ ...v4, ...bgp, ...peers })).toStrictEqual({});
    expect(problemsOf({ ...dual6, ...bgp, loadBalancerPeers: "2001:db8::1 64512" })).toStrictEqual({});
  });

  it.each(["64512", "2001:db8::1", "1.2.3", "256.1.1.1", "0.0.0.0", "a.b.c.d"])("refuses %s as a router id", (id) => {
    expect(problemsOf({ ...v6, ...bgp, loadBalancerRouterID: id }).loadBalancerRouterID).toBe(
      "Enter a dotted ID like 192.0.2.1, not 0.0.0.0",
    );
  });

  it("takes routers and addresses of the cluster's families only", () => {
    expect(
      problemsOf({ ...v6, ...bgp, loadBalancerRouterID: "192.0.2.1", loadBalancerPeers: "10.0.0.1 64512" }),
    ).toStrictEqual({
      loadBalancerPeers: "10.0.0.1 is not of the cluster's address families",
    });
    expect(
      problemsOf({ ...v4, ...bgp, loadBalancerPeers: "10.0.0.1 64512", loadBalancerAddresses: "2001:db8:1::/120" })
        .loadBalancerAddresses,
    ).toBe("2001:db8:1::/120 is not of the cluster's address families");
    expect(
      problemsOf({
        ...dual4,
        ...bgp,
        loadBalancerPeers: "10.0.0.1 64512\n2001:db8::1 64512",
        loadBalancerAddresses: "198.51.100.0/28, 2001:db8:1::/120",
      }),
    ).toStrictEqual({});
  });
});

describe("addresses of either family", () => {
  it.each([
    ["2001:0db8:0000:0000:0000:0000:0000:0001", "2001:db8::1"],
    ["::", "::"],
    ["::1", "::1"],
    ["fe80::", "fe80::"],
    ["2001:db8:0:1:0:0:0:1", "2001:db8:0:1::1"],
    ["2001:DB8::A", "2001:db8::a"],
    ["1:2:3:4:5:6:7:8", "1:2:3:4:5:6:7:8"],
    ["2001:db8:0:0:1:0:0:1", "2001:db8::1:0:0:1"],
    ["1:0:0:2:0:0:0:3", "1:0:0:2::3"],
    ["::ffff:192.0.2.1", "::ffff:c000:201"],
  ])("writes %s as %s", (text, short) => {
    expect(formatAddress(address(text))).toBe(short);
  });

  it.each(noAddresses)("refuses %j as an address", (text) => {
    expect(parseAddress(text)).toBeUndefined();
  });

  it("takes none of them for a server's address", () => {
    expect(noAddresses.filter((text) => isIP(text))).toStrictEqual([]);
  });

  it("refuses a prefix length past the family's bits", () => {
    expect(parseCidr("2001:db8::/129")).toBeUndefined();
    expect(parseCidr("10.0.0.0/33")).toBeUndefined();
    expect(parseCidr("2001:db8::")).toBeUndefined();
  });

  it("finds a network and an address in it", () => {
    expect(networkOf("2001:db8:0:1::5/64")).toBe("2001:db8:0:1::/64");
    expect(networkOf("10.0.0.5/24")).toBe("10.0.0.0/24");
    expect(addressIn("2001:db8::/64", 20)).toBe("2001:db8::14");
    expect(addressIn("10.0.0.0/24", 20)).toBe("10.0.0.20");
    expect(addressIn("2001:db8::/126", 3)).toBeUndefined();
    expect(["2001:db8::/32", "2001:db8::1/32"].filter((n) => isNetwork(n))).toStrictEqual(["2001:db8::/32"]);
    expect(rangeProblem("fd5e:4b1c:9a20::/56")).toBeUndefined();
  });

  it("counts IPv6 node addresses within the gateway's network", () => {
    expect(addressCount(["2001:db8::21-2001:db8::29", "2001:db8::40"], "2001:db8::1", 64)).toStrictEqual({ count: 10 });
    expect(addressCount(["2001:db8::/124"], "2001:db8::1", 64)).toStrictEqual({ count: 15 });
    expect(addressCount(["2001:db9::5"], "2001:db8::1", 64)).toStrictEqual({ count: 0, bad: "2001:db9::5" });
    expect(addressCount(["10.0.0.5"], "2001:db8::1", 64).bad).toBe("10.0.0.5");
    expect(addressCount(["10.0.0.5-2001:db8::5"], "10.0.0.1", 24).bad).toBe("10.0.0.5-2001:db8::5");
    expect(
      addressCountIn(
        ["10.0.0.5", "2001:db8::5"],
        [
          { gateway: "10.0.0.1", prefix: 24 },
          { gateway: "2001:db8::1", prefix: 64 },
        ],
      ),
    ).toStrictEqual({ count: 2 });
  });

  it("keeps a network's own address, and in IPv4 its broadcast address, from the nodes", () => {
    expect(reserved("10.0.0.1", 24)).toStrictEqual(["10.0.0.0", "10.0.0.255"]);
    expect(reserved("2001:db8::1", 64)).toStrictEqual(["2001:db8::"]);
    expect(reserved("10.0.0.1", 31)).toStrictEqual([]);
    expect(reserved("2001:db8::1", 127)).toStrictEqual([]);
  });

  it("never lets an IPv4 and an IPv6 entry overlap", () => {
    expect(spanOf("::ffff:10.0.0.1")?.family).toBe("IPv6");
    expect(overlapping(["10.0.0.0/24"], ["2001:db8::/32"])).toBeUndefined();
    expect(overlapping(["2001:db8::5"], ["2001:db8::/64"])).toBe("2001:db8::5");
    expect(overlapping(["2001:db8::5-2001:db8::9"], ["2001:db8::9-2001:db8::a"])).toBe("2001:db8::5-2001:db8::9");
  });
});

describe("the form around the families", () => {
  it("fills the API address from the primary family's free address", () => {
    const free = {
      endpoint: "10.0.0.20",
      addresses: "10.0.0.21-10.0.0.22",
      endpoint6: "2001:db8::14",
      addresses6: "2001:db8::15-2001:db8::16",
      answering: [],
    };
    const empty = { ...newForm(), families: "ipv6-ipv4" as const };
    expect(withFreeAddresses(empty, free, () => false)).toMatchObject({
      endpoint: "2001:db8::14",
      addresses: "10.0.0.21-10.0.0.22",
      addresses6: "2001:db8::15-2001:db8::16",
    });
    expect(withFreeAddresses({ ...empty, families: "ipv4" }, free, () => false).endpoint).toBe("10.0.0.20");
  });

  it("names the families in the summary only when they are not IPv4 alone", () => {
    const labels = (form: Form): string[] => summary(form, cls).map(([label]) => label);
    expect(labels(v4)).not.toContain("Address families");
    expect(labels(v4)).not.toContain("IPv6 node addresses");
    expect(Object.fromEntries(summary(dual6, cls))).toMatchObject({
      "Address families": "IPv6 and IPv4",
      "IPv6 node addresses": "2001:db8::21-2001:db8::29",
      "IPv6 prefix length and gateway": "/64, 2001:db8::1",
      "Node addresses": "10.0.0.21-10.0.0.29",
    });
    expect(labels(v6)).not.toContain("Node addresses");
  });

  it("fills the IPv6 gateway from the bridge where the cluster started from has none", () => {
    const base = newForm();
    const started: Form = {
      ...base,
      from: "clusters/web",
      ...networkFieldsOf({ bridge: "vmbr0", prefix: 24, gateway: "10.0.0.1" }, base),
    };
    const bridge = { gateway: "10.0.0.254", gateway6: "2001:db8::1", cidr6: "2001:db8::5/64", dns: [], search: "" };
    expect(withBridgeNetwork(started, bridge, (k) => startKeys.has(k))).toMatchObject({
      gateway: "10.0.0.1",
      gateway6: "2001:db8::1",
    });
    const dual = { ...started, gateway6: "2001:db9::1" };
    expect(withBridgeNetwork(dual, bridge, (k) => startKeys.has(k)).gateway6).toBe("2001:db9::1");
  });

  it("starts a new cluster from the families and IPv6 network of another", () => {
    // fromCluster reads only the fields given here.
    // oxlint-disable-next-line typescript/no-unsafe-type-assertion
    const from = {
      namespace: "clusters",
      name: "web",
      class: "kubehenge-rke2",
      created: "2026-01-01T00:00:00Z",
      controlPlane: { desired: 1 },
      workers: { desired: 1 },
      workerPools: [],
      variables: {
        network: {
          bridge: "vmbr0",
          ipFamilies: ["IPv6", "IPv4"],
          prefix: 24,
          gateway: "10.0.0.1",
          ipv6: { prefix: 56, gateway: "2001:db8::1" },
        },
        loadBalancer: {
          enabled: true,
          implementation: "metallb",
          addresses: ["x"],
          mode: "bgp",
          bgp: { asn: 1, routerID: "192.0.2.1", peers: [] },
        },
      },
    } as unknown as Cluster;
    expect(fromCluster(from, { name: "", namespace: "", className: "", version: "" })).toMatchObject({
      families: "ipv6-ipv4",
      prefix6: 56,
      gateway6: "2001:db8::1",
      loadBalancerRouterID: "192.0.2.1",
      addresses6: "",
    });
    expect(choiceOf(["IPv4", "IPv6"])).toBe("ipv4-ipv6");
    expect(choiceOf(["IPv6"])).toBe("ipv6");
    expect(choiceOf(undefined)).toBe("ipv4");
  });
});

function Step({ initial, c = cls }: { initial?: Partial<Form>; c?: ClusterClass }): JSX.Element {
  const [form, setForm] = useState<Form>({ ...newForm(), ...initial });
  return (
    <NetworkStep
      form={form}
      set={(k, v) => {
        setForm((f) => ({ ...f, [k]: v }));
      }}
      shown={(k) => noProblems.get(k)}
      cls={c}
      bridgeOptions={[]}
    />
  );
}

const ipv4Fields = ["Node addresses", "Prefix length", "Gateway"];
const ipv6Fields = ["IPv6 node addresses", "IPv6 prefix length", "IPv6 gateway"];

describe("the family switch", () => {
  it("shows the fields of the families chosen", async () => {
    const user = userEvent.setup();
    render(<Step />);
    const shownFields = (): string[] =>
      [...ipv4Fields, ...ipv6Fields].filter((label) => screen.queryByLabelText(label) !== null);
    const families = screen.getByLabelText("Address families");
    expect(shownFields()).toStrictEqual(ipv4Fields);
    expect(
      within(families)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toStrictEqual(["IPv4", "IPv4 and IPv6", "IPv6 and IPv4", "IPv6"]);
    await user.selectOptions(families, "IPv4 and IPv6");
    expect(shownFields()).toStrictEqual([...ipv4Fields, ...ipv6Fields]);
    await user.selectOptions(families, "IPv6");
    expect(shownFields()).toStrictEqual(ipv6Fields);
    await user.selectOptions(families, "IPv6 and IPv4");
    expect(shownFields()).toStrictEqual([...ipv4Fields, ...ipv6Fields]);
    // The primary family's fields come first.
    const v6First = screen.getByLabelText("IPv6 node addresses");
    expect(v6First.compareDocumentPosition(screen.getByLabelText("Node addresses"))).toBe(
      Node.DOCUMENT_POSITION_FOLLOWING,
    );
    await user.selectOptions(families, "IPv4");
    expect(shownFields()).toStrictEqual(ipv4Fields);
  });

  it("asks for a pod and service range per family when there are two", async () => {
    const user = userEvent.setup();
    render(<Step />);
    expect(screen.getByLabelText("Pod range")).toHaveAttribute("placeholder", "10.244.0.0/16");
    await user.selectOptions(screen.getByLabelText("Address families"), "IPv6 and IPv4");
    expect(screen.getByLabelText("Pod ranges")).toHaveAttribute("placeholder", "fd5e:4b1c:9a20::/56, 10.244.0.0/16");
    expect(screen.getByLabelText("Service ranges")).toHaveAttribute(
      "placeholder",
      "fd5e:4b1c:9a21::/112, 10.96.0.0/12",
    );
  });

  it("is left out by a class that does not know the families", () => {
    render(<Step c={legacy} />);
    expect(screen.queryByLabelText("Address families")).toBeNull();
    expect(screen.getByLabelText("Gateway")).toBeInTheDocument();
  });

  it("shows the router id in BGP mode, required on IPv6 only", async () => {
    const user = userEvent.setup();
    render(<Step initial={{ loadBalancer: "metallb", loadBalancerMode: "bgp" }} />);
    expect(screen.getByLabelText("Router ID")).toHaveAccessibleDescription("Empty: each node's own IPv4 address");
    await user.selectOptions(screen.getByLabelText("Address families"), "IPv6");
    expect(screen.getByLabelText("Router ID")).toHaveAccessibleDescription("Required, like 192.0.2.1");
  });

  it("has no router id outside BGP mode", () => {
    render(<Step initial={{ loadBalancer: "metallb", loadBalancerMode: "l2" }} />);
    expect(screen.queryByLabelText("Router ID")).toBeNull();
  });
});

import { describe, expect, it } from "vitest";

import {
  keyProblem,
  labelProblems,
  maxLabels,
  maxTaints,
  nodeSettingsProblem,
  nodeSettingsText,
  nodesOf,
  nodesVariable,
  reservedProblem,
  taintProblems,
  valueProblem,
} from "./node-settings";

const reserved =
  "The kubelet refuses labels in kubernetes.io and k8s.io, except kubelet.kubernetes.io/ and node.kubernetes.io/.";

describe("a label or taint key", () => {
  it("is refused empty", () => {
    expect(keyProblem("")).toBe("Enter a key");
  });

  it("is a name, or a DNS prefix and a name", () => {
    expect(keyProblem("team")).toBeUndefined();
    expect(keyProblem("example.com/team")).toBeUndefined();
    expect(keyProblem("a_b-c.d")).toBeUndefined();
    expect(keyProblem("-team")).toBeDefined();
    expect(keyProblem("team-")).toBeDefined();
    expect(keyProblem("te am")).toBeDefined();
  });

  it("is refused with a bad prefix", () => {
    expect(keyProblem("Example.com/team")).toBe("The prefix is a DNS name of at most 253 characters");
    expect(keyProblem("/team")).toBe("The prefix is a DNS name of at most 253 characters");
    expect(keyProblem(`${"a".repeat(254)}/team`)).toBe("The prefix is a DNS name of at most 253 characters");
    expect(keyProblem("a/b/c")).toBe("Use name or prefix/name");
  });

  it("is refused with a name over 63 characters", () => {
    expect(keyProblem("n".repeat(63))).toBeUndefined();
    expect(keyProblem("n".repeat(64))).toBe("The name is at most 63 characters");
  });
});

describe("a label or taint value", () => {
  it("may be empty, or up to 63 characters of a name's", () => {
    expect(valueProblem("")).toBeUndefined();
    expect(valueProblem("a")).toBeUndefined();
    expect(valueProblem("v".repeat(63))).toBeUndefined();
  });

  it("is refused over 63 characters or with a character a name has none of", () => {
    expect(valueProblem("v".repeat(64))).toBe("At most 63 characters");
    expect(valueProblem("a b")).toBeDefined();
    expect(valueProblem("-a")).toBeDefined();
  });
});

describe("the kubelet's reserved label keys", () => {
  it("are refused", () => {
    for (const key of [
      "kubernetes.io/foo",
      "k8s.io/foo",
      "a.kubernetes.io/foo",
      "a.k8s.io/foo",
      "beta.kubernetes.io/foo",
    ]) {
      expect(reservedProblem(key)).toBe(reserved);
    }
  });

  it("are allowed for the keys the kubelet sets and for its two prefixes", () => {
    for (const key of [
      "kubernetes.io/hostname",
      "kubernetes.io/os",
      "kubernetes.io/arch",
      "beta.kubernetes.io/os",
      "beta.kubernetes.io/arch",
      "beta.kubernetes.io/instance-type",
      "node.kubernetes.io/instance-type",
      "topology.kubernetes.io/zone",
      "topology.kubernetes.io/region",
      "failure-domain.beta.kubernetes.io/zone",
      "failure-domain.beta.kubernetes.io/region",
      "kubelet.kubernetes.io/anything",
      "node.kubernetes.io/anything",
      "a.kubelet.kubernetes.io/anything",
      "a.node.kubernetes.io/anything",
    ]) {
      expect(reservedProblem(key)).toBeUndefined();
    }
  });

  it("leave other domains and keys without a prefix alone", () => {
    expect(reservedProblem("example.com/foo")).toBeUndefined();
    expect(reservedProblem("notkubernetes.io/foo")).toBeUndefined();
    expect(reservedProblem("kubernetes.io")).toBeUndefined();
  });
});

describe("the labels' problems", () => {
  it("name the key, the reserved prefix and the value", () => {
    expect(
      labelProblems([
        { key: "", value: "" },
        { key: "k8s.io/x", value: "" },
        { key: "ok", value: "a b" },
        { key: "fine", value: "yes" },
      ]),
    ).toStrictEqual([
      { key: "Enter a key" },
      { key: reserved },
      { value: "Letters, digits, dashes, dots and underscores, from a letter or digit" },
      {},
    ]);
  });

  it("refuse a key used twice", () => {
    expect(
      labelProblems([
        { key: "team", value: "a" },
        { key: "team", value: "b" },
      ]),
    ).toStrictEqual([{}, { key: "Each key once per pool." }]);
  });
});

describe("the taints' problems", () => {
  it("allow a kubernetes.io key, which only labels refuse", () => {
    expect(taintProblems([{ key: "node.kubernetes.io/unreachable", value: "", effect: "NoExecute" }])).toStrictEqual([
      {},
    ]);
    expect(taintProblems([{ key: "kubernetes.io/foo", value: "", effect: "NoSchedule" }])).toStrictEqual([{}]);
  });

  it("refuse an empty key, a bad value and an effect that is none of the three", () => {
    expect(
      taintProblems([
        { key: "", value: "", effect: "NoSchedule" },
        { key: "a", value: "x".repeat(64), effect: "PreferNoSchedule" },
        { key: "b", value: "", effect: "Never" },
      ]),
    ).toStrictEqual([
      { key: "Enter a key" },
      { value: "At most 63 characters" },
      { effect: "Choose NoSchedule, PreferNoSchedule, NoExecute" },
    ]);
  });

  it("refuse a key used twice", () => {
    expect(
      taintProblems([
        { key: "dedicated", value: "a", effect: "NoSchedule" },
        { key: "dedicated", value: "b", effect: "NoExecute" },
      ]),
    ).toStrictEqual([{}, { key: "Each key once per pool." }]);
  });
});

const label = (i: number): { key: string; value: string } => ({ key: `k${i}`, value: "" });
const taint = (i: number): { key: string; value: string; effect: string } => ({
  key: `t${i}`,
  value: "",
  effect: "NoSchedule",
});

describe("the settings' problem", () => {
  it("is none for nothing, or for the most labels and taints", () => {
    expect(nodeSettingsProblem({ labels: [], taints: [] })).toBeUndefined();
    expect(
      nodeSettingsProblem({
        labels: Array.from({ length: maxLabels }, (_, i) => label(i)),
        taints: Array.from({ length: maxTaints }, (_, i) => taint(i)),
      }),
    ).toBeUndefined();
  });

  it("refuses more than 50 labels or 20 taints", () => {
    expect(nodeSettingsProblem({ labels: Array.from({ length: maxLabels + 1 }, (_, i) => label(i)), taints: [] })).toBe(
      "At most 50 labels",
    );
    expect(nodeSettingsProblem({ labels: [], taints: Array.from({ length: maxTaints + 1 }, (_, i) => taint(i)) })).toBe(
      "At most 20 taints",
    );
  });

  it("is the first row's problem", () => {
    expect(nodeSettingsProblem({ labels: [{ key: "", value: "" }], taints: [] })).toBe("Enter a key");
    expect(nodeSettingsProblem({ labels: [], taints: [{ key: "a", value: "", effect: "x" }] })).toBe(
      "Choose NoSchedule, PreferNoSchedule, NoExecute",
    );
  });
});

describe("the settings as the API holds them", () => {
  it("read from a variable, with a taint's effect defaulting to NoSchedule", () => {
    expect(
      nodesOf({
        labels: [{ key: "team", value: "a" }, { key: "x" }],
        taints: [{ key: "d", value: "db", effect: "NoExecute" }, { key: "e" }],
      }),
    ).toStrictEqual({
      labels: [
        { key: "team", value: "a" },
        { key: "x", value: "" },
      ],
      taints: [
        { key: "d", value: "db", effect: "NoExecute" },
        { key: "e", value: "", effect: "NoSchedule" },
      ],
    });
    expect(nodesOf(undefined)).toStrictEqual({ labels: [], taints: [] });
  });

  it("are written without a taint's empty value", () => {
    expect(
      nodesVariable({
        labels: [{ key: "team", value: "" }],
        taints: [
          { key: "d", value: "", effect: "NoSchedule" },
          { key: "e", value: "v", effect: "NoExecute" },
        ],
      }),
    ).toStrictEqual({
      labels: [{ key: "team", value: "" }],
      taints: [
        { key: "d", effect: "NoSchedule" },
        { key: "e", value: "v", effect: "NoExecute" },
      ],
    });
  });

  it("are counted in a line", () => {
    expect(nodeSettingsText({ labels: [], taints: [] })).toBe("None");
    expect(nodeSettingsText({ labels: [{ key: "a", value: "" }], taints: [] })).toBe("1 label");
    expect(
      nodeSettingsText({
        labels: [
          { key: "a", value: "" },
          { key: "b", value: "" },
        ],
        taints: [{ key: "t", value: "", effect: "NoSchedule" }],
      }),
    ).toBe("2 labels, 1 taint");
  });
});

import { describe, expect, it } from "vitest";

import { upgradeHeldCondition } from "./fleet";
import { abnormal } from "./ui";

const marked = (types: string[], status: string): boolean[] => types.map((type) => abnormal(type, status));

describe("the Health page's condition list", () => {
  it("marks Kubehenge's conditions when they are true", () => {
    const kubehenge = [
      upgradeHeldCondition,
      "DeletionStalled",
      "VMsNotRunning",
      "VMsMissing",
      "CertificatesExpiring",
      "NodesSkipped",
      "NodesLackMemory",
    ];
    expect(marked(kubehenge, "True")).toStrictEqual(kubehenge.map(() => true));
    expect(marked(kubehenge, "False")).toStrictEqual(kubehenge.map(() => false));
  });

  it("marks Cluster API's conditions by their own polarity", () => {
    expect(marked(["Available", "Paused"], "True")).toStrictEqual([false, true]);
    expect(marked(["Available", "Paused"], "False")).toStrictEqual([true, false]);
  });
});

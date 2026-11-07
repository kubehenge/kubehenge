import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { fieldProblems, newForm, request, summary, withClass, type FieldKey } from "./create-form";
import { MachinesStep, type StepProps } from "./CreateSteps";
import type { ClusterClass } from "./types";

const withNodes: ClusterClass = {
  name: "kubehenge-k3s",
  distribution: "k3s",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+k3s\d+$` },
  variables: [
    { name: "storage", required: true, schema: { type: "string" } },
    { name: "workerNodes", required: false, schema: { type: "object" } },
  ],
};

const without: ClusterClass = { ...withNodes, variables: withNodes.variables.slice(0, 1) };

const form = {
  ...newForm(),
  storage: "ceph",
  workerNodes: {
    labels: [{ key: "team", value: "a" }],
    taints: [
      { key: "dedicated", value: "db", effect: "NoSchedule" },
      { key: "spot", value: "", effect: "PreferNoSchedule" },
    ],
  },
};

const noProblems = new Map<FieldKey, string>();

function step(
  cls: ClusterClass,
  set: StepProps["set"],
  shown: { attempted?: boolean; form?: typeof form } = {},
): ReturnType<typeof render> {
  return render(
    <MachinesStep
      form={shown.form ?? form}
      set={set}
      shown={(k) => noProblems.get(k)}
      cls={cls}
      usableStorages={[]}
      placed={false}
      haOffered={false}
      csi={{ offered: false, problem: undefined }}
      snapshots={{ offered: false, problem: undefined }}
      haGroups={false}
      nodes={[]}
      rules={{ haRules: undefined, haRulesError: undefined }}
      attempted={shown.attempted ?? false}
    />,
  );
}

describe("the workers' labels and taints in the wizard", () => {
  it("go into the request as the cluster-wide value, a taint's empty value left out", () => {
    expect(request(form, withNodes).variables["workerNodes"]).toStrictEqual({
      labels: [{ key: "team", value: "a" }],
      taints: [
        { key: "dedicated", value: "db", effect: "NoSchedule" },
        { key: "spot", effect: "PreferNoSchedule" },
      ],
    });
  });

  it("stay out of the request without any, without workers or for a class that lacks the variable", () => {
    expect(request({ ...newForm(), storage: "ceph" }, withNodes).variables).not.toHaveProperty("workerNodes");
    expect(request({ ...form, workerReplicas: 0 }, withNodes).variables).not.toHaveProperty("workerNodes");
    expect(request(form, without).variables).not.toHaveProperty("workerNodes");
  });

  it("show in the summary beside the workers' machines", () => {
    expect(Object.fromEntries(summary(form, withNodes))["Worker labels and taints"]).toBe("1 label, 2 taints");
    expect(Object.fromEntries(summary(form, without))).not.toHaveProperty("Worker labels and taints");
    expect(Object.fromEntries(summary({ ...form, workerReplicas: 0 }, withNodes))).not.toHaveProperty(
      "Worker labels and taints",
    );
  });

  it("hold the step back with a problem, and only while the class has them and workers exist", () => {
    const bad = { ...form, workerNodes: { labels: [{ key: "k8s.io/x", value: "" }], taints: [] } };
    expect(fieldProblems("Machines", bad, withNodes)["workerNodes"]).toMatch(/kubelet refuses labels/u);
    expect(fieldProblems("Machines", bad, without)).not.toHaveProperty("workerNodes");
    expect(fieldProblems("Machines", { ...bad, workerReplicas: 0 }, withNodes)).not.toHaveProperty("workerNodes");
    expect(fieldProblems("Machines", form, withNodes)).not.toHaveProperty("workerNodes");
  });

  it("are dropped with a class that has no such variable", () => {
    expect(withClass(form, without, () => true).workerNodes).toStrictEqual({ labels: [], taints: [] });
    expect(withClass(form, withNodes, () => true).workerNodes).toStrictEqual(form.workerNodes);
  });

  it("are edited on the machines step, where the class has them", async () => {
    const set = vi.fn<StepProps["set"]>();
    step(withNodes, set);
    const label = screen.getByRole("group", { name: "Label 1" });
    await userEvent.setup({ delay: null }).type(within(label).getByLabelText("Key"), "x");
    expect(set).toHaveBeenLastCalledWith("workerNodes", {
      ...form.workerNodes,
      labels: [{ key: "teamx", value: "a" }],
    });
  });

  it("are left off the machines step without the variable or without workers", () => {
    const set = vi.fn<StepProps["set"]>();
    step(without, set);
    expect(screen.queryByRole("group", { name: "Labels" })).not.toBeInTheDocument();
  });

  it("show every problem once Next was pressed, and before only the touched field's", () => {
    const bad = { ...form, workerNodes: { labels: [{ key: "", value: "" }], taints: [] } };
    const set = vi.fn<StepProps["set"]>();
    const { unmount } = step(withNodes, set, { form: bad });
    expect(screen.queryByText("Enter a key")).not.toBeInTheDocument();
    unmount();
    step(withNodes, set, { attempted: true, form: bad });
    expect(screen.getByText("Enter a key")).toBeVisible();
  });
});

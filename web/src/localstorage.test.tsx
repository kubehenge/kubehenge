import { render, screen } from "@testing-library/react";
import type { JSX } from "react";
import { describe, expect, it, vi } from "vitest";

import { newForm, type FieldKey } from "./create-form";
import { MachinesStep, type StepProps } from "./CreateSteps";
import type { Storage } from "./types";

const storage = (name: string, shared: boolean): Storage => ({
  storage: name,
  node: "pve1",
  shared,
  content: ["images"],
  active: true,
  avail: 1,
  total: 2,
  usable: true,
  allocatable: false,
});

const storages = [storage("ceph", true), storage("local-lvm", false)];
const noProblems = new Map<FieldKey, string>();

const step = (chosen: string): JSX.Element => (
  <MachinesStep
    form={{ ...newForm(), storage: chosen }}
    set={vi.fn<StepProps["set"]>()}
    shown={(k) => noProblems.get(k)}
    cls={undefined}
    usableStorages={storages}
    placed={false}
    haOffered={false}
    csi={{ offered: false, problem: undefined }}
    snapshots={{ offered: false, problem: undefined }}
    haGroups={false}
    nodes={[]}
    rules={{ haRules: undefined, haRulesError: undefined }}
  />
);

describe("the machines step's storage", () => {
  it("says that volumes on node-local storage stay on one node", () => {
    const { rerender } = render(step("local-lvm"));
    expect(screen.getByText("Volumes on node-local storage stay on one node.")).toBeInTheDocument();
    rerender(step("ceph"));
    expect(screen.queryByText("Volumes on node-local storage stay on one node.")).toBeNull();
  });
});

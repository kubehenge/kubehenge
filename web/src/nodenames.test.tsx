import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { fieldProblems, newForm, request, type Form } from "./create-form";
import { GeneralStep } from "./CreateSteps";
import type { ClusterClass } from "./types";

const classOf = (names: string[]): ClusterClass => ({
  name: "kubehenge-rke2",
  distribution: "rke2",
  annotations: {},
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  variables: names.map((name) => ({ name, required: false, schema: {} })),
});

const cls = classOf(["controlPlaneEndpoint", "network", "storage", "nodeNamePrefix"]);

describe("the create wizard's node name prefix", () => {
  it("is empty by default and sent only when set, for a class that defines it", () => {
    const form = { ...newForm(), name: "dev", storage: "tank" };
    expect(form.nodeNamePrefix).toBe("");
    expect(request(form, cls).variables).not.toHaveProperty("nodeNamePrefix");
    const prefixed = { ...form, nodeNamePrefix: "kubehenge-dev" };
    expect(request(prefixed, cls).variables).toMatchObject({ nodeNamePrefix: "kubehenge-dev" });
    expect(request(prefixed, classOf(["storage"])).variables).not.toHaveProperty("nodeNamePrefix");
  });

  it("is held to the rules of a cluster's name", () => {
    const form = { ...newForm(), name: "dev", version: "v1.36.5+rke2r1", className: cls.name };
    expect(fieldProblems("General", { ...form, nodeNamePrefix: "kubehenge-dev" }, cls).nodeNamePrefix).toBeUndefined();
    expect(fieldProblems("General", { ...form, nodeNamePrefix: "Dev" }, cls).nodeNamePrefix).toBe(
      "Use only lowercase letters, digits and dashes",
    );
    expect(fieldProblems("General", { ...form, nodeNamePrefix: "a".repeat(41) }, cls).nodeNamePrefix).toBe(
      "Name must be 40 characters or fewer",
    );
  });

  it("shows under Node names, with the cluster's name in its place", () => {
    const form = { ...newForm(), name: "dev" };
    render(
      <GeneralStep
        form={form}
        set={vi.fn<(k: keyof Form, v: unknown) => void>()}
        shown={(): string | undefined => undefined}
        classes={[cls]}
        cls={cls}
        offered={undefined}
        onChooseClass={vi.fn<(name: string) => void>()}
        namespaces={[]}
        onChooseNamespace={vi.fn<(namespace: string) => void>()}
        starts={[]}
        onChooseFrom={vi.fn<(key: string) => void>()}
      />,
    );
    expect(screen.getByText("Node names").closest("details")).not.toHaveAttribute("open");
    const field = screen.getByLabelText("Name prefix");
    expect(field).toHaveValue("");
    expect(field).toHaveAttribute("placeholder", "dev");
  });
});

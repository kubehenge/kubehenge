import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { newForm, type FieldKey } from "./create-form";
import { GeneralStep, type StepProps } from "./CreateSteps";
import { fakeServer } from "./fake-server";
import type { ClusterClass } from "./types";

const noProblems = new Map<FieldKey, string>();

const kubeadm: ClusterClass = {
  name: "kubehenge-kubeadm",
  distribution: "kubeadm",
  variables: [],
  versionFormat: { pattern: "" },
};

const rke2: ClusterClass = {
  ...kubeadm,
  name: "kubehenge-rke2",
  distribution: "rke2",
  noTemplate: "no VM template kubehenge-ubuntu-26.04 yet",
};

const step = (cls: ClusterClass): void => {
  render(
    <GeneralStep
      form={newForm()}
      set={vi.fn<StepProps["set"]>()}
      shown={(k) => noProblems.get(k)}
      classes={[cls]}
      cls={cls}
      offered={{ versions: ["v1.36.5"] }}
      onChooseClass={vi.fn<(name: string) => void>()}
    />,
  );
};

describe("a missing template", () => {
  it("is built on a cluster creator's request when the administrators allow it", async () => {
    const builds = { allowed: true, requested: [] as string[], building: [] as string[] };
    const calls = fakeServer({
      "GET /api/template-builds": () => ({ body: builds }),
      "POST /api/template-builds": () => {
        builds.requested = ["kubeadm-v1.37.1"];
        return { status: 202 };
      },
    });
    const user = userEvent.setup({ delay: null });
    step(kubeadm);
    const button = await screen.findByRole("button", { name: "Have the template built" });
    expect(button).toBeDisabled();
    await user.type(screen.getByLabelText("Kubernetes version to build"), "1.37");
    expect(screen.getByLabelText("Kubernetes version to build")).toHaveAttribute("aria-invalid", "true");
    await user.clear(screen.getByLabelText("Kubernetes version to build"));
    await user.type(screen.getByLabelText("Kubernetes version to build"), "v1.37.1");
    await user.click(button);
    await waitFor(() => expect(screen.getByRole("button", { name: "Asked for" })).toBeDisabled());
    expect(calls.find((c) => c.key === "POST /api/template-builds")?.body).toStrictEqual({
      distribution: "kubeadm",
      kubernetesVersion: "v1.37.1",
    });
  });

  it("asks for the shared cloud image only while it is missing", async () => {
    fakeServer({ "GET /api/template-builds": { body: { allowed: true, requested: [], building: [] } } });
    step(rke2);
    await expect(screen.findByRole("button", { name: "Have the template built" })).resolves.toBeInTheDocument();
    expect(screen.queryByLabelText("Kubernetes version to build")).not.toBeInTheDocument();
  });

  it("is not offered when the administrators do not allow it", async () => {
    const calls = fakeServer({
      "GET /api/template-builds": { body: { allowed: false, requested: [], building: [] } },
    });
    step(rke2);
    await waitFor(() => expect(calls.map((c) => c.key)).toContain("GET /api/template-builds"));
    expect(screen.queryByRole("button", { name: "Have the template built" })).not.toBeInTheDocument();
  });
});

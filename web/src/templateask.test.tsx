import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { newForm, withClass, type FieldKey } from "./create-form";
import { GeneralStep, type StepProps } from "./CreateSteps";
import { fakeServer } from "./fake-server";
import type { ClusterClass, NodeOSChoice } from "./types";

const noProblems = new Map<FieldKey, string>();

const alma: NodeOSChoice = {
  family: "almalinux",
  release: "10",
  title: "AlmaLinux 10",
  cpuType: "x86-64-v2-AES",
  mac: "selinux",
  default: true,
};

const rocky: NodeOSChoice = {
  family: "rocky",
  release: "10",
  title: "Rocky Linux 10",
  cpuType: "x86-64-v3",
  mac: "selinux",
};

const kubeadm: ClusterClass = {
  name: "kubehenge-kubeadm",
  distribution: "kubeadm",
  variables: [],
  versionFormat: { pattern: "" },
  nodeOSes: [alma, rocky],
};

const rke2: ClusterClass = {
  ...kubeadm,
  name: "kubehenge-rke2",
  distribution: "rke2",
  nodeOSes: [
    alma,
    {
      ...rocky,
      noTemplate:
        "there is no RKE2 VM template on Rocky Linux 10 yet; an administrator builds it on the Administration page",
    },
  ],
};

const step = (cls: ClusterClass, nodeOS = ""): void => {
  render(
    <GeneralStep
      form={{ ...withClass(newForm(), cls, () => false), ...(nodeOS && { nodeOS }) }}
      set={vi.fn<StepProps["set"]>()}
      shown={(k) => noProblems.get(k)}
      classes={[cls]}
      cls={cls}
      offered={{ versions: ["v1.36.5"] }}
      onChooseClass={vi.fn<(name: string) => void>()}
      namespaces={["clusters"]}
      onChooseNamespace={vi.fn<(namespace: string) => void>()}
    />,
  );
};

describe("a missing template", () => {
  it("is built on a cluster creator's request when the administrators allow it", async () => {
    const builds = { allowed: true, requested: [] as string[], building: [] as string[] };
    const calls = fakeServer({
      "GET /api/template-builds": () => ({ body: builds }),
      "POST /api/template-builds": () => {
        builds.requested = ["kubeadm-v1.37.1-rocky-10"];
        return { status: 202 };
      },
    });
    const user = userEvent.setup({ delay: null });
    step(kubeadm, "rocky-10");
    const button = await screen.findByRole("button", { name: "Have the template built" });
    expect(button).toBeDisabled();
    expect(screen.getByText(/versions that have a VM template on Rocky Linux 10\./u)).toBeInTheDocument();
    await user.type(screen.getByLabelText("Kubernetes version to build"), "1.37");
    expect(screen.getByLabelText("Kubernetes version to build")).toHaveAttribute("aria-invalid", "true");
    await user.clear(screen.getByLabelText("Kubernetes version to build"));
    await user.type(screen.getByLabelText("Kubernetes version to build"), "v1.37.1");
    await user.click(button);
    await waitFor(() => expect(screen.getByRole("button", { name: "Asked for" })).toBeDisabled());
    expect(calls.find((c) => c.key === "POST /api/template-builds")?.body).toStrictEqual({
      distribution: "kubeadm",
      kubernetesVersion: "v1.37.1",
      nodeOS: "rocky-10",
    });
  });

  it("asks for the distribution's template on the node OS chosen only while it is missing", async () => {
    const calls = fakeServer({
      "GET /api/template-builds": { body: { allowed: true, requested: [], building: [] } },
      "POST /api/template-builds": { status: 202 },
    });
    const user = userEvent.setup({ delay: null });
    step(rke2, "rocky-10");
    await user.click(await screen.findByRole("button", { name: "Have the template built" }));
    expect(screen.queryByLabelText("Kubernetes version to build")).not.toBeInTheDocument();
    await waitFor(() =>
      expect(calls.find((c) => c.key === "POST /api/template-builds")?.body).toStrictEqual({
        distribution: "rke2",
        nodeOS: "rocky-10",
      }),
    );
  });

  it("says when the template on the node OS chosen was asked for already", async () => {
    fakeServer({
      "GET /api/template-builds": { body: { allowed: true, requested: ["rke2-rocky-10"], building: [] } },
    });
    step(rke2, "rocky-10");
    await expect(screen.findByRole("button", { name: "Asked for" })).resolves.toBeDisabled();
  });

  it("is not offered while the node OS chosen has its template", async () => {
    const calls = fakeServer({
      "GET /api/template-builds": { body: { allowed: true, requested: [], building: [] } },
    });
    step(rke2);
    await waitFor(() => expect(calls.map((c) => c.key)).toContain("GET /api/template-builds"));
    expect(screen.queryByRole("button", { name: "Have the template built" })).not.toBeInTheDocument();
  });

  it("is not offered when the administrators do not allow it", async () => {
    const calls = fakeServer({
      "GET /api/template-builds": { body: { allowed: false, requested: [], building: [] } },
    });
    step(rke2, "rocky-10");
    await waitFor(() => expect(calls.map((c) => c.key)).toContain("GET /api/template-builds"));
    expect(screen.queryByRole("button", { name: "Have the template built" })).not.toBeInTheDocument();
  });
});

import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fieldProblems, newForm, request } from "./create-form";
import { applyStaged, fakeServer } from "./fake-server";
import { graceOf, graceProblem, graceText } from "./HostOutage";
import type { ClusterClass } from "./types";

const cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5+rke2r1",
  generation: 1,
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 3, ready: 3, available: 3, upToDate: 3 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { hostOutageGraceMinutes: 45 },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

const cls: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  annotations: {},
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  variables: [
    "controlPlaneEndpoint",
    "network",
    "storage",
    "controlPlaneMachine",
    "workerMachine",
    "hostOutageGraceMinutes",
  ].map((name) => ({ name, required: false, schema: {} })),
};

describe("the create wizard's host outage wait", () => {
  it("sends the minutes only when they differ from the default, and only for a class that defines them", () => {
    const form = { ...newForm(), name: "web", storage: "tank" };
    expect(request(form, cls).variables).not.toHaveProperty("hostOutageGraceMinutes");
    expect(request({ ...form, hostOutageGrace: "0" }, cls).variables).toMatchObject({ hostOutageGraceMinutes: 0 });
    expect(request({ ...form, hostOutageGrace: "90" }, cls).variables).toMatchObject({ hostOutageGraceMinutes: 90 });
    const without = { ...cls, variables: cls.variables.filter((v) => v.name !== "hostOutageGraceMinutes") };
    expect(request({ ...form, hostOutageGrace: "90" }, without).variables).not.toHaveProperty("hostOutageGraceMinutes");
  });

  it("holds the step back for minutes the class refuses", () => {
    const form = { ...newForm(), storage: "tank", hostOutageGrace: "3" };
    expect(fieldProblems("Machines", form, cls).hostOutageGrace).toBe("0, or 5 to 10080");
    expect(fieldProblems("Machines", { ...form, hostOutageGrace: "5" }, cls).hostOutageGrace).toBeUndefined();
  });
});

describe("the host outage wait", () => {
  it("reads and says the minutes", () => {
    expect(graceOf(undefined)).toBe("30");
    expect(graceOf(0)).toBe("0");
    expect(graceText("0")).toBe("Until the host returns");
    expect(graceText("45")).toBe("45 minutes");
  });

  it("accepts 0 or 5 to a week, and nothing else", () => {
    for (const ok of ["0", "5", "30", "10080"]) {
      expect(graceProblem(ok)).toBeUndefined();
    }
    for (const bad of ["", "4", "1", "10081", "1.5", "soon", "-5"]) {
      expect(graceProblem(bad)).toBeDefined();
    }
  });

  it("is changed from the options with the minutes the cluster sends", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const row = await screen.findByRole("rowheader", { name: "Host outage wait" });
    expect(row.closest("tr")).toHaveTextContent("45 minutes");
    await user.click(screen.getByRole("button", { name: "Edit the host outage wait" }));
    const dialog = screen.getByRole("dialog", { name: "Edit host outage wait" });
    const field = within(dialog).getByLabelText("Host outage wait");
    const save = within(dialog).getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    await user.clear(field);
    await user.type(field, "3");
    expect(save).toBeDisabled();
    expect(within(dialog).getByText("0, or 5 to 10080")).toBeInTheDocument();
    await user.clear(field);
    await user.type(field, "0");
    await user.click(save);
    await applyStaged(user);
    await waitFor(() =>
      expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
        variables: { hostOutageGraceMinutes: 0 },
      }),
    );
  });
});

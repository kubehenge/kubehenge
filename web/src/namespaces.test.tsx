import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { newForm, request, summary, withNamespace } from "./create-form";
import { fakeServer } from "./fake-server";
import type { ClusterClass } from "./types";

const rke2: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  variables: [{ name: "storage", required: true, schema: { type: "string" } }],
};

function openWizard(session: Record<string, unknown>): void {
  window.history.replaceState(null, "", "/clusters/new");
  fakeServer({
    "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1", ...session } }),
    "GET /api/classes": () => ({ body: [rke2] }),
    "GET /api/proxmox/storages": () => ({ body: [] }),
    "GET /api/proxmox/nodes": () => ({ body: [] }),
  });
  render(<App />);
}

describe("the namespace of a new cluster", () => {
  it("is asked for only where the interface manages more than one", async () => {
    openWizard({ namespace: "clusters" });
    await screen.findByLabelText("Name");
    expect(screen.queryByLabelText("Namespace")).not.toBeInTheDocument();
  });

  it("starts at the interface's own and offers the others", async () => {
    openWizard({ namespace: "clusters", namespaces: ["clusters", "team-a", "team-b"] });
    const user = userEvent.setup({ delay: null });
    const namespace = await screen.findByLabelText("Namespace");
    expect(namespace).toHaveValue("clusters");
    expect(
      within(namespace)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toStrictEqual(["clusters", "team-a", "team-b"]);
    await user.selectOptions(namespace, "team-b");
    await waitFor(() => expect(namespace).toHaveValue("team-b"));
  });

  it("goes into the request and the summary once chosen, and not otherwise", () => {
    const form = { ...newForm(), name: "web", version: "v1.36.5+rke2r1", storage: "local-lvm" };
    expect(request(form, rke2)).not.toHaveProperty("namespace");
    expect(summary(form, rke2).map(([k]) => k)).not.toContain("Namespace");
    const chosen = { ...form, namespace: "team-a" };
    expect(request(chosen, rke2)).toHaveProperty("namespace", "team-a");
    expect(summary(chosen, rke2)[0]).toStrictEqual(["Namespace", "team-a"]);
  });

  it("drops the Cluster Mesh peers of the namespace it leaves", () => {
    const form = { ...newForm(), clusterMesh: true, meshPeers: ["api"] };
    expect(withNamespace(form, "clusters", "clusters").meshPeers).toStrictEqual(["api"]);
    expect(withNamespace(form, "team-a", "clusters")).toMatchObject({ namespace: "team-a", meshPeers: [] });
  });
});

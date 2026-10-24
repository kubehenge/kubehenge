import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import {
  newForm,
  request,
  snapshotStoragesAnnotation,
  snapshotsOffer,
  snapshotsProblems,
  summary,
  volumeSnapshotsAnnotation,
  type FieldKey,
} from "./create-form";
import { MachinesStep, type StepProps } from "./CreateSteps";
import { fakeServer } from "./fake-server";
import { formOf, rootPasswordOf, settings, setupFieldProblems } from "./Setup";
import type { Cluster, ClusterClass } from "./types";

const k3s: ClusterClass = {
  name: "kubehenge-k3s",
  distribution: "k3s",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+k3s\d+$` },
  annotations: { [volumeSnapshotsAnnotation]: "true", [snapshotStoragesAnnotation]: "local,local-zfs" },
  variables: [
    { name: "storage", required: true, schema: { type: "string" } },
    { name: "csi", required: false, schema: { type: "object" } },
  ],
};

const cluster: Cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-k3s",
  distribution: "k3s",
  version: "v1.36.5+k3s1",
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { storage: "local-zfs", csi: { datastoreAllocate: true } },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

const noProblems = new Map<FieldKey, string>();

describe("volume snapshots", () => {
  it("are offered where an administrator lets clusters take them, on node-local storage, to those who hold Permissions.Modify on /", () => {
    expect([
      snapshotsOffer(k3s, "local-zfs", true),
      snapshotsOffer(k3s, "local-zfs", undefined),
      snapshotsOffer({ ...k3s, annotations: {} }, "local-zfs", true),
      snapshotsOffer({ ...k3s, variables: k3s.variables.slice(0, 1) }, "local-zfs", true),
      snapshotsOffer(k3s, "", true).problem,
      snapshotsOffer(k3s, "ceph", true).problem,
      snapshotsOffer(k3s, "local", false).problem,
    ]).toStrictEqual([
      { offered: true, problem: undefined },
      { offered: true, problem: undefined },
      { offered: false, problem: undefined },
      { offered: false, problem: undefined },
      "Choose the cluster's storage first.",
      "The CSI plugin takes volume snapshots only on storage local to each node (ZFS, LVM, LVM-thin, directory or btrfs), which storage ceph is not.",
      "You do not hold Permissions.Modify on /, which giving the cluster's CSI plugin root@pam takes.",
    ]);
  });

  it("are asked for in the csi variable only when chosen, beside the newest release", () => {
    const form = { ...newForm(), storage: "local-zfs" };
    expect(request(form, k3s).variables).not.toHaveProperty("csi");
    expect(request({ ...form, volumeSnapshots: true }, k3s).variables).toHaveProperty("csi", { volumeSnapshots: true });
    expect(request({ ...form, volumeSnapshots: true, datastoreAllocate: true }, k3s).variables).toHaveProperty("csi", {
      datastoreAllocate: true,
      volumeSnapshots: true,
    });
    expect(
      Object.fromEntries(summary({ ...form, volumeSnapshots: true }, k3s, { snapshots: true }))["Volume snapshots"],
    ).toBe("On, with the CSI plugin signed in as root@pam");
    expect(Object.fromEntries(summary(form, k3s))).not.toHaveProperty("Volume snapshots");
  });

  it("hold the machines step back while chosen where something stands in the way", () => {
    const refused = { offered: true, problem: "No." };
    const form = { ...newForm(), volumeSnapshots: true };
    expect([
      snapshotsProblems("Machines", form, refused),
      snapshotsProblems("Machines", { ...form, volumeSnapshots: false }, refused),
      snapshotsProblems("Add-ons", form, refused),
      snapshotsProblems("Machines", form, { offered: true, problem: undefined }),
    ]).toStrictEqual([{ volumeSnapshots: "No." }, {}, {}, {}]);
  });

  it("are chosen on the machines step, saying what they grant", async () => {
    const set = vi.fn<StepProps["set"]>();
    render(
      <MachinesStep
        form={{ ...newForm(), storage: "local-zfs" }}
        set={set}
        shown={(k) => noProblems.get(k)}
        cls={k3s}
        usableStorages={[]}
        placed={false}
        haOffered={false}
        csi={{ offered: false, problem: undefined }}
        snapshots={{ offered: true, problem: undefined }}
        haGroups={false}
        nodes={[]}
        rules={{ haRules: undefined, haRulesError: undefined }}
      />,
    );
    const box = screen.getByLabelText("Take volume snapshots");
    expect(box).toHaveAccessibleDescription(/holds the whole Proxmox VE cluster/u);
    await userEvent.setup({ delay: null }).click(box);
    expect(set).toHaveBeenCalledWith("volumeSnapshots", true);
  });
});

function openOptions(classes: ClusterClass[], canModifyPermissions: boolean, shown: Cluster = cluster): () => unknown {
  window.history.replaceState(null, "", "/clusters/clusters/web/options");
  const calls = fakeServer({
    "GET /api/session": { body: { user: "dev@pve", csrf: "c", canModifyPermissions } },
    "GET /api/clusters/clusters/web": { body: shown },
    "GET /api/clusters": { body: [shown] },
    "GET /api/classes": { body: classes },
    "GET /api/proxmox/storages": { body: [] },
    "PATCH /api/clusters/clusters/web": { body: shown },
  });
  render(<App />);
  return () => calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body;
}

describe("a cluster's volume snapshots", () => {
  it("are switched on under Options, keeping the plugin's release", async () => {
    const sent = openOptions([k3s], true);
    const user = userEvent.setup({ delay: null });
    const heading = await screen.findByRole("rowheader", { name: "Volume snapshots" });
    expect(heading.closest("tr")).toHaveTextContent("Off");
    await user.click(screen.getByRole("button", { name: "Edit volume snapshots" }));
    const dialog = screen.getByRole("dialog", { name: "Volume snapshots" });
    const save = within(dialog).getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    await waitFor(() => expect(within(dialog).getByLabelText("Take volume snapshots")).toBeEnabled());
    await user.click(within(dialog).getByLabelText("Take volume snapshots"));
    await user.click(save);
    await waitFor(() =>
      expect(sent()).toStrictEqual({ variables: { csi: { datastoreAllocate: true, volumeSnapshots: true } } }),
    );
  });

  it("cannot be switched on without Permissions.Modify on /", async () => {
    openOptions([k3s], false);
    const user = userEvent.setup({ delay: null });
    await user.click(await screen.findByRole("button", { name: "Edit volume snapshots" }));
    const dialog = screen.getByRole("dialog", { name: "Volume snapshots" });
    await expect(within(dialog).findByText(/You do not hold Permissions.Modify on \//u)).resolves.toBeInTheDocument();
    expect(within(dialog).getByLabelText("Take volume snapshots")).toBeDisabled();
  });

  it("stay switchable off after an administrator withdraws them", async () => {
    const sent = openOptions([{ ...k3s, annotations: {} }], true, {
      ...cluster,
      variables: { storage: "local-zfs", csi: { volumeSnapshots: true } },
    });
    const user = userEvent.setup({ delay: null });
    const heading = await screen.findByRole("rowheader", { name: "Volume snapshots" });
    expect(heading.closest("tr")).toHaveTextContent("root@pam");
    await user.click(screen.getByRole("button", { name: "Edit volume snapshots" }));
    const dialog = screen.getByRole("dialog", { name: "Volume snapshots" });
    await user.click(within(dialog).getByLabelText("Take volume snapshots"));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(sent()).toStrictEqual({ variables: { csi: { volumeSnapshots: false } } }));
  });

  it("are not shown where an administrator has not let clusters take them", async () => {
    openOptions([{ ...k3s, annotations: {} }], true);
    await screen.findByRole("rowheader", { name: "Placement" });
    await expect(screen.findByRole("rowheader", { name: "Volume snapshots" }, { timeout: 200 })).rejects.toThrow(
      /Unable to find/u,
    );
  });
});

describe("the appliance's volume snapshots setting", () => {
  const form = {
    ...formOf(),
    clusterVolumeSnapshots: true,
  };

  it("asks for root@pam's password when switched on, and keeps the stored one after", () => {
    expect(setupFieldProblems(form).rootPassword).toMatch(/root@pam's password/u);
    expect(setupFieldProblems({ ...form, rootPassword: "s3cret" }).rootPassword).toBeUndefined();
    const stored = { ...settings(formOf()), clusterVolumeSnapshots: true };
    expect(setupFieldProblems(form, false, stored).rootPassword).toBeUndefined();
    expect(setupFieldProblems({ ...form, clusterVolumeSnapshots: false }).rootPassword).toBeUndefined();
  });

  it("never carries the password in the settings", () => {
    const s = settings({ ...form, rootPassword: "s3cret" });
    expect(s).toMatchObject({ clusterVolumeSnapshots: true });
    expect(JSON.stringify(s)).not.toContain("s3cret");
  });

  it("hands over the password only while volume snapshots are on", () => {
    expect(rootPasswordOf({ ...form, rootPassword: "s3cret" })).toBe("s3cret");
    expect(rootPasswordOf({ ...form, clusterVolumeSnapshots: false, rootPassword: "s3cret" })).toBe("");
  });
});

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState, type JSX } from "react";
import { describe, expect, it } from "vitest";

import { AddOnsStep } from "./AddOnsStep";
import { expectNoViolations } from "./axe";
import { backupCredentialHint, classTargets, newBackupFields } from "./Backups";
import {
  etcdChosen,
  fieldProblems,
  newForm,
  pendingCredentials,
  request,
  snapshotsWait,
  snapshotsWaitText,
  summary,
  withBackupChoices,
  type Form,
} from "./create-form";
import type { ClusterClass } from "./types";
import { check } from "./writing";

// The zone backups start in, which snapshots start in too.
const browserZone = (): string => newBackupFields().timeZone;

const targets = JSON.stringify([
  { name: "store", endpoint: "https://s3.example.org:8333", bucket: "backups" },
  { name: "aws", bucket: "aws-backups" },
]);

const oneTarget = JSON.stringify([{ name: "store", bucket: "backups" }]);

const object = { type: "object" };

const classOf = (
  distribution: string,
  offered: string | undefined,
  names = ["etcdSnapshots", "backups"],
): ClusterClass => ({
  name: `kubehenge-${distribution}`,
  distribution,
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+$` },
  annotations: offered === undefined ? {} : { "kubehenge.dev/backup-targets": offered },
  variables: names.map((name) => ({ name, required: false, schema: object })),
});

const rke2 = classOf("rke2", targets);
const kubeadm = classOf("kubeadm", oneTarget);
const bare = classOf("rke2", undefined);
const withoutVariable = classOf("rke2", targets, ["backups"]);

const key = { accessKeyID: "AKIA", secretAccessKey: "secret" };

// hintTexts are the texts of the hints rendered in root.
const hintTexts = (root: ParentNode): string[] => [...root.querySelectorAll("p.hint")].map((p) => p.textContent ?? "");

const filledForm = (c: ClusterClass, patch: Partial<Form> = {}): Form =>
  withBackupChoices({ ...newForm(), name: "web", etcdSnapshots: true, backups: false, ...patch }, c, true);

describe("a new cluster's etcd snapshots", () => {
  it("are on from the defaults with the first target chosen, and Next needs no input", () => {
    for (const c of [kubeadm, rke2]) {
      const fresh = withBackupChoices({ ...newForm(), name: "web" }, c, true);
      expect({ chosen: etcdChosen(fresh, c) }).toStrictEqual({ chosen: true });
      expect(fresh.etcdFields.target).toBe("store");
      expect(fieldProblems("Add-ons", fresh, c)).toStrictEqual({});
      expect(pendingCredentials(fresh, c)).toStrictEqual([]);
    }
  });

  it("pass Next in all three key cases, and wait only with no key at all", () => {
    const wait = "No snapshots until a key for store is stored";
    const cases = [
      { name: "shared key", patch: { noSharedKey: false }, waits: false, sent: undefined, pending: [] },
      {
        name: "own key",
        patch: { noSharedKey: true, backupKey: { accessKeyID: "AK", secretAccessKey: "SK" } },
        waits: false,
        sent: { accessKeyID: "AK", secretAccessKey: "SK" },
        pending: ["backup"],
      },
      { name: "neither", patch: { noSharedKey: true }, waits: true, sent: undefined, pending: [] },
    ];
    for (const c of cases) {
      const form = filledForm(kubeadm, c.patch);
      expect({ name: c.name, problems: fieldProblems("Add-ons", form, kubeadm) }).toStrictEqual({
        name: c.name,
        problems: {},
      });
      expect({ name: c.name, waits: snapshotsWait(form, kubeadm) }).toStrictEqual({ name: c.name, waits: c.waits });
      expect({ name: c.name, key: request(form, kubeadm).backupKey }).toStrictEqual({ name: c.name, key: c.sent });
      expect(pendingCredentials(form, kubeadm).map((p) => p.purpose)).toStrictEqual(c.pending);
    }
    expect(snapshotsWaitText("store")).toBe(wait);
  });

  it("say on the step and on Confirm that no snapshots are taken while no key exists", () => {
    render(<Step cls={kubeadm} first={{ noSharedKey: true }} />);
    expect(screen.getByText("No snapshots until a key for store is stored")).toBeInTheDocument();
    const form = filledForm(kubeadm, { noSharedKey: true });
    expect(Object.fromEntries(summary(form, kubeadm))["etcd snapshots"]).toBe(
      "No snapshots until a key for store is stored",
    );
  });

  it("say nothing of waiting where a key for every cluster is stored", () => {
    render(<Step cls={kubeadm} />);
    expect(screen.queryByText(/Kept on the management cluster/u)).not.toBeInTheDocument();
  });

  it("take the one target the class offers once chosen", () => {
    const form = filledForm(kubeadm);
    expect({ chosen: etcdChosen(form, kubeadm) }).toStrictEqual({ chosen: true });
    expect(form.etcdFields.target).toBe("store");
    expect(filledForm(rke2).etcdFields.target).toBe("store");
  });

  it("are off and absent where no variable is offered", () => {
    const form = filledForm(withoutVariable);
    expect({ chosen: etcdChosen(form, withoutVariable) }).toStrictEqual({ chosen: false });
    expect(fieldProblems("Add-ons", form, withoutVariable)).toStrictEqual({});
    expect(request(form, withoutVariable).variables).not.toHaveProperty("etcdSnapshots");
    expect(Object.fromEntries(summary(form, withoutVariable))).not.toHaveProperty("etcd snapshots");
    expect(pendingCredentials(form, withoutVariable)).toStrictEqual([]);
  });

  it("go to the backup volume where no target is offered, with no key and no wait", () => {
    const form = filledForm(bare, { noSharedKey: true });
    expect({ chosen: etcdChosen(form, bare) }).toStrictEqual({ chosen: true });
    expect(fieldProblems("Add-ons", form, bare)).toStrictEqual({});
    expect(request(form, bare).variables["etcdSnapshots"]).toStrictEqual({
      enabled: true,
      schedule: "0 */12 * * *",
      timeZone: form.etcdFields.timeZone,
      keep: 14,
    });
    expect(Object.fromEntries(summary(form, bare))["etcd snapshots"]).toMatch(/to the backup volume/u);
    expect(pendingCredentials(form, bare)).toStrictEqual([]);
    // Half a key typed for a bucket before is neither asked for nor stored.
    const halfKey = filledForm(bare, { noSharedKey: true, backupKey: { accessKeyID: "AK", secretAccessKey: "" } });
    expect(fieldProblems("Add-ons", halfKey, bare)).toStrictEqual({});
    expect(pendingCredentials(halfKey, bare)).toStrictEqual([]);
  });

  it("take the backups' target when it is one of the offered", () => {
    const form = withBackupChoices(
      {
        ...newForm(),
        backups: true,
        backupFields: { ...newBackupFields(), target: "aws" },
      },
      rke2,
      true,
    );
    expect(form.etcdFields.target).toBe("aws");
  });

  it("hold the step until the target, schedule and number kept are right", () => {
    const form = filledForm(rke2, { backupKey: key });
    expect(fieldProblems("Add-ons", { ...form, etcdFields: { ...form.etcdFields, target: "" } }, rke2)).toStrictEqual({
      etcdTarget: "Choose a target",
    });
    const wrong = {
      ...form,
      etcdFields: { ...form.etcdFields, target: "gone", schedule: "daily", keep: "0" },
    };
    expect(Object.keys(fieldProblems("Add-ons", wrong, rke2)).toSorted()).toStrictEqual([
      "etcdKeep",
      "etcdSchedule",
      "etcdTarget",
    ]);
    const off = { ...wrong, etcdSnapshots: false };
    expect(fieldProblems("Add-ons", off, rke2)).toStrictEqual({});
  });

  it("start in the browser's time zone, as backups do", () => {
    const form = filledForm(kubeadm);
    expect(form.etcdFields.timeZone).toBe(browserZone());
    expect(form.backupFields.timeZone).toBe(browserZone());
  });

  it("go to the backups' target when both are on", () => {
    const both = filledForm(rke2, {
      backups: true,
      backupKey: key,
      backupFields: { ...newBackupFields(), target: "aws" },
    });
    expect(both.etcdFields.target).toBe("aws");
    expect(fieldProblems("Add-ons", both, rke2)).toStrictEqual({});
    const apart = { ...both, etcdFields: { ...both.etcdFields, target: "store" } };
    expect(fieldProblems("Add-ons", apart, rke2)).toStrictEqual({ etcdTarget: "Use the backups' target" });
    expect(fieldProblems("Add-ons", { ...apart, backups: false }, rke2)).toStrictEqual({});
    expect(fieldProblems("Add-ons", { ...apart, etcdSnapshots: false }, rke2)).toStrictEqual({});
  });

  it("go into the request, and nothing goes in when they are off", () => {
    const form = filledForm(rke2, {
      backupKey: key,
      etcdFields: { ...newForm().etcdFields, target: "aws", keep: "30" },
    });
    expect(fieldProblems("Add-ons", form, rke2)).toStrictEqual({});
    expect(request(form, rke2).variables["etcdSnapshots"]).toStrictEqual({
      enabled: true,
      target: "aws",
      schedule: "0 */12 * * *",
      timeZone: browserZone(),
      keep: 30,
    });
    // The key travels beside the variables, which the cluster object keeps.
    expect(JSON.stringify(request(form, rke2).variables)).not.toContain("secret");
    expect(request({ ...form, etcdSnapshots: false }, rke2).variables).not.toHaveProperty("etcdSnapshots");
  });

  it("carry the distribution's own upload only where it has one", () => {
    const upload = { ...newForm().etcdFields, target: "store", upload: true };
    expect(
      request(filledForm(rke2, { backupKey: key, etcdFields: upload }), rke2).variables["etcdSnapshots"],
    ).toMatchObject({
      distributionUpload: true,
    });
    expect(
      request(filledForm(kubeadm, { backupKey: key, etcdFields: upload }), kubeadm).variables["etcdSnapshots"],
    ).not.toHaveProperty("distributionUpload");
  });

  it("share the backups' key, which is asked for once", () => {
    const etcdOnly = filledForm(kubeadm);
    expect(fieldProblems("Add-ons", etcdOnly, kubeadm)).toStrictEqual({});
    const uploading = filledForm(rke2, { etcdFields: { ...filledForm(rke2).etcdFields, upload: true } });
    expect(fieldProblems("Add-ons", uploading, rke2)).toStrictEqual({
      backupKey: "Enter both parts of the access key",
    });
    const both = { ...etcdOnly, backups: true, backupKey: key };
    expect(fieldProblems("Add-ons", both, kubeadm)).toStrictEqual({});
    expect(pendingCredentials(both, kubeadm)).toStrictEqual([
      { purpose: "backup", values: key, label: "S3 access key", section: "Backups", forget: {} },
    ]);
    expect(pendingCredentials({ ...etcdOnly, backupKey: key }, kubeadm)).toMatchObject([
      { purpose: "backup", values: key, section: "etcd snapshots" },
    ]);
    expect(pendingCredentials({ ...etcdOnly, etcdSnapshots: false, backupKey: key }, kubeadm)).toStrictEqual([]);
  });

  it("wait for the key where it cannot be stored, as backups do", () => {
    const form = withBackupChoices(
      { ...newForm(), etcdSnapshots: true, backups: false, backupKey: key },
      kubeadm,
      false,
    );
    expect(form).toMatchObject({ storeBackupKey: false });
    expect(fieldProblems("Add-ons", form, kubeadm)).toStrictEqual({});
    expect(pendingCredentials(form, kubeadm)).toStrictEqual([]);
    expect(Object.fromEntries(summary(form, kubeadm))["etcd snapshots"]).toBe(
      `Every 12 hours from 00:00 (${browserZone()}), keeps 14, to store, access key to store later`,
    );
  });

  it("are summed up beside the backups", () => {
    const form = filledForm(kubeadm, { backupKey: key });
    expect(summary(form, kubeadm).map(([label]) => label)).toContain("etcd snapshots");
    expect(Object.fromEntries(summary(form, kubeadm))["etcd snapshots"]).toBe(
      `Every 12 hours from 00:00 (${browserZone()}), keeps 14, to store, own access key`,
    );
    expect(Object.fromEntries(summary({ ...form, etcdSnapshots: false }, kubeadm))["etcd snapshots"]).toBe("Off");
  });
});

// Step keeps the form as the wizard does: the key and targets are filled in by withBackupChoices
// and a field's problem shows once Next was pressed.
function Step({
  cls,
  canTrust = true,
  first,
}: {
  cls: ClusterClass;
  canTrust?: boolean;
  first?: Partial<Form>;
}): JSX.Element {
  const [entered, setEntered] = useState<Form>({
    ...newForm(),
    name: "web",
    etcdSnapshots: true,
    backups: false,
    ...first,
  });
  const form = withBackupChoices(entered, cls, canTrust);
  const found = fieldProblems("Add-ons", form, cls);
  return (
    <AddOnsStep
      form={form}
      set={(k, v) => setEntered((f) => ({ ...f, [k]: v }))}
      shown={(k) => found[k]}
      cls={cls}
      canTrust={canTrust}
      namespace={undefined}
    />
  );
}

describe("the add-ons step", () => {
  it("moves the snapshots to the backup volume and back where the class offers a bucket", async () => {
    const user = userEvent.setup({ delay: null });
    render(<Step cls={kubeadm} first={{ noSharedKey: true }} />);
    const target = screen.getByLabelText("Target");
    expect(target).toHaveValue("store");
    expect(screen.getByText("No snapshots until a key for store is stored")).toBeInTheDocument();
    await user.selectOptions(target, "Backup volume");
    expect(target).toHaveDisplayValue("Backup volume");
    expect(screen.queryByRole("option", { name: /no longer offered/u })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Access key ID")).not.toBeInTheDocument();
    expect(screen.queryByText(/No snapshots until a key/u)).not.toBeInTheDocument();
    await user.selectOptions(target, "store");
    expect(target).toHaveValue("store");
    expect(screen.getByText("No snapshots until a key for store is stored")).toBeInTheDocument();
  });

  it("offers etcd snapshots on, with the fields of the options and one access key", async () => {
    const user = userEvent.setup({ delay: null });
    const { container } = render(<Step cls={kubeadm} />);
    const snapshots = screen.getByLabelText("Take etcd snapshots");
    expect(snapshots).toBeChecked();
    // Backups are off, so every field below is the snapshots' own.
    const inside = screen;
    expect(inside.getByLabelText("Target")).toHaveValue("store");
    expect(inside.getByLabelText("Schedule")).toHaveValue("0 */12 * * *");
    expect(inside.getByLabelText("Snapshots to keep")).toHaveValue(14);
    expect(inside.queryByLabelText("Also take kubeadm snapshots")).not.toBeInTheDocument();
    expect(inside.getByLabelText("Access key ID")).toBeInTheDocument();
    await expectNoViolations(container, "the add-ons step with etcd snapshots on");

    await user.clear(inside.getByLabelText("Snapshots to keep"));
    await user.type(inside.getByLabelText("Snapshots to keep"), "0");
    expect(inside.getByLabelText("Snapshots to keep")).toHaveAccessibleDescription("A whole number from 1 to 1000");

    // Backups take the key into their own block, so it is entered once.
    await user.click(screen.getByLabelText("Back up with Velero"));
    expect(screen.getAllByLabelText("Access key ID")).toHaveLength(1);
    await expectNoViolations(container, "the add-ons step with backups and etcd snapshots on");

    await user.click(screen.getByLabelText("Take etcd snapshots"));
    expect(screen.queryByLabelText("Snapshots to keep")).not.toBeInTheDocument();
    await expectNoViolations(container, "the add-ons step with etcd snapshots off");
  });

  it("offers the distribution's own upload where it has one", async () => {
    const user = userEvent.setup({ delay: null });
    render(<Step cls={classOf("rke2", oneTarget)} />);
    await user.click(screen.getByLabelText("Also take RKE2 snapshots"));
    expect(screen.getByLabelText("Also take RKE2 snapshots")).toBeChecked();
  });

  it("names the folder the access key is limited to", () => {
    render(<Step cls={kubeadm} first={{ backups: true }} />);
    expect(screen.getByText("Limit it to the folder <namespace>.web/")).toBeInTheDocument();
  });

  it("names the folder beneath the target's prefix", () => {
    const prefixed = classOf("kubeadm", JSON.stringify([{ name: "store", bucket: "backups", prefix: "site/a" }]));
    expect(classTargets(prefixed)).toStrictEqual([{ name: "store", bucket: "backups", prefix: "site/a" }]);
    expect(backupCredentialHint("clusters", "web", classTargets(prefixed)[0])).toBe(
      "Limit it to the folder site/a/clusters.web/",
    );
    render(<Step cls={prefixed} first={{ backups: true }} />);
    expect(screen.getByText("Limit it to the folder site/a/<namespace>.web/")).toBeInTheDocument();
  });

  it("makes the access key optional for etcd snapshots alone", () => {
    render(<Step cls={kubeadm} />);
    expect(screen.getByText("Empty uses the key stored for every cluster")).toBeInTheDocument();
  });

  it("offers the backup volume where the class offers no target, for every distribution", () => {
    for (const d of ["rke2", "kubeadm", "k3s", "talos"]) {
      const { unmount } = render(<Step cls={classOf(d, undefined)} first={{ noSharedKey: true }} />);
      expect(screen.getByLabelText("Take etcd snapshots")).toBeInTheDocument();
      expect(screen.getByRole("option", { name: "Backup volume" })).toBeInTheDocument();
      expect(screen.queryByLabelText("Access key ID")).not.toBeInTheDocument();
      expect(screen.queryByText(/No snapshots until a key/u)).not.toBeInTheDocument();
      unmount();
    }
  });

  it("keeps the step's hints within the hint limit", () => {
    const { container, unmount } = render(<Step cls={bare} />);
    const shown = hintTexts(container);
    unmount();
    for (const first of [{ noSharedKey: true }, { noSharedKey: false }]) {
      const { container: c, unmount: done } = render(<Step cls={kubeadm} first={first} />);
      shown.push(...hintTexts(c));
      done();
    }
    const problems = shown.flatMap((text) => check({ file: "step", line: 0, kind: "hint", text }));
    expect(problems.map((p) => `${p.text}: ${p.detail}`)).toStrictEqual([]);
  });

  it("says a key cannot be stored without Sys.Modify", () => {
    render(<Step cls={kubeadm} canTrust={false} />);
    expect(screen.getByText("Storing it needs Sys.Modify on the workload pool")).toBeInTheDocument();
    expect(screen.queryByLabelText("Access key ID")).not.toBeInTheDocument();
  });

  it("does not show them where the class has no etcdSnapshots variable", () => {
    render(<Step cls={withoutVariable} />);
    expect(screen.queryByLabelText("Take etcd snapshots")).not.toBeInTheDocument();
  });
});

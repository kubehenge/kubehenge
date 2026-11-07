import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState, type JSX } from "react";
import { describe, expect, it } from "vitest";

import { AddOnsStep } from "./AddOnsStep";
import { expectNoViolations } from "./axe";
import { newBackupFields } from "./Backups";
import {
  etcdChosen,
  fieldProblems,
  newForm,
  pendingCredentials,
  request,
  summary,
  withBackupChoices,
  type Form,
} from "./create-form";
import { fromCluster } from "./start-from";
import type { Cluster, ClusterClass } from "./types";

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

const filledForm = (c: ClusterClass, patch: Partial<Form> = {}): Form =>
  withBackupChoices({ ...newForm(), name: "web", ...patch }, c, true);

describe("a new cluster's etcd snapshots", () => {
  it("are on when the class offers a target, and the one target is the choice", () => {
    const form = filledForm(kubeadm);
    expect({ chosen: etcdChosen(form, kubeadm) }).toStrictEqual({ chosen: true });
    expect(form.etcdFields.target).toBe("store");
    expect(filledForm(rke2).etcdFields.target).toBe("");
  });

  it("are off and absent where no target or no variable is offered", () => {
    for (const c of [bare, withoutVariable]) {
      const form = filledForm(c);
      expect({ chosen: etcdChosen(form, c) }).toStrictEqual({ chosen: false });
      expect(fieldProblems("Add-ons", form, c)).toStrictEqual({});
      expect(request(form, c).variables).not.toHaveProperty("etcdSnapshots");
      expect(Object.fromEntries(summary(form, c))).not.toHaveProperty("etcd snapshots");
      expect(pendingCredentials(form, c)).toStrictEqual([]);
    }
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
    expect(fieldProblems("Add-ons", form, rke2)).toStrictEqual({ etcdTarget: "Choose a target" });
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
    expect(JSON.stringify(request(form, rke2))).not.toContain("secret");
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
    expect(fieldProblems("Add-ons", etcdOnly, kubeadm)).toStrictEqual({
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
    const form = withBackupChoices({ ...newForm(), backupKey: key }, kubeadm, false);
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

describe("a new cluster started from another", () => {
  const source: Cluster = {
    namespace: "default",
    name: "web",
    class: "kubehenge-rke2",
    distribution: "rke2",
    version: "v1.36.5",
    generation: 1,
    created: "2026-10-01T00:00:00Z",
    deleting: false,
    phase: "Provisioned",
    available: true,
    conditions: [],
    controlPlane: { desired: 3, ready: 3, available: 3, upToDate: 3 },
    workers: { desired: 2, ready: 2, available: 2, upToDate: 2 },
    workerPools: [{ name: "md-0", class: "worker", replicas: 2 }],
    variables: {},
    pool: "/pool/kubehenge/default.web",
    allowed: { change: true, kubeconfig: true },
  };

  it("takes the other's etcd snapshot settings", () => {
    const started = fromCluster(
      {
        ...source,
        variables: {
          etcdSnapshots: {
            enabled: true,
            target: "aws",
            schedule: "0 */6 * * *",
            timeZone: "Europe/Zurich",
            keep: 5,
            distributionUpload: true,
          },
        },
      },
      newForm(),
    );
    expect(started).toMatchObject({ etcdSnapshots: true });
    expect(started.etcdFields).toStrictEqual({
      target: "aws",
      schedule: "0 */6 * * *",
      timeZone: "Europe/Zurich",
      keep: "5",
      upload: true,
    });
    expect(request(withBackupChoices(started, rke2, true), rke2).variables["etcdSnapshots"]).toMatchObject({
      target: "aws",
      keep: 5,
      distributionUpload: true,
    });
  });

  it("takes that they are off, and leaves the default where the other never set them", () => {
    const off = fromCluster({ ...source, variables: { etcdSnapshots: { enabled: false } } }, newForm());
    expect(off).toMatchObject({ etcdSnapshots: false });
    expect(fromCluster(source, newForm())).toMatchObject({
      etcdSnapshots: true,
      etcdFields: newForm().etcdFields,
    });
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
  const [entered, setEntered] = useState<Form>({ ...newForm(), name: "web", ...first });
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

  it("says a key cannot be stored without Sys.Modify", () => {
    render(<Step cls={kubeadm} canTrust={false} />);
    expect(screen.getByText("Storing it needs Sys.Modify on the workload pool")).toBeInTheDocument();
    expect(screen.queryByLabelText("Access key ID")).not.toBeInTheDocument();
  });

  it("does not show them where the class offers no target", () => {
    render(<Step cls={bare} />);
    expect(screen.queryByLabelText("Take etcd snapshots")).not.toBeInTheDocument();
  });
});

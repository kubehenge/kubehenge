import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { namespaceProblem, newRestoreFields, restoreProblems, restoreRequest, restoreScope } from "./backup-runs";
import { backupsText, scheduleProblem, scheduleText } from "./Backups";
import { fieldProblems, newForm, request, summary, withBackupChoices } from "./create-form";
import { applyStaged, fakeServer } from "./fake-server";
import type { ClusterBackups, ClusterClass, RestoreRun } from "./types";
import { browserZone } from "./ui";

const targets = JSON.stringify([
  { name: "store", endpoint: "https://s3.example.org:8333", bucket: "backups", checksums: true },
  { name: "aws", bucket: "aws-backups", region: "eu-central-2", checksums: true },
]);

const k3s: ClusterClass = {
  name: "kubehenge-k3s",
  distribution: "k3s",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+k3s\d+$` },
  annotations: { "kubehenge.dev/backup-targets": targets, "kubehenge.dev/addon-memory": '{"backups":384}' },
  variables: [{ name: "backups", required: false, schema: { type: "object" } }],
};

const cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-k3s",
  distribution: "k3s",
  version: "v1.36.5+k3s1",
  generation: 1,
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { backups: { enabled: false } },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

const backingUp = {
  ...cluster,
  variables: {
    backups: { enabled: true, target: "store", schedule: "30 1 * * 1,4", timeZone: "UTC", retentionDays: 14 },
  },
};

const started = "2026-10-21T01:30:00Z";

const view: ClusterBackups = {
  enabled: true,
  target: "store",
  reachable: true,
  location: "Available",
  lastBackup: started,
  backups: [
    {
      name: "kubehenge-abcde",
      phase: "InProgress",
      scheduled: false,
      by: "dev@pve",
      items: 3,
      total: 40,
      warnings: 0,
      errors: 0,
    },
    {
      name: "velero-scheduled-20261021013000",
      phase: "Completed",
      scheduled: true,
      started,
      expires: "2026-11-04T01:30:00Z",
      items: 40,
      total: 40,
      warnings: 2,
      errors: 0,
    },
  ],
  restores: [
    {
      name: "velero-scheduled-20261021013000-x",
      backup: "velero-scheduled-20261021013000",
      phase: "InProgress",
      by: "dev@pve",
      namespaces: ["shop"],
      volumeData: true,
      started,
      items: 10,
      total: 20,
      warnings: 0,
      errors: 0,
      volumes: 2,
      volumesDone: 1,
      bytesDone: 1024,
      bytesTotal: 4096,
    },
  ],
  sources: [],
  attention: [],
  canBackUp: true,
  canDelete: true,
  canRestore: true,
};

const scheduledBackup = "velero-scheduled-20261021013000";

const finishedRestore: RestoreRun = {
  name: "shop-restore",
  backup: scheduledBackup,
  phase: "PartiallyFailed",
  by: "dev@pve",
  namespaces: ["shop"],
  excludedResources: ["pods"],
  volumeData: false,
  started,
  completed: started,
  items: 20,
  total: 20,
  warnings: 1,
  errors: 2,
  volumes: 0,
  volumesDone: 0,
  bytesDone: 0,
  bytesTotal: 0,
};

// dr reads web's backups, so web's can be restored into it.
const dr = {
  ...backingUp,
  name: "dr",
  pool: "/pool/kubehenge/clusters.dr",
  variables: { backups: { ...backingUp.variables.backups, readFrom: ["web"] } },
};

const signedIn = { "GET /api/session": { body: { user: "dev@pve", csrf: "c", canTrust: true } } };

describe("backup schedules", () => {
  it("say in words when they run", () => {
    expect(scheduleText("0 2 * * *")).toBe("Every day at 02:00");
    expect(scheduleText("30 1 * * 1,4")).toBe("Every Monday, Thursday at 01:30");
    expect(scheduleText("15 * * * *")).toBe("Every hour at minute 15");
    expect(scheduleText("@weekly")).toBe("Every Sunday at 00:00");
    expect(scheduleText("0 2 1 * *")).toBe("By the cron schedule 0 2 1 * *");
    expect(backupsText({ enabled: true, target: "store", timeZone: "Europe/Zurich" })).toBe(
      "Every day at 02:00 (Europe/Zurich), kept 7 days, to store",
    );
    expect(backupsText({ enabled: false })).toBe("Off");
  });

  it("refuse what cron would", () => {
    expect(scheduleProblem("0 2 * * *")).toBeUndefined();
    expect(scheduleProblem("@daily")).toBeUndefined();
    expect(scheduleProblem("")).toMatch(/A schedule/u);
    expect(scheduleProblem("0 2 * *")).toMatch(/Five fields/u);
    expect(scheduleProblem("CRON_TZ=UTC 0 2 * * *")).toMatch(/time zone/u);
    expect(scheduleProblem("{{ .Values }} * * * *")).toMatch(/Five fields/u);
    expect(namespaceProblem([])).toMatch(/at least one/u);
    expect(namespaceProblem(["Shop"])).toMatch(/not a namespace/u);
    expect(namespaceProblem(["shop", "shop"])).toMatch(/twice/u);
    expect(namespaceProblem(["shop", "web"])).toBeUndefined();
  });
});

describe("a new cluster's backups", () => {
  it("go into the request with the target its class offers", () => {
    const form = withBackupChoices({ ...newForm(), name: "web", backups: true }, k3s, true);
    expect(fieldProblems("Add-ons", form, k3s)).toStrictEqual({
      backupTarget: "Choose a target.",
      backupKey: "Both parts of the access key.",
    });
    const chosen = withBackupChoices(
      {
        ...form,
        backupFields: { ...form.backupFields, target: "aws", retention: "30" },
        backupKey: { accessKeyID: "AKIA", secretAccessKey: "secret" },
      },
      k3s,
      true,
    );
    expect(fieldProblems("Add-ons", chosen, k3s)).toStrictEqual({});
    expect(request(chosen, k3s).variables["backups"]).toStrictEqual({
      enabled: true,
      target: "aws",
      schedule: "0 2 * * *",
      timeZone: browserZone(),
      retentionDays: 30,
      volumes: "all",
    });
    expect(JSON.stringify(request(chosen, k3s))).not.toContain("secret");
    expect(Object.fromEntries(summary(chosen, k3s))["Backups"]).toMatch(
      /kept 30 days, to aws, with an access key of its own/u,
    );
    // Without Sys.Modify the cluster takes the key for every cluster.
    const shared = withBackupChoices({ ...chosen, backupKey: {} }, k3s, false);
    expect(shared).toMatchObject({ backupOwnKey: false });
    expect(fieldProblems("Add-ons", shared, k3s)).toStrictEqual({});
  });
});

describe("a cluster's backups", () => {
  it("are switched on in its options, with the key stored apart", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": { body: cluster },
      "GET /api/classes": { body: [k3s] },
      "GET /api/clusters/clusters/web/credentials/backup": { body: { purpose: "backup", set: false, canChange: true } },
      "PATCH /api/clusters/clusters/web": { body: backingUp },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit backups" }));
    const dialog = screen.getByRole("dialog", { name: "Backups" });
    await user.click(within(dialog).getByLabelText("Back up with Velero"));
    expect(within(dialog).getByText("Choose a target.")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.selectOptions(within(dialog).getByLabelText("Target"), "store");
    const schedule = within(dialog).getByLabelText("Schedule");
    expect(schedule).toHaveAccessibleDescription(/Every day at 02:00/u);
    await user.clear(schedule);
    await user.type(schedule, "0 3 * * 0");
    expect(schedule).toHaveAccessibleDescription(/Every Sunday at 03:00/u);
    await user.selectOptions(within(dialog).getByLabelText("Time zone"), "UTC");
    expect(within(dialog).getByText("Not set.")).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: {
        backups: {
          enabled: true,
          target: "store",
          schedule: "0 3 * * 0",
          timeZone: "UTC",
          retentionDays: 7,
          volumes: "all",
        },
      },
    });
  });

  it("are listed, made at once and restored", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/backups");
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": { body: backingUp },
      "GET /api/clusters/clusters/web/backups": { body: view },
      "POST /api/clusters/clusters/web/backups": { status: 201, body: view.backups[0] },
      "POST /api/clusters/clusters/web/restores": { status: 201, body: view.restores[0] },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(
      screen.findByText("Every Monday, Thursday at 01:30 (UTC), kept 14 days, to store"),
    ).resolves.toBeInTheDocument();
    expect(screen.getByText("Reachable")).toBeInTheDocument();
    const scheduled = screen.getByRole("row", { name: /^velero-scheduled-20261021013000 .*By the schedule/u });
    expect(scheduled).toHaveTextContent("By the schedule");
    expect(scheduled).toHaveTextContent("2 warnings.");
    expect(screen.getByRole("button", { name: "Restore kubehenge-abcde" })).toBeDisabled();
    expect(screen.getByText("1 of 2 volumes, 1.0 KiB of 4.0 KiB")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Back up now" }));
    await waitFor(() => expect(calls.map((c) => c.key)).toContain("POST /api/clusters/clusters/web/backups"));

    await user.click(screen.getByRole("button", { name: "Restore velero-scheduled-20261021013000" }));
    const dialog = screen.getByRole("dialog", { name: "Restore velero-scheduled-20261021013000" });
    await user.click(within(dialog).getByLabelText("Chosen namespaces"));
    await user.click(within(dialog).getByRole("button", { name: "Restore" }));
    expect(within(dialog).getByText("Name at least one namespace.")).toBeInTheDocument();
    await user.type(within(dialog).getByLabelText("Namespaces"), "shop, web");
    await user.click(within(dialog).getByRole("button", { name: "Restore" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === "POST /api/clusters/clusters/web/restores")?.body).toStrictEqual({
      backup: "velero-scheduled-20261021013000",
      namespaces: ["shop", "web"],
    });
  });

  it("say what restoring needs of those who may not", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/backups");
    fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": { body: backingUp },
      "GET /api/clusters/clusters/web/backups": {
        body: { ...view, canRestore: false, reachable: false, message: "Velero is being set up in the cluster." },
      },
    });
    render(<App />);
    await expect(
      screen.findByText(/Restoring needs Sys.Modify on pool kubehenge\/clusters.web/u),
    ).resolves.toBeInTheDocument();
    expect(screen.getByText("Velero is being set up in the cluster.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Back up now" })).toBeDisabled();
  });

  it("are deleted once the backup's name is typed", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/backups");
    const deletion = `DELETE /api/clusters/clusters/web/backups/${scheduledBackup}`;
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": { body: backingUp },
      "GET /api/clusters/clusters/web/backups": { body: view },
      [deletion]: { status: 202, body: { ...view.backups[1], deleting: true } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    const remove = await screen.findByRole("button", { name: `Delete ${scheduledBackup}` });
    // One still running is Velero's to finish first.
    expect(screen.getByRole("button", { name: "Delete kubehenge-abcde" })).toBeDisabled();
    await user.click(remove);
    const dialog = screen.getByRole("dialog", { name: `Delete ${scheduledBackup}` });
    const confirm = within(dialog).getByRole("button", { name: "Delete backup" });
    expect(confirm).toBeDisabled();
    await user.type(within(dialog).getByLabelText("Confirm name"), scheduledBackup.slice(0, -1));
    expect(confirm).toBeDisabled();
    await user.type(within(dialog).getByLabelText("Confirm name"), "0");
    await user.click(confirm);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === deletion)?.body).toStrictEqual({ confirm: scheduledBackup });
  });

  it("show other clusters' backups, what needs a look and what a restore left", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/backups");
    const [, scheduled] = view.backups;
    fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": { body: backingUp },
      "GET /api/clusters/clusters/web/backups": {
        body: {
          ...view,
          backups: [
            ...view.backups.slice(0, 1),
            { ...scheduled, deleting: true, deleteErrors: ["the target refused"] },
            { ...scheduled, name: "shop-20261020", from: "shop" },
          ],
          sources: [{ cluster: "shop", phase: "Available", volumeData: false }],
          restores: [finishedRestore],
          attention: [
            {
              severity: "warning",
              section: "backups",
              what: "Backups overdue",
              message: "None has completed since 2026-10-21 01:30 UTC.",
            },
          ],
        },
      },
      "GET /api/clusters/clusters/web/restores/shop-restore/results": {
        body: {
          errors: {
            velero: [],
            cluster: ["no kind Widget in the cluster"],
            namespaces: [{ namespace: "shop", messages: ["persistentvolumeclaims data already exists"] }],
            more: 3,
          },
          warnings: { velero: [], cluster: [], namespaces: [] },
        },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(
      screen.findByText("Backups overdue. None has completed since 2026-10-21 01:30 UTC."),
    ).resolves.toBeInTheDocument();
    expect(screen.getByText("their resources only, not their volumes' data")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Backups of shop" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Restore shop-20261020" })).toBeEnabled();
    // Another cluster's backups are deleted where they are made.
    expect(screen.queryByRole("button", { name: "Delete shop-20261020" })).not.toBeInTheDocument();
    const deleting = screen.getByRole("row", { name: new RegExp(`^${scheduledBackup} .*By the schedule`, "u") });
    expect(deleting).toHaveTextContent("Deleting");
    expect(deleting).toHaveTextContent("Not deleted: the target refused.");
    expect(within(deleting).getByRole("button", { name: `Delete ${scheduledBackup}` })).toBeDisabled();

    const restore = screen.getByRole("row", { name: /^velero-scheduled-20261021013000 shop/u });
    expect(restore).toHaveTextContent("shop; without the volumes' data");
    expect(restore).toHaveTextContent("2 errors, 1 warning. Results lists them.");
    await user.click(within(restore).getByRole("button", { name: "Results of shop-restore" }));
    const dialog = screen.getByRole("dialog", { name: "Results of shop-restore" });
    await expect(within(dialog).findByText("no kind Widget in the cluster")).resolves.toBeInTheDocument();
    expect(
      within(dialog).getByRole("row", { name: "shop persistentvolumeclaims data already exists" }),
    ).toBeInTheDocument();
    expect(within(dialog).getByText("3 more are not shown.")).toBeInTheDocument();
    expect(within(dialog).getByText("None.")).toBeInTheDocument();
  });

  it("are restored into a cluster that reads them, without the data it cannot read", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/backups");
    const [, scheduled] = view.backups;
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters": { body: [backingUp, dr] },
      "GET /api/clusters/clusters/web": { body: backingUp },
      "GET /api/clusters/clusters/web/backups": { body: view },
      "GET /api/clusters/clusters/dr/backups": {
        body: {
          ...view,
          backups: [{ ...scheduled, from: "web" }],
          restores: [],
          sources: [{ cluster: "web", phase: "Available", volumeData: false }],
        },
      },
      "POST /api/clusters/clusters/dr/restores": { status: 201, body: { ...finishedRestore, phase: "New" } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(screen.findByText("Read by")).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: `Restore ${scheduledBackup}` }));
    const dialog = screen.getByRole("dialog", { name: `Restore ${scheduledBackup}` });
    const data = within(dialog).getByLabelText("Restore the volumes' data");
    expect(data).toBeChecked();
    await user.selectOptions(within(dialog).getByLabelText("Into"), "dr");
    await waitFor(() => expect(data).toBeDisabled());
    expect(data).not.toBeChecked();
    expect(data).toHaveAccessibleDescription(/cannot read the volumes' data/u);

    // Without the data the pods stay out, which the server would refuse to restore.
    await user.click(within(dialog).getByText("Kinds of resources"));
    await user.type(within(dialog).getByLabelText("Only these kinds"), "deployments pods");
    await user.click(within(dialog).getByRole("button", { name: "Restore" }));
    expect(within(dialog).getByText("Without the volumes' data, the pods stay out.")).toBeInTheDocument();
    await user.clear(within(dialog).getByLabelText("Only these kinds"));
    await user.type(within(dialog).getByLabelText("Leave out"), "logs");
    await user.click(within(dialog).getByRole("button", { name: "Restore" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === "POST /api/clusters/clusters/dr/restores")?.body).toStrictEqual({
      backup: scheduledBackup,
      excludedNamespaces: ["logs"],
      volumeData: false,
    });
    expect(screen.getByRole("link", { name: "Open it" })).toHaveAttribute("href", "/clusters/clusters/dr/backups");
  });
});

describe("a restore's choices", () => {
  it("go to the server as it takes them", () => {
    const f = newRestoreFields();
    expect(restoreProblems(f)).toStrictEqual({});
    expect(restoreRequest("b", f)).toStrictEqual({ backup: "b" });
    expect(restoreProblems({ ...f, whole: false })).toStrictEqual({ namespaces: "Name at least one namespace." });
    expect(restoreProblems({ ...f, resources: "Deployments" })).toStrictEqual({
      resources: "Deployments is not a kind's name.",
    });
    expect(restoreProblems({ ...f, resources: "deployments, secrets", excludedResources: "secrets" })).toStrictEqual({
      excludedResources: "A kind both restored and left out.",
    });
    expect(
      restoreRequest("b", {
        ...f,
        excludedNamespaces: "kube-public, logs",
        resources: "deployments",
        excludedResources: "secrets",
        volumeData: false,
      }),
    ).toStrictEqual({
      backup: "b",
      excludedNamespaces: ["kube-public", "logs"],
      resources: ["deployments"],
      excludedResources: ["secrets"],
      volumeData: false,
    });
    // What is left out while whole namespaces are chosen is not asked for.
    expect(restoreRequest("b", { ...f, whole: false, namespaces: "shop", excludedNamespaces: "logs" })).toStrictEqual({
      backup: "b",
      namespaces: ["shop"],
    });
    expect(restoreScope(finishedRestore)).toBe("shop; without the volumes' data");
    expect(
      restoreScope({
        ...finishedRestore,
        namespaces: [],
        excludedNamespaces: ["logs"],
        resources: ["deployments"],
        excludedResources: ["secrets"],
        volumeData: true,
      }),
    ).toBe("Every namespace but logs; only deployments; without secrets");
  });
});

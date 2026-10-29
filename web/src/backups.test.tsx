import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { backupsText, namespaceProblem, scheduleProblem, scheduleText } from "./Backups";
import { fieldProblems, newForm, request, summary, withBackupChoices } from "./create-form";
import { applyStaged, fakeServer } from "./fake-server";
import type { ClusterClass } from "./types";
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

const view = {
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
  canBackUp: true,
  canRestore: true,
};

const signedIn = { "GET /api/session": { body: { user: "dev@pve", csrf: "c", canTrust: true } } };

describe("backup schedules", () => {
  it("say in words when they run", () => {
    expect(scheduleText("0 2 * * *")).toBe("Every day at 02:00");
    expect(scheduleText("30 1 * * 1,4")).toBe("Every Monday, Thursday at 01:30");
    expect(scheduleText("15 * * * *")).toBe("Every hour at minute 15");
    expect(scheduleText("@weekly")).toBe("Every Sunday at 00:00");
    expect(scheduleText("0 2 1 * *")).toBe("Cron 0 2 1 * *");
    expect(backupsText({ enabled: true, target: "store", timeZone: "Europe/Zurich" })).toBe(
      "Every day at 02:00 (Europe/Zurich), kept 7 days, to store",
    );
    expect(backupsText({ enabled: false })).toBe("Off");
  });

  it("refuse what cron would", () => {
    expect(scheduleProblem("0 2 * * *")).toBeUndefined();
    expect(scheduleProblem("@daily")).toBeUndefined();
    expect(scheduleProblem("")).toMatch(/Enter a schedule/u);
    expect(scheduleProblem("0 2 * *")).toMatch(/Enter 5 fields/u);
    expect(scheduleProblem("CRON_TZ=UTC 0 2 * * *")).toMatch(/time zone/u);
    expect(scheduleProblem("{{ .Values }} * * * *")).toMatch(/Enter 5 fields/u);
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
      backupTarget: "Choose a target",
      backupKey: "Enter both parts of the access key",
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
    expect(Object.fromEntries(summary(chosen, k3s))["Backups"]).toMatch(/kept 30 days, to aws, own access key/u);
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
    const dialog = screen.getByRole("dialog", { name: "Edit backups" });
    await user.click(within(dialog).getByLabelText("Back up with Velero"));
    expect(within(dialog).getByText("Choose a target")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.selectOptions(within(dialog).getByLabelText("Target"), "store");
    const schedule = within(dialog).getByLabelText("Schedule");
    expect(schedule).toHaveAccessibleDescription(/Every day at 02:00/u);
    await user.clear(schedule);
    await user.type(schedule, "0 3 * * 0");
    expect(schedule).toHaveAccessibleDescription(/Every Sunday at 03:00/u);
    await user.selectOptions(within(dialog).getByLabelText("Time zone"), "UTC");
    expect(within(dialog).getByText("Not set")).toBeInTheDocument();
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
    const scheduled = screen.getByRole("row", { name: /^velero-scheduled-20261021013000 Schedule/u });
    expect(scheduled).toHaveTextContent("Completed2 warnings");
    expect(
      within(scheduled).getByText("velero backup describe velero-scheduled-20261021013000 --details"),
    ).not.toBeVisible();
    expect(screen.getByRole("button", { name: "Restore kubehenge-abcde" })).toBeDisabled();
    expect(screen.getByText("1 of 2 volumes, 1.0 KiB of 4.0 KiB")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Back up now" }));
    await waitFor(() => expect(calls.map((c) => c.key)).toContain("POST /api/clusters/clusters/web/backups"));

    await user.click(screen.getByRole("button", { name: "Restore velero-scheduled-20261021013000" }));
    const dialog = screen.getByRole("dialog", { name: "Restore velero-scheduled-20261021013000" });
    await user.click(within(dialog).getByLabelText("Chosen namespaces"));
    await user.click(within(dialog).getByRole("button", { name: "Restore" }));
    expect(within(dialog).getByText("Enter at least one namespace")).toBeInTheDocument();
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
        body: { ...view, canRestore: false, reachable: false, message: "Setting up Velero." },
      },
    });
    render(<App />);
    await expect(
      screen.findByText(/Restoring needs Sys.Modify on pool kubehenge\/clusters.web/u),
    ).resolves.toBeInTheDocument();
    expect(screen.getByText("Setting up Velero.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Back up now" })).toBeDisabled();
  });
});

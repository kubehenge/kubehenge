import { render, screen, waitFor, within, type BoundFunctions, type queries } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { fieldProblems, newForm, request, summary, type FieldKey } from "./create-form";
import { MachinesStep, type StepProps } from "./CreateSteps";
import { applyStaged, fakeServer } from "./fake-server";
import {
  newFile,
  noNodeCustomisation,
  nodeCustomisationOf,
  nodeCustomisationText,
  nodeCustomisationVariable,
  type NodeCustomisation,
} from "./node-customisation";
import type { ClusterClass } from "./types";

const k3s: ClusterClass = {
  name: "kubehenge-k3s",
  distribution: "k3s",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+k3s\d+$` },
  variables: [
    { name: "storage", required: true, schema: { type: "string" } },
    { name: "nodeCustomisation", required: false, schema: { type: "object" } },
  ],
};

const talos: ClusterClass = { ...k3s, name: "kubehenge-talos", distribution: "talos" };
const without: ClusterClass = { ...k3s, variables: k3s.variables.slice(0, 1) };

const filled: NodeCustomisation = {
  ...noNodeCustomisation,
  files: [
    {
      ...newFile(),
      path: "/etc/audit/rules.d/50-x.rules",
      content: "-w /etc/passwd",
      permissions: "0640",
      roles: "controlPlane",
    },
    { ...newFile(), path: "/etc/token", source: "secret", secretName: "creds", secretKey: "token" },
  ],
  commands: [{ run: "augenrules --load", roles: "controlPlane" }],
  kubeletArgs: [{ name: "event-qps", value: "50" }],
  apiServerArgs: [{ name: "audit-log-maxage", value: "30" }],
  auditConfigMap: "audit",
  webhookSecret: "hook",
  webhookKey: "conf",
};

const form = { ...newForm(), storage: "ceph", nodeCustomisation: filled };

const noProblems = new Map<FieldKey, string>();

function step(
  cls: ClusterClass,
  set: StepProps["set"],
  shown: { attempted?: boolean; form?: typeof form; refusal?: string } = {},
): ReturnType<typeof render> {
  return render(
    <MachinesStep
      form={shown.form ?? form}
      set={set}
      shown={(k) => noProblems.get(k)}
      cls={cls}
      usableStorages={[]}
      placed={false}
      haOffered={false}
      csi={{ offered: false, problem: undefined }}
      snapshots={{ offered: false, problem: undefined }}
      haGroups={false}
      nodes={[]}
      rules={{ haRules: undefined, haRulesError: undefined }}
      attempted={shown.attempted ?? false}
      refusal={shown.refusal}
    />,
  );
}

describe("the nodes' files, commands and flags in the wizard", () => {
  it("go into the request as the variable, each default left out", () => {
    expect(request(form, k3s).variables["nodeCustomisation"]).toStrictEqual({
      files: [
        {
          path: "/etc/audit/rules.d/50-x.rules",
          permissions: "0640",
          content: "-w /etc/passwd",
          roles: ["controlPlane"],
        },
        { path: "/etc/token", permissions: "0644", secret: { name: "creds", key: "token" } },
      ],
      commands: [{ run: "augenrules --load", roles: ["controlPlane"] }],
      kubeletArgs: [{ name: "event-qps", value: "50" }],
      apiServerArgs: [{ name: "audit-log-maxage", value: "30" }],
      auditPolicy: { configMap: "audit" },
      auditWebhook: { secret: "hook", key: "conf" },
    });
  });

  it("stay out of the request when empty or for a class that lacks the variable", () => {
    expect(request({ ...newForm(), storage: "ceph" }, k3s).variables).not.toHaveProperty("nodeCustomisation");
    expect(request(form, without).variables).not.toHaveProperty("nodeCustomisation");
  });

  it("leave out on Talos what it cannot carry", () => {
    expect(request(form, talos).variables["nodeCustomisation"]).toStrictEqual({
      files: [
        {
          path: "/etc/audit/rules.d/50-x.rules",
          permissions: "0640",
          content: "-w /etc/passwd",
          roles: ["controlPlane"],
        },
      ],
      kubeletArgs: [{ name: "event-qps", value: "50" }],
      apiServerArgs: [{ name: "audit-log-maxage", value: "30" }],
    });
  });

  it("keep an inline audit policy that kubectl set", () => {
    const policy = { rules: [{ level: "Metadata" }] };
    const read = nodeCustomisationOf({ auditPolicy: { policy }, kubeletArgs: [{ name: "a", value: "b" }] });
    expect(nodeCustomisationVariable(read, false)).toStrictEqual({
      kubeletArgs: [{ name: "a", value: "b" }],
      auditPolicy: { policy },
    });
    expect(nodeCustomisationVariable({ ...read, auditConfigMap: "other" }, true)).toHaveProperty("auditPolicy", {
      policy,
    });
  });

  it("read back what the variable holds", () => {
    expect(nodeCustomisationOf(nodeCustomisationVariable(filled, false))).toStrictEqual(filled);
    expect(nodeCustomisationOf("not an object")).toStrictEqual(noNodeCustomisation);
  });

  it("show in the summary, only for a class that has them", () => {
    const rows = Object.fromEntries(summary(form, k3s));
    const empty = Object.fromEntries(summary(newForm(), k3s));
    const lacking = Object.fromEntries(summary(form, without));
    const one = nodeCustomisationText({ ...noNodeCustomisation, files: [newFile()] });
    expect(rows["Node customisation"]).toBe("2 files, 1 command, 2 flags, audit");
    expect(empty["Node customisation"]).toBe("None");
    expect(lacking).not.toHaveProperty("Node customisation");
    expect(one).toBe("1 file");
  });

  it("hold the step back with the first row's problem", () => {
    const bad = (patch: Partial<NodeCustomisation>): Record<string, string | undefined> =>
      fieldProblems("Machines", { ...form, nodeCustomisation: { ...noNodeCustomisation, ...patch } }, k3s);
    expect(bad({ files: [{ ...newFile(), path: "etc/x", content: "a" }] })["nodeCustomisation"]).toBe(
      "Start the path with /",
    );
    expect(bad({ files: [{ ...newFile(), path: "/etc/../x", content: "a" }] })["nodeCustomisation"]).toBe(
      "The path cannot contain ..",
    );
    expect(bad({ files: [{ ...newFile(), path: "/etc/x" }] })["nodeCustomisation"]).toBe("Enter the file's text");
    expect(
      bad({ files: [{ ...newFile(), path: "/x", source: "secret", secretName: "Bad", secretKey: "k" }] })[
        "nodeCustomisation"
      ],
    ).toMatch(/^Secret: Use lower-case/u);
    expect(bad({ commands: [{ run: " ", roles: "both" }] })["nodeCustomisation"]).toBe("Enter a command");
    expect(bad({ commands: [{ run: "x".repeat(1025), roles: "both" }] })["nodeCustomisation"]).toBe(
      "At most 1024 characters",
    );
    expect(bad({ kubeletArgs: [{ name: "--event-qps", value: "1" }] })["nodeCustomisation"]).toMatch(/without dashes/u);
    expect(
      bad({
        schedulerArgs: [
          { name: "a", value: "" },
          { name: "a", value: "" },
        ],
      })["nodeCustomisation"],
    ).toBe("Each flag once");
    expect(
      bad({ files: Array.from({ length: 11 }, () => ({ ...newFile(), path: "/x", content: "a" })) })[
        "nodeCustomisation"
      ],
    ).toBe("At most 10 files");
    expect(bad({ auditConfigMap: "Bad_Name" })["nodeCustomisation"]).toMatch(/lower-case/u);
    expect(fieldProblems("Machines", form, k3s)).not.toHaveProperty("nodeCustomisation");
    expect(
      fieldProblems(
        "Machines",
        { ...form, nodeCustomisation: { ...noNodeCustomisation, files: [newFile()] } },
        without,
      ),
    ).not.toHaveProperty("nodeCustomisation");
  });

  it("refuse on Talos a Secret file, which the request would leave out", () => {
    const secret = { ...noNodeCustomisation, files: [{ ...newFile(), path: "/x", source: "secret" as const }] };
    expect(fieldProblems("Machines", { ...form, nodeCustomisation: secret }, talos)["nodeCustomisation"]).toBe(
      "Talos takes inline text only",
    );
  });

  it("are edited under Advanced on the machines step, collapsed at first", async () => {
    const set = vi.fn<StepProps["set"]>();
    step(k3s, set);
    const details = screen.getByText("Advanced").closest("details");
    expect(details).not.toHaveAttribute("open");
    await userEvent.setup({ delay: null }).click(screen.getByText("Advanced"));
    const file = screen.getByRole("group", { name: "File 1" });
    await userEvent.setup({ delay: null }).type(within(file).getByLabelText("Path"), "x");
    expect(set).toHaveBeenLastCalledWith("nodeCustomisation", {
      ...filled,
      files: [{ ...filled.files[0], path: "/etc/audit/rules.d/50-x.rulesx" }, filled.files[1]],
    });
  });

  it("add a file, a command and a flag", async () => {
    const set = vi.fn<StepProps["set"]>();
    step(k3s, set, { form: { ...form, nodeCustomisation: noNodeCustomisation } });
    const user = userEvent.setup({ delay: null });
    await user.click(within(screen.getByRole("group", { name: "Files" })).getByRole("button", { name: "Add file" }));
    expect(set).toHaveBeenLastCalledWith("nodeCustomisation", { ...noNodeCustomisation, files: [newFile()] });
    await user.click(
      within(screen.getByRole("group", { name: "Commands" })).getByRole("button", { name: "Add command" }),
    );
    expect(set).toHaveBeenLastCalledWith("nodeCustomisation", {
      ...noNodeCustomisation,
      commands: [{ run: "", roles: "both" }],
    });
    await user.click(
      within(screen.getByRole("group", { name: "Scheduler flags" })).getByRole("button", { name: "Add flag" }),
    );
    expect(set).toHaveBeenLastCalledWith("nodeCustomisation", {
      ...noNodeCustomisation,
      schedulerArgs: [{ name: "", value: "" }],
    });
  });

  it("are left off the step for a class without the variable", () => {
    step(without, vi.fn<StepProps["set"]>());
    expect(screen.queryByText("Advanced")).not.toBeInTheDocument();
  });

  it("hide on Talos the commands, the Secret source and the audit settings, and say so", () => {
    step(talos, vi.fn<StepProps["set"]>());
    expect(screen.getByText("Talos takes inline files and flags only.")).toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "Commands" })).not.toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "Audit" })).not.toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "Secret" })).not.toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Kubelet flags" })).toBeInTheDocument();
  });

  it("show the commands, the Secret source and the audit settings elsewhere", () => {
    step(k3s, vi.fn<StepProps["set"]>());
    expect(screen.queryByText(/^Talos takes/u)).not.toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Commands" })).toBeInTheDocument();
    expect(screen.getByLabelText("Policy ConfigMap")).toHaveValue("audit");
    expect(screen.getByLabelText("Webhook Secret")).toHaveValue("hook");
    expect(screen.getAllByRole("option", { name: "Secret" }).length).toBeGreaterThan(0);
  });

  it("show every problem once Next was pressed, and before only the touched row's", () => {
    const bad = { ...form, nodeCustomisation: { ...noNodeCustomisation, files: [{ ...newFile(), path: "etc/x" }] } };
    const { unmount } = step(k3s, vi.fn<StepProps["set"]>(), { form: bad });
    expect(screen.queryByText("Start the path with /")).not.toBeInTheDocument();
    unmount();
    step(k3s, vi.fn<StepProps["set"]>(), { form: bad, attempted: true });
    expect(screen.getByText("Start the path with /")).toBeInTheDocument();
    expect(screen.getByLabelText("Path")).toBeInvalid();
  });

  it("show the server's refusal with the section and open it", () => {
    step(k3s, vi.fn<StepProps["set"]>(), { refusal: "Node customisation: /etc/kubernetes is protected" });
    expect(within(screen.getByRole("alert")).getByText(/\/etc\/kubernetes is protected/u)).toBeVisible();
    expect(screen.getByText("Advanced").closest("details")).toHaveAttribute("open");
  });
});

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
  variables: { oidc: [] },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

const path = "/api/clusters/clusters/web";

async function dialogOf(
  shown: object,
  refusal?: { status: number; body: unknown },
): Promise<{
  calls: ReturnType<typeof fakeServer>;
  dialog: BoundFunctions<typeof queries>;
  user: ReturnType<typeof userEvent.setup>;
}> {
  window.history.replaceState(null, "", "/clusters/clusters/web/options");
  const calls = fakeServer({
    "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
    [`GET ${path}`]: { body: shown },
    [`PATCH ${path}`]: refusal ?? { body: shown },
  });
  const user = userEvent.setup({ delay: null });
  render(<App />);
  await user.click(await screen.findByRole("button", { name: "Edit node customisation" }));
  return { calls, dialog: within(screen.getByRole("dialog", { name: "Edit node customisation" })), user };
}

describe("a cluster's node customisation in its options", () => {
  it("lists the row with what is set", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      [`GET ${path}`]: {
        body: { ...cluster, variables: { oidc: [], nodeCustomisation: nodeCustomisationVariable(filled, false) } },
      },
    });
    render(<App />);
    await expect(screen.findByRole("row", { name: /Node customisation/u })).resolves.toHaveTextContent(
      "2 files, 1 command, 2 flags, audit",
    );
  });

  it("saves the variable, staged with the plan that replaces every machine", async () => {
    const { calls, dialog, user } = await dialogOf(cluster);
    expect(dialog.getByText("Saving replaces every node, one at a time.")).toBeVisible();
    expect(dialog.getByRole("button", { name: "Save" })).toBeDisabled();
    await user.click(dialog.getByRole("button", { name: "Add file" }));
    const file = within(screen.getByRole("group", { name: "File 1" }));
    await user.type(file.getByLabelText("Path"), "/etc/audit/rules.d/50-x.rules");
    await user.type(file.getByLabelText("File 1 text"), "-w /etc/passwd");
    await user.selectOptions(file.getByLabelText("File 1 mode"), "0640");
    await user.selectOptions(file.getByLabelText("Nodes"), "controlPlane");
    await user.click(dialog.getByRole("button", { name: "Add command" }));
    await user.type(screen.getByLabelText("Command"), "augenrules --load");
    await user.click(
      within(dialog.getByRole("group", { name: "Kubelet flags" })).getByRole("button", { name: "Add flag" }),
    );
    await user.type(screen.getByLabelText("Name"), "event-qps");
    await user.type(screen.getByLabelText("Value"), "50");
    await user.type(dialog.getByLabelText("Policy ConfigMap"), "audit");
    await user.click(dialog.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    await user.click(await screen.findByRole("button", { name: "Review plan" }));
    expect(
      within(screen.getByRole("dialog", { name: "Review plan" })).getByRole("row", { name: /^Node customisation/u }),
    ).toHaveTextContent("Replaces every machine one by one");
    await user.click(
      within(screen.getByRole("dialog", { name: "Review plan" })).getByRole("button", { name: "Apply" }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === `PATCH ${path}`)?.body).toStrictEqual({
      variables: {
        nodeCustomisation: {
          files: [
            {
              path: "/etc/audit/rules.d/50-x.rules",
              permissions: "0640",
              content: "-w /etc/passwd",
              roles: ["controlPlane"],
            },
          ],
          commands: [{ run: "augenrules --load" }],
          kubeletArgs: [{ name: "event-qps", value: "50" }],
          auditPolicy: { configMap: "audit" },
        },
      },
    });
  });

  it("refuses to save a file with a relative path, and shows why", async () => {
    const { calls, dialog, user } = await dialogOf(cluster);
    await user.click(dialog.getByRole("button", { name: "Add file" }));
    await user.type(screen.getByLabelText("Path"), "etc/x");
    await user.type(screen.getByLabelText("File 1 text"), "a");
    await user.click(dialog.getByRole("button", { name: "Save" }));
    await expect(screen.findByText("Start the path with /")).resolves.toBeVisible();
    expect(screen.getByRole("dialog", { name: "Edit node customisation" })).toBeVisible();
    expect(calls.map((c) => c.key)).not.toContain(`PATCH ${path}`);
  });

  it("shows the server's refusal in the dialog", async () => {
    const { dialog, user } = await dialogOf(cluster, {
      status: 422,
      body: { error: "The file /etc/kubernetes/x is protected" },
    });
    await user.click(dialog.getByRole("button", { name: "Add file" }));
    await user.type(screen.getByLabelText("Path"), "/etc/kubernetes/x");
    await user.type(screen.getByLabelText("File 1 text"), "a");
    await user.click(dialog.getByRole("button", { name: "Save" }));
    await expect(dialog.findByRole("alert")).resolves.toHaveTextContent("/etc/kubernetes/x is protected");
    expect(screen.getByRole("dialog", { name: "Edit node customisation" })).toBeVisible();
  });

  it("clears the variable when everything is removed", async () => {
    const set = {
      ...cluster,
      variables: { oidc: [], nodeCustomisation: { kubeletArgs: [{ name: "a", value: "b" }] } },
    };
    const { calls, dialog, user } = await dialogOf(set);
    await user.click(dialog.getByRole("button", { name: "Remove kubelet flags 1" }));
    await user.click(dialog.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.findLast((c) => c.key === `PATCH ${path}`)?.body).toStrictEqual({
      variables: { nodeCustomisation: {} },
    });
  });

  it("hides on Talos what it cannot carry, and keeps an inline policy as it is", async () => {
    const policy = { rules: [{ level: "Metadata" }] };
    const set = {
      ...cluster,
      class: "kubehenge-talos",
      distribution: "talos",
      variables: { oidc: [], nodeCustomisation: { auditPolicy: { policy } } },
    };
    const { calls, dialog, user } = await dialogOf(set);
    expect(dialog.getByText("Inline policy set with kubectl")).toBeVisible();
    expect(dialog.queryByRole("group", { name: "Commands" })).not.toBeInTheDocument();
    expect(dialog.queryByLabelText("Policy ConfigMap")).not.toBeInTheDocument();
    expect(dialog.queryByLabelText("Webhook Secret")).not.toBeInTheDocument();
    expect(dialog.getByText("Talos takes inline files and flags only.")).toBeVisible();
    await user.click(
      within(dialog.getByRole("group", { name: "Kubelet flags" })).getByRole("button", { name: "Add flag" }),
    );
    await user.type(screen.getByLabelText("Name"), "event-qps");
    await user.click(dialog.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.findLast((c) => c.key === `PATCH ${path}`)?.body).toStrictEqual({
      variables: {
        nodeCustomisation: { kubeletArgs: [{ name: "event-qps", value: "" }], auditPolicy: { policy } },
      },
    });
  });
});

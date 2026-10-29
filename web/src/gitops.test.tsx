import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { addonMemory, fieldProblems, newForm, request, summary } from "./create-form";
import { applyStaged, fakeServer } from "./fake-server";
import { GitOpsFields, gitOpsOf, gitOpsProblems, gitOpsVariable, newGitOps, type GitOpsChoice } from "./GitOps";
import type { Cluster, ClusterClass } from "./types";

const repo = "https://git.example.org/platform/clusters.git";

const choice = (over: Partial<GitOpsChoice>): GitOpsChoice => ({
  ...newGitOps(),
  engine: "argocd",
  url: repo,
  ...over,
});

describe("the GitOps settings", () => {
  it("write the variable the class takes, and read it back", () => {
    const g = choice({ path: "clusters/web", hostname: "argo.example.org", admins: "platform, ops" });
    const v = gitOpsVariable(g);
    expect(v).toStrictEqual({
      enabled: true,
      engine: "argocd",
      repository: { url: repo, revision: "main", path: "clusters/web" },
      auth: "none",
      hostname: "argo.example.org",
      admins: ["platform", "ops"],
      signIn: { clientSecret: "none" },
    });
    expect(gitOpsOf(v)).toStrictEqual({ ...g, admins: "platform, ops" });
    // Switched off, what the cluster had stays, for switching on again.
    expect(gitOpsVariable({ ...g, engine: "" }, { enabled: true, repository: { url: repo } })).toStrictEqual({
      enabled: false,
      repository: { url: repo },
    });
    expect(gitOpsVariable({ ...g, engine: "" })).toBeUndefined();
    expect(gitOpsVariable(choice({ engine: "flux", ui: true, admins: "ops" }))).not.toHaveProperty("admins");
  });

  it("hold the repository's URL to the way it is read", () => {
    expect(gitOpsProblems(choice({}))).toStrictEqual({});
    expect(gitOpsProblems(choice({ url: "git@git.example.org:a/b.git" })).url).toMatch(/https:\/\/ or ssh:\/\//u);
    expect(gitOpsProblems(choice({ url: "https://me:t0ken@git.example.org/a.git" })).url).toMatch(
      /token out of the URL/u,
    );
    expect(gitOpsProblems(choice({ auth: "deployKey" })).url).toMatch(/deploy key/u);
    expect(gitOpsProblems(choice({ url: "ssh://git@git.example.org/a.git" })).url).toMatch(/https:\/\//u);
    expect(gitOpsProblems(choice({ path: "../other" })).path).toBeDefined();
    expect(gitOpsProblems(choice({ auth: "existingSecret" })).existingSecret).toBeDefined();
    expect(gitOpsProblems(choice({ engine: "flux", ui: true })).ui).toMatch(/host name and a client secret/u);
    expect(gitOpsProblems(choice({ engine: "flux", ui: true, clientSecret: "stored" })).ui).toBe(
      "Flux's web interface needs a host name",
    );
    expect(
      gitOpsProblems(choice({ engine: "flux", ui: true, hostname: "flux.example.org", clientSecret: "stored" })),
    ).toStrictEqual({});
  });

  it("give Flux's web interface the stored client secret in place of a public client", async () => {
    const changes: GitOpsChoice[] = [];
    const user = userEvent.setup({ delay: null });
    render(
      <GitOpsFields
        value={choice({ engine: "flux" })}
        onChange={(g) => {
          changes.push(g);
        }}
      />,
    );
    await user.click(screen.getByRole("checkbox", { name: "Serve Flux's web interface" }));
    expect(changes.at(-1)).toMatchObject({ ui: true, clientSecret: "stored" });
  });

  it("go into the wizard's request, summary and memory", () => {
    const cls: ClusterClass = {
      name: "kubehenge-rke2",
      distribution: "rke2",
      annotations: { "kubehenge.dev/addon-memory": '{"argocd":512,"flux":384}' },
      versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
      variables: [
        { name: "storage", required: true, schema: { type: "string" } },
        { name: "gitops", required: false, schema: { type: "object" } },
      ],
    };
    const form = { ...newForm(), rancherRegister: false, gitops: choice({ engine: "flux" }) };
    expect(request(form, cls).variables["gitops"]).toMatchObject({ enabled: true, engine: "flux" });
    expect(request({ ...form, gitops: newGitOps() }, cls).variables).not.toHaveProperty("gitops");
    expect(Object.fromEntries(summary(form, cls))).toMatchObject({ GitOps: `Flux from ${repo} (main)` });
    expect(addonMemory(form, cls)).toStrictEqual({ names: ["Flux"], mib: 384, controlPlaneMiB: 0 });
    expect(fieldProblems("Add-ons", { ...form, gitops: choice({ url: "" }) }, cls)).toHaveProperty("gitops.url");
    expect(fieldProblems("Add-ons", { ...form, gitops: choice({ url: "" }) }, { ...cls, variables: [] })).toStrictEqual(
      {},
    );
  });
});

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
  variables: {
    gitops: {
      enabled: true,
      engine: "flux",
      auth: "deployKey",
      repository: { url: "ssh://git@git.example.org/platform/clusters.git", revision: "main", path: "." },
    },
  },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
  gitops: {
    deployKey: "ssh-ed25519 AAAAC3Nza kubehenge clusters/web",
    deployKeyFingerprint: "SHA256:abc",
    hostKeys: ["ssh-ed25519 SHA256:def"],
  },
};

describe("a cluster's GitOps options", () => {
  it("show the deploy key to copy and save a change", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
      "GET /api/clusters/clusters/web/credentials/gitops": { body: { purpose: "gitops", set: false, canChange: true } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(
      screen.findByRole("cell", { name: "Flux from ssh://git@git.example.org/platform/clusters.git (main)" }),
    ).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Edit GitOps" }));
    const dialog = screen.getByRole("dialog", { name: "Edit GitOps" });
    expect(within(dialog).getByLabelText("Deploy key")).toHaveTextContent(
      "ssh-ed25519 AAAAC3Nza kubehenge clusters/web",
    );
    expect(within(dialog).getByText("ssh-ed25519 SHA256:def")).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Copy deploy key" }));
    await expect(navigator.clipboard.readText()).resolves.toBe("ssh-ed25519 AAAAC3Nza kubehenge clusters/web");
    await expect(within(dialog).findByRole("button", { name: "Copied" })).resolves.toBeInTheDocument();

    const save = within(dialog).getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    await user.clear(within(dialog).getByLabelText("Path"));
    await user.type(within(dialog).getByLabelText("Path"), "clusters/web");
    await user.selectOptions(within(dialog).getByLabelText("Access"), "token");
    expect(save).toBeDisabled();
    await expect(within(dialog).findByRole("group", { name: "Repository token" })).resolves.toBeInTheDocument();
    await user.selectOptions(within(dialog).getByLabelText("Access"), "deployKey");
    await user.click(save);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: {
        gitops: {
          enabled: true,
          engine: "flux",
          auth: "deployKey",
          repository: {
            url: "ssh://git@git.example.org/platform/clusters.git",
            revision: "main",
            path: "clusters/web",
          },
          signIn: { clientSecret: "none" },
        },
      },
    });
  });

  it("let only those who may take the kubeconfig choose a repository", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: { ...cluster, allowed: { change: true, kubeconfig: false } } },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit GitOps" }));
    const dialog = screen.getByRole("dialog", { name: "Edit GitOps" });
    expect(within(dialog).getByLabelText("URL")).toBeDisabled();
    expect(within(dialog).getByRole("option", { name: "Argo CD (recommended)" })).toBeDisabled();
    expect(within(dialog).getByText(/needs Sys.Modify/u)).toBeInTheDocument();
    await user.selectOptions(within(dialog).getByLabelText("GitOps"), "");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toMatchObject({
      variables: { gitops: { enabled: false, engine: "flux" } },
    });
  });
});

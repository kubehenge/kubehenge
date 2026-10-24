import { useState, type JSX } from "react";

import { CredentialField } from "./CredentialField";
import { list } from "./network";
import type { Cluster, GitOpsKeys } from "./types";
import { Dialog, ErrorBox, Field, filled, useChange } from "./ui";
import { flag, oneOf, record, text, texts } from "./values";

/** The engines a cluster chooses from, by their names in its gitops variable; "" is none. */
export const engines = ["", "argocd", "flux"] as const;
export type Engine = (typeof engines)[number];
export const repoAuths = ["none", "deployKey", "token", "existingSecret"] as const;
export type RepoAuth = (typeof repoAuths)[number];
export const clientSecrets = ["none", "stored", "existingSecret"] as const;
export type ClientSecret = (typeof clientSecrets)[number];

/** A cluster's gitops variable as the interface edits it. */
export interface GitOpsChoice {
  engine: Engine;
  url: string;
  revision: string;
  path: string;
  auth: RepoAuth;
  existingSecret: string;
  knownHosts: string;
  hostname: string;
  admins: string;
  clientSecret: ClientSecret;
  clientSecretName: string;
  ui: boolean;
}

export const newGitOps = (): GitOpsChoice => ({
  engine: "",
  url: "",
  revision: "main",
  path: ".",
  auth: "none",
  existingSecret: "",
  knownHosts: "",
  hostname: "",
  admins: "",
  clientSecret: "none",
  clientSecretName: "",
  ui: false,
});

// engineOf is the engine a switched-on variable names, Argo CD unless it is Flux, as the schema's default.
const engineOf = (e: string | undefined): Engine => (e === "flux" ? "flux" : "argocd");

export function gitOpsOf(v: unknown): GitOpsChoice {
  const r = record(v);
  const repo = record(r["repository"]);
  const signIn = record(r["signIn"]);
  const engine = text(r["engine"]);
  const auth = text(r["auth"]);
  const secret = text(signIn["clientSecret"]);
  return {
    engine: flag(r["enabled"]) === true ? engineOf(engine) : "",
    url: text(repo["url"]) ?? "",
    revision: text(repo["revision"]) ?? "main",
    path: text(repo["path"]) ?? ".",
    auth: oneOf(repoAuths, auth) ? auth : "none",
    existingSecret: text(r["existingSecret"]) ?? "",
    knownHosts: text(r["knownHosts"]) ?? "",
    hostname: text(r["hostname"]) ?? "",
    admins: (texts(r["admins"]) ?? []).join(", "),
    clientSecret: oneOf(clientSecrets, secret) ? secret : "none",
    clientSecretName: text(signIn["existingSecret"]) ?? "",
    ui: flag(r["ui"]) === true,
  };
}

/**
 * gitOpsVariable is the variable a choice sets: undefined for none, or with held, what the cluster
 * had, switched off and kept for switching on again.
 */
export function gitOpsVariable(g: GitOpsChoice, held?: Record<string, unknown>): Record<string, unknown> | undefined {
  if (g.engine === "") {
    return held === undefined ? undefined : { ...held, enabled: false };
  }
  const v: Record<string, unknown> = {
    enabled: true,
    engine: g.engine,
    repository: { url: g.url.trim(), revision: g.revision.trim() || "main", path: g.path.trim() || "." },
    auth: g.auth,
  };
  if (g.auth === "existingSecret") {
    v["existingSecret"] = g.existingSecret.trim();
  }
  if (g.url.startsWith("ssh://") && filled(g.knownHosts.trim())) {
    v["knownHosts"] = `${g.knownHosts.trim()}\n`;
  }
  if (filled(g.hostname)) {
    v["hostname"] = g.hostname;
  }
  const admins = list(g.admins);
  if (g.engine === "argocd" && admins.length > 0) {
    v["admins"] = admins;
  }
  const signIn: Record<string, unknown> = { clientSecret: g.clientSecret };
  if (g.clientSecret === "existingSecret") {
    signIn["existingSecret"] = g.clientSecretName.trim();
  }
  v["signIn"] = signIn;
  if (g.engine === "flux" && g.ui) {
    v["ui"] = true;
  }
  return v;
}

const engineName = (e: Engine): string => (e === "flux" ? "Flux" : "Argo CD");

/** gitOpsText says in a line which engine syncs the cluster, from where. */
export function gitOpsText(g: GitOpsChoice): string {
  if (g.engine === "") {
    return "Off";
  }
  const at = g.path === "." || g.path === "" ? "" : `, ${g.path}`;
  const web = filled(g.hostname) && (g.engine === "argocd" || g.ui) ? `; web interface at ${g.hostname}` : "";
  return `${engineName(g.engine)}, syncing ${g.url} (${g.revision}${at})${web}`;
}

/** The fields of a GitOps choice a problem belongs to. */
export const gitOpsFields = ["url", "revision", "path", "existingSecret", "admins", "clientSecretName", "ui"] as const;
export type GitOpsField = (typeof gitOpsFields)[number];

const refPattern = /^[A-Za-z0-9._/-]+$/u;

// urlProblem says what is wrong with the repository's URL, for the way it is read.
function urlProblem(g: GitOpsChoice): string | undefined {
  const url = g.url.trim();
  if (!/^(?:https|ssh):\/\/\S+$/u.test(url)) {
    return "An https:// or ssh:// URL.";
  }
  if (/^https:\/\/[^/]*@/u.test(url)) {
    return "No token in the URL: choose Token and store it in the cluster's credentials.";
  }
  if (g.auth === "deployKey" && !url.startsWith("ssh://")) {
    return "A deploy key reads the repository over ssh://.";
  }
  if ((g.auth === "none" || g.auth === "token") && !url.startsWith("https://")) {
    return "Without a key, the repository is read over https://.";
  }
  return undefined;
}

/** gitOpsProblems says, by field, what is wrong with a choice; the server checks the same. */
export function gitOpsProblems(g: GitOpsChoice): Partial<Record<GitOpsField, string>> {
  const p: Partial<Record<GitOpsField, string>> = {};
  if (g.engine === "") {
    return p;
  }
  const url = urlProblem(g);
  if (url !== undefined) {
    p.url = url;
  }
  if (!refPattern.test(g.revision.trim() || "main")) {
    p.revision = "A branch or a ref, such as main or refs/tags/v1.";
  }
  const path = g.path.trim() || ".";
  if (!refPattern.test(path) || /(?:^|\/)\.\.(?:\/|$)/u.test(path)) {
    p.path = "A directory inside the repository, such as clusters/web.";
  }
  if (g.auth === "existingSecret" && !filled(g.existingSecret.trim())) {
    p.existingSecret = "The Secret's name.";
  }
  if (g.clientSecret === "existingSecret" && !filled(g.clientSecretName.trim())) {
    p.clientSecretName = "The Secret's name.";
  }
  if (g.engine === "flux" && g.ui) {
    const missing = [
      ...(filled(g.hostname) ? [] : ["a host name"]),
      ...(g.clientSecret === "none" ? ["a client secret to sign in with"] : []),
    ];
    if (missing.length > 0) {
      p.ui = `Flux's web interface needs ${missing.join(" and ")}.`;
    }
  }
  return p;
}

const engineOptions: { value: Engine; label: string }[] = [
  { value: "", label: "Off" },
  { value: "argocd", label: "Argo CD (recommended)" },
  { value: "flux", label: "Flux" },
];

const authOptions: { value: RepoAuth; label: string }[] = [
  { value: "none", label: "Public, over HTTPS" },
  { value: "deployKey", label: "Deploy key, over SSH (recommended for a private repository)" },
  { value: "token", label: "Token, over HTTPS" },
  { value: "existingSecret", label: "A Secret of your own" },
];

const clientSecretOptions: { value: ClientSecret; label: string }[] = [
  { value: "none", label: "None: a public client, with PKCE (recommended)" },
  { value: "stored", label: "Stored in Kubehenge" },
  { value: "existingSecret", label: "A Secret of your own" },
];

export const gitOpsHint =
  "Keeps the cluster in step with a directory of a Git repository: what it holds is applied, and what goes from it is removed. Switching it off, or to the other engine, leaves what it deployed running.";

/** gitOpsTrustHint says why switching GitOps on needs more than creating a cluster. */
export const gitOpsTrustHint =
  "The engine applies whatever the repository holds with cluster-wide rights, so choosing the repository needs Sys.Modify, as taking the admin kubeconfig does.";

const engineHint = (e: Engine): string =>
  e === "flux"
    ? "Flux, installed by the Flux Operator. Its web interface is optional."
    : "Argo CD, with a web interface that signs in through the cluster's first identity provider; its local admin stays disabled.";

const authHint = (a: RepoAuth, engine: Engine): string => {
  switch (a) {
    case "deployKey": {
      return "Kubehenge makes a key pair for this cluster and shows its public half in the cluster's options, to add to the repository as a read-only deploy key.";
    }
    case "token": {
      return "A user name and a read-only token, stored in the cluster's options once it exists; until then the engine waits for it.";
    }
    case "existingSecret": {
      return engine === "flux"
        ? "A Secret in flux-system that Flux reads the repository with: identity and known_hosts, or username and password."
        : "A repository Secret of your own in argocd, labelled argocd.argoproj.io/secret-type=repository, for this URL.";
    }
    case "none": {
      break;
    }
  }
  return "Anyone may read the repository.";
};

const uiHint =
  "Shows every Flux object and the workloads they deployed, and lets those signed in reconcile, suspend and resume them, with the rights the cluster's RBAC gives them. Off by default; it signs in through the cluster's first identity provider with a client secret.";

const noProblems: Partial<Record<GitOpsField, string>> = {};

/** GitOpsFields chooses the engine, the repository and how the web interface signs in. */
export function GitOpsFields({
  value: g,
  onChange,
  problems = noProblems,
  locked = false,
}: {
  value: GitOpsChoice;
  onChange: (g: GitOpsChoice) => void;
  problems?: Partial<Record<GitOpsField, string | undefined>>;
  /** Only switching off is open, without the right to choose a repository. */
  locked?: boolean;
}): JSX.Element {
  const set = <K extends keyof GitOpsChoice>(k: K, v: GitOpsChoice[K]): void => onChange({ ...g, [k]: v });
  const on = g.engine !== "";
  return (
    <>
      <Field label="GitOps" hint={on ? engineHint(g.engine) : gitOpsHint}>
        {(id) => (
          <select
            id={id}
            value={g.engine}
            onChange={(e) => {
              const v = e.target.value;
              if (oneOf(engines, v)) {
                set("engine", v);
              }
            }}
          >
            {engineOptions.map((o) => (
              <option key={o.value} value={o.value} disabled={locked && o.value !== "" && o.value !== g.engine}>
                {o.label}
              </option>
            ))}
          </select>
        )}
      </Field>
      {on && (
        <fieldset className="group" disabled={locked}>
          <legend>Repository</legend>
          <Field label="URL" hint="https://host/path, or ssh://user@host/path for a deploy key." error={problems.url}>
            {(id) => (
              <input id={id} spellCheck={false} value={g.url} onChange={(e) => set("url", e.target.value.trim())} />
            )}
          </Field>
          <Field label="Branch or ref" hint="A branch, or a full ref such as refs/tags/v1." error={problems.revision}>
            {(id) => (
              <input
                id={id}
                spellCheck={false}
                value={g.revision}
                onChange={(e) => set("revision", e.target.value.trim())}
              />
            )}
          </Field>
          <Field
            label="Path"
            hint="The directory synced, with plain manifests or a kustomization; . is the repository's root."
            error={problems.path}
          >
            {(id) => (
              <input id={id} spellCheck={false} value={g.path} onChange={(e) => set("path", e.target.value.trim())} />
            )}
          </Field>
          <Field label="Access" hint={authHint(g.auth, g.engine)}>
            {(id) => (
              <select
                id={id}
                value={g.auth}
                onChange={(e) => {
                  const v = e.target.value;
                  if (oneOf(repoAuths, v)) {
                    set("auth", v);
                  }
                }}
              >
                {authOptions.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            )}
          </Field>
          {g.auth === "existingSecret" && (
            <Field label="Secret" error={problems.existingSecret}>
              {(id) => (
                <input
                  id={id}
                  spellCheck={false}
                  value={g.existingSecret}
                  onChange={(e) => set("existingSecret", e.target.value.trim())}
                />
              )}
            </Field>
          )}
          {g.url.startsWith("ssh://") && (
            <Field
              label="SSH host keys"
              hint="Optional: the server's keys as known_hosts lines. Left empty, Kubehenge reads them from the server once and trusts them from then on; their fingerprints show in the cluster's options to compare."
            >
              {(id) => (
                <textarea
                  id={id}
                  rows={2}
                  spellCheck={false}
                  value={g.knownHosts}
                  onChange={(e) => set("knownHosts", e.target.value)}
                />
              )}
            </Field>
          )}
        </fieldset>
      )}
      {on && (
        <fieldset className="group" disabled={locked}>
          <legend>Web interface</legend>
          {g.engine === "flux" && (
            <>
              <label className="check">
                <input
                  type="checkbox"
                  checked={g.ui}
                  aria-describedby="flux-ui-hint"
                  onChange={(e) => {
                    // Flux's web interface signs in only with a client secret, so a public client
                    // gives way to the stored one.
                    const ui = e.target.checked;
                    onChange({ ...g, ui, clientSecret: ui && g.clientSecret === "none" ? "stored" : g.clientSecret });
                  }}
                />{" "}
                Serve Flux&apos;s web interface
              </label>
              <p className="hint" id="flux-ui-hint">
                {uiHint}
              </p>
              {filled(problems.ui) && <p className="field-error">{problems.ui}</p>}
            </>
          )}
          {(g.engine === "argocd" || g.ui) && (
            <>
              <Field
                label="Host name"
                hint={
                  g.engine === "argocd"
                    ? "Optional: serve Argo CD at this name through the cluster's ingress controller; without one, reach it with kubectl port-forward."
                    : "The name it is served at through the cluster's ingress controller."
                }
              >
                {(id) => <input id={id} value={g.hostname} onChange={(e) => set("hostname", e.target.value.trim())} />}
              </Field>
              {g.engine === "argocd" && (
                <Field
                  label="Admin groups"
                  hint="Groups of the cluster's first identity provider that administer Argo CD; nobody else has any rights in it."
                  error={problems.admins}
                >
                  {(id) => <input id={id} value={g.admins} onChange={(e) => set("admins", e.target.value)} />}
                </Field>
              )}
              <Field
                label="Client secret"
                hint="What the web interface signs in to the identity provider with, as the provider's client."
              >
                {(id) => (
                  <select
                    id={id}
                    value={g.clientSecret}
                    onChange={(e) => {
                      const v = e.target.value;
                      if (oneOf(clientSecrets, v)) {
                        set("clientSecret", v);
                      }
                    }}
                  >
                    {clientSecretOptions.map((o) => (
                      <option key={o.value} value={o.value} disabled={o.value === "none" && g.engine === "flux"}>
                        {o.label}
                      </option>
                    ))}
                  </select>
                )}
              </Field>
              {g.clientSecret === "existingSecret" && (
                <Field
                  label="Client secret Secret"
                  hint={`A Secret in ${g.engine === "flux" ? "flux-system" : "argocd"} with the key clientSecret${g.engine === "argocd" ? ", labelled app.kubernetes.io/part-of=argocd" : ""}.`}
                  error={problems.clientSecretName}
                >
                  {(id) => (
                    <input
                      id={id}
                      spellCheck={false}
                      value={g.clientSecretName}
                      onChange={(e) => set("clientSecretName", e.target.value.trim())}
                    />
                  )}
                </Field>
              )}
            </>
          )}
        </fieldset>
      )}
    </>
  );
}

/** DeployKey shows what the controller made for the cluster: the key to add and the host keys it trusts. */
function DeployKey({ keys, auth, url }: { keys: GitOpsKeys | undefined; auth: RepoAuth; url: string }): JSX.Element {
  const [copied, setCopied] = useState(false);
  const deployKey = keys?.deployKey;
  const copy = async (key: string): Promise<void> => {
    await navigator.clipboard.writeText(key);
    setCopied(true);
  };
  return (
    <fieldset className="group">
      <legend>Keys</legend>
      {auth === "deployKey" &&
        (filled(deployKey) ? (
          <>
            <p className="hint">Add this to the repository as a read-only deploy key.</p>
            <p className="key" aria-label="Deploy key">
              {deployKey}
            </p>
            <p className="hint">Fingerprint {keys?.deployKeyFingerprint}</p>
            <div className="actions">
              <button
                type="button"
                onClick={() => {
                  void copy(deployKey);
                }}
              >
                {copied ? "Copied" : "Copy deploy key"}
              </button>
            </div>
          </>
        ) : (
          <p className="muted">Kubehenge makes the deploy key once this is saved; it shows here then.</p>
        ))}
      {url.startsWith("ssh://") &&
        ((keys?.hostKeys ?? []).length > 0 ? (
          <>
            <p className="hint">
              The repository host&apos;s keys Kubehenge trusts, to compare with the host&apos;s own:
            </p>
            <ul className="key">
              {keys?.hostKeys?.map((k) => (
                <li key={k}>{k}</li>
              ))}
            </ul>
          </>
        ) : (
          <p className="muted">The host&apos;s keys are read once this is saved; their fingerprints show here then.</p>
        ))}
    </fieldset>
  );
}

export function GitOpsDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const held = record(c.variables["gitops"]);
  const current = gitOpsOf(held);
  const [value, setValue] = useState(current);
  const { busy, error, send } = useChange(c, onClose);
  const problems = gitOpsProblems(value);
  const next = gitOpsVariable(value, held);
  const changed = JSON.stringify(next) !== JSON.stringify(gitOpsVariable(current, held));
  // Without the right to choose a repository, GitOps can still be switched off.
  const locked = !c.allowed.kubeconfig;
  const ref = { namespace: c.namespace, name: c.name };
  return (
    <Dialog
      title="GitOps"
      onClose={onClose}
      onSubmit={() => {
        if (next !== undefined) {
          void send({ variables: { gitops: next } });
        }
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button
            type="submit"
            className="primary"
            disabled={busy || !changed || next === undefined || Object.keys(problems).length > 0}
          >
            Save
          </button>
        </>
      }
    >
      <GitOpsFields value={value} onChange={setValue} problems={problems} locked={locked} />
      {locked && <p className="hint">{gitOpsTrustHint} You may switch it off.</p>}
      {value.engine !== "" && (value.auth === "deployKey" || value.url.startsWith("ssh://")) && (
        <DeployKey keys={c.gitops} auth={value.auth} url={value.url} />
      )}
      {value.engine !== "" && value.auth === "token" && (
        <CredentialField
          label="Repository token"
          purpose="gitops"
          cluster={ref}
          keys={[
            { key: "username", label: "User name", plain: true },
            { key: "password", label: "Token" },
          ]}
          hint="A read-only token for the repository, with the user name it goes with."
        />
      )}
      {value.engine !== "" && value.clientSecret === "stored" && (
        <CredentialField
          label="Sign-in client secret"
          purpose="oidc-client"
          cluster={ref}
          keys={[{ key: "clientSecret", label: "Client secret" }]}
          hint="The identity provider's secret for the web interface's client."
        />
      )}
      <ErrorBox message={error} />
    </Dialog>
  );
}

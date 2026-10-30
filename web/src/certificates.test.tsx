import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import {
  CertificatesDialog,
  certificatesOf,
  certificatesProblems,
  certificatesText,
  certificatesVariable,
  newCertificates,
} from "./Certificates";
import { fakeServer, type Route } from "./fake-server";
import type { Cluster, ClusterClass } from "./types";

const web: Cluster = {
  namespace: "default",
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
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { storage: "local-lvm" },
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

const rke2: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  variables: [
    { name: "controlPlaneEndpoint", required: true, schema: { type: "string" } },
    { name: "network", required: true, schema: { type: "object" } },
    { name: "storage", required: true, schema: { type: "string" } },
    { name: "certificates", required: false, schema: { type: "object" } },
  ],
};

const credentialPath = "/api/clusters/default/web/credentials/dns01";
const unset = { body: { purpose: "dns01", set: false, canChange: true } };

describe("certificates", () => {
  it("reads back what it writes, and leaves out what the issuer does not use", () => {
    const f = {
      ...newCertificates(),
      enabled: true,
      domain: "apps.example.com",
      email: "ops@example.com",
      provider: "route53" as const,
      nameservers: "1.1.1.1:53, [2606:4700::1111]:53",
      settings: { route53: { region: "eu-central-1", accessKeyID: "AKIA1", hostedZoneID: " " } },
    };
    const v = certificatesVariable(f);
    expect(v).toStrictEqual({
      enabled: true,
      issuer: "acme-dns01",
      domain: "apps.example.com",
      acme: { email: "ops@example.com" },
      dns01: {
        provider: "route53",
        nameservers: ["1.1.1.1:53", "[2606:4700::1111]:53"],
        route53: { region: "eu-central-1", accessKeyID: "AKIA1" },
      },
    });
    expect(certificatesVariable(certificatesOf(v))).toStrictEqual(v);
    expect(certificatesProblems(f)).toStrictEqual({});
    expect(certificatesText(f)).toBe(
      "Let's Encrypt by DNS-01 through Amazon Route 53, for apps.example.com and *.apps.example.com",
    );

    // HTTP-01 takes neither a domain nor a DNS provider; the CA no ACME account.
    expect(certificatesVariable({ ...f, issuer: "acme-http01", staging: true })).toStrictEqual({
      enabled: true,
      issuer: "acme-http01",
      acme: { email: "ops@example.com", staging: true },
    });
    expect(certificatesVariable({ ...f, issuer: "ca" })).toStrictEqual({
      enabled: true,
      issuer: "ca",
      domain: "apps.example.com",
    });
    expect(certificatesVariable({ ...f, enabled: false })).toBeUndefined();
    // Another ACME server stands in for Let's Encrypt staging.
    expect(
      certificatesVariable({ ...f, issuer: "acme-http01", staging: true, server: "https://acme.example/dir" }),
    ).toMatchObject({ acme: { server: "https://acme.example/dir" } });
  });

  it("takes the credential from where it is chosen, and Akamai's from a Secret only", () => {
    const f = { ...newCertificates(), enabled: true, domain: "example.org" };
    expect(certificatesVariable({ ...f, source: "shared" })).toMatchObject({
      dns01: { provider: "cloudflare", sharedCredential: true },
    });
    expect(certificatesVariable({ ...f, source: "secret", existingSecret: "dns" })).toMatchObject({
      dns01: { provider: "cloudflare", existingSecret: "dns" },
    });
    const akamai = { ...f, provider: "akamai" as const, settings: { akamai: { serviceConsumerDomain: "akab-1" } } };
    expect(certificatesProblems(akamai)).toStrictEqual({
      existingSecret: "Use lowercase letters, digits, dashes and dots",
    });
    expect(certificatesVariable({ ...akamai, existingSecret: "edgegrid" })).toMatchObject({
      dns01: { provider: "akamai", existingSecret: "edgegrid", akamai: { serviceConsumerDomain: "akab-1" } },
    });
    expect(certificatesOf(certificatesVariable({ ...f, source: "shared" })).source).toBe("shared");
  });

  it("names what is wrong with a choice", () => {
    const f = { ...newCertificates(), enabled: true, provider: "rfc2136" as const };
    expect(
      certificatesProblems({
        ...f,
        domain: "Not A Domain",
        email: "nobody",
        server: "http://acme.example",
        caBundle: "not pem",
        nameservers: "1.1.1.1",
        settings: { rfc2136: { nameserver: "ns1.example:99999" } },
      }),
    ).toStrictEqual({
      domain: "Enter a domain name, such as apps.example.com",
      email: "Enter an email address, such as ops@example.com",
      server: "Enter an https:// URL",
      caBundle: "Paste one or more PEM certificates",
      nameservers: "Enter 1.1.1.1 as host:port, such as 192.0.2.53:53",
      "setting.nameserver": "Enter ns1.example:99999 as host:port, such as 192.0.2.53:53",
      "setting.tsigKeyName": "Enter the TSIG key name",
    });
    expect(certificatesProblems({ ...f, enabled: false, domain: "" })).toStrictEqual({});
    expect(certificatesProblems({ ...f, issuer: "acme-http01", domain: "" })).toStrictEqual({});
  });

  it("switches certificates on with the cluster's own credential, and off keeping the rest", async () => {
    const routes: Record<string, Route> = {
      [`GET ${credentialPath}`]: unset,
      "PATCH /api/clusters/default/web": { body: web },
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    const closed = vi.fn<() => void>();
    const { unmount } = render(<CertificatesDialog cluster={web} onClose={closed} />);
    const dialog = within(screen.getByRole("dialog", { name: "Edit certificates" }));
    await user.click(dialog.getByRole("checkbox", { name: "Issue certificates" }));
    expect(dialog.getByLabelText("Issuer")).toHaveValue("acme-dns01");
    await user.type(dialog.getByLabelText("Domain"), "Apps.Example.com");
    expect(dialog.getByRole("radio", { name: "For this cluster (recommended)" })).toBeChecked();
    expect(dialog.getByRole("radio", { name: "Shared by all clusters" })).toBeEnabled();
    const token = within(dialog.getByRole("group", { name: "API token" }));
    await expect(token.findByText("Not set")).resolves.toBeInTheDocument();
    expect(token.getByText(/Zone, DNS, Edit on this zone only/u)).toBeInTheDocument();
    await user.click(dialog.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(calls.at(-1)?.key).toBe("PATCH /api/clusters/default/web"));
    expect(calls.at(-1)?.body).toStrictEqual({
      variables: {
        certificates: {
          enabled: true,
          issuer: "acme-dns01",
          domain: "apps.example.com",
          dns01: { provider: "cloudflare" },
        },
      },
    });
    unmount();

    const on: Cluster = {
      ...web,
      variables: { certificates: { enabled: true, issuer: "ca", domain: "lab.internal", extra: 1 } },
    };
    render(<CertificatesDialog cluster={on} onClose={closed} />);
    await user.click(screen.getByRole("checkbox", { name: "Issue certificates" }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(closed).toHaveBeenCalledTimes(2));
    expect(calls.at(-1)?.body).toStrictEqual({
      variables: { certificates: { enabled: false, issuer: "ca", domain: "lab.internal", extra: 1 } },
    });
  });

  it("holds back the credential for every cluster without Sys.Modify, and checks before saving", async () => {
    fakeServer({ [`GET ${credentialPath}`]: unset });
    const user = userEvent.setup({ delay: null });
    render(
      <CertificatesDialog
        cluster={{ ...web, allowed: { change: true, kubeconfig: false } }}
        onClose={vi.fn<() => void>()}
      />,
    );
    await user.click(screen.getByRole("checkbox", { name: "Issue certificates" }));
    const shared = screen.getByRole("radio", { name: "Shared by all clusters" });
    expect(shared).toBeDisabled();
    expect(shared).toHaveAccessibleDescription("Needs Sys.Modify on the cluster's pool You don't hold it");
    await user.selectOptions(screen.getByLabelText("DNS provider"), "akamai");
    // Akamai takes a Secret of the cluster's only.
    expect(screen.queryByRole("radio", { name: /For this cluster/u })).not.toBeInTheDocument();
    expect(screen.getByLabelText("Secret name")).toHaveAccessibleDescription(
      "In cert-manager, with keys clientToken, clientSecret and accessToken",
    );
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByLabelText("Domain")).toHaveAccessibleDescription(/^Enter a domain name/u);
    expect(screen.getByLabelText("Service consumer domain")).toHaveAccessibleDescription(
      /^Enter the service consumer domain/u,
    );
  });

  it("hands out the cluster's CA, and says when there is none yet", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const withCA: Cluster = {
      ...web,
      variables: { certificates: { enabled: true, issuer: "ca", domain: "lab.internal" } },
    };
    const calls = fakeServer({
      "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1" } }),
      "GET /api/clusters/default/web": () => ({ body: withCA }),
      "GET /api/clusters/default/web/certificates/ca": { status: 404, body: { error: "the cluster has no CA yet" } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "CA certificate" }));
    await expect(screen.findByText("The cluster has no CA yet.")).resolves.toBeInTheDocument();
    expect(calls.map((c) => c.key)).toContain("GET /api/clusters/default/web/certificates/ca");
  });
});

const wizard = (store: Route): ReturnType<typeof fakeServer> => {
  window.history.replaceState(null, "", "/clusters/new");
  return fakeServer({
    "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1", canCreate: true, canTrust: true } }),
    "GET /api/classes": () => ({ body: [rke2] }),
    "GET /api/proxmox/storages": () => ({
      body: [
        { storage: "local-lvm", node: "pve1", shared: false, content: ["images"], avail: 1, total: 2, usable: true },
      ],
    }),
    "GET /api/proxmox/nodes": () => ({ body: [{ node: "pve1", online: true }] }),
    "GET /api/proxmox/nodes/pve1/bridges": () => ({
      body: [{ bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true }],
    }),
    "GET /api/proxmox/ha-rules": () => ({ body: { readable: true, rules: [] } }),
    "POST /api/clusters": () => ({ status: 201, body: web }),
    [`PUT ${credentialPath}`]: store,
    "GET /api/clusters/default/web": () => ({ body: web }),
  });
};

const createWithToken = async (): Promise<void> => {
  const user = userEvent.setup({ delay: null });
  render(<App />);
  await user.type(await screen.findByLabelText("Name"), "web");
  await waitFor(() => expect(screen.getByRole("option", { name: "RKE2" })).toBeInTheDocument());
  await user.selectOptions(screen.getByLabelText("Distribution"), "kubehenge-rke2");
  await user.type(screen.getByLabelText("Kubernetes version"), "v1.36.5+rke2r1");
  await user.click(screen.getByRole("button", { name: "Next" }));
  await waitFor(() => expect(screen.getByLabelText("Bridge")).toHaveValue("vmbr0"));
  await user.type(screen.getByLabelText("API address"), "10.0.0.20");
  await user.type(screen.getByLabelText("Node addresses"), "10.0.0.21-10.0.0.29");
  await user.type(screen.getByLabelText("Gateway"), "10.0.0.1");
  await user.type(screen.getByLabelText("DNS servers"), "10.0.0.1");
  await user.click(screen.getByRole("button", { name: "Next" }));
  await user.click(screen.getByRole("button", { name: "Next" }));

  const certificates = within(screen.getByRole("group", { name: "Certificates" }));
  await user.click(certificates.getByRole("checkbox", { name: "Issue certificates" }));
  // Next on an incomplete choice stays and says what is missing.
  await user.click(screen.getByRole("button", { name: "Next" }));
  expect(certificates.getByLabelText("Domain")).toHaveAccessibleDescription(/^Enter a domain name/u);
  await user.type(certificates.getByLabelText("Domain"), "apps.example.com");
  await user.type(certificates.getByLabelText("API token"), "zone-token");
  await user.click(screen.getByRole("button", { name: "Next" }));
  await user.click(screen.getByRole("button", { name: "Next" }));
  expect(screen.getByText("Stored once created")).toBeInTheDocument();
  expect(screen.getByLabelText("Request")).not.toHaveTextContent("zone-token");
  await user.click(screen.getByRole("button", { name: "Create" }));
};

describe("certificates of a new cluster", () => {
  it("stores the token for the cluster once it is created", async () => {
    const calls = wizard({ body: { purpose: "dns01", set: true, canChange: true } });
    await createWithToken();
    await waitFor(() => expect(window.location.pathname).toBe("/clusters/default/web"));
    const keys = calls.map((c) => c.key);
    expect(keys.indexOf(`PUT ${credentialPath}`)).toBeGreaterThan(keys.indexOf("POST /api/clusters"));
    expect(calls.find((c) => c.key === `PUT ${credentialPath}`)?.body).toStrictEqual({
      values: { token: "zone-token" },
    });
    expect(calls.find((c) => c.key === "POST /api/clusters")?.body).toMatchObject({
      variables: { certificates: { enabled: true, domain: "apps.example.com", dns01: { provider: "cloudflare" } } },
    });
  });

  it("says when the cluster was created but its token was not stored, and never creates it twice", async () => {
    const calls = wizard({ status: 403, body: { error: "alice@pve needs Sys.Modify on /pool/kubehenge/default.web" } });
    await createWithToken();
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent(
      "Cluster created, but the API token was not stored. Add it in Options, Certificates.",
    );
    expect(screen.getByText(/alice@pve needs Sys\.Modify on \/pool\/kubehenge\/default\.web/u)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Create" })).not.toBeInTheDocument();
    expect(screen.getByText("None yet: the cluster waits")).toBeInTheDocument();
    const user = userEvent.setup({ delay: null });
    await user.click(screen.getByRole("button", { name: "Open cluster" }));
    await waitFor(() => expect(window.location.pathname).toBe("/clusters/default/web/options"));
    expect(calls.filter((c) => c.key === "POST /api/clusters")).toHaveLength(1);
  });
});

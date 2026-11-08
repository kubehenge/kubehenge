import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fieldProblems, newForm, request, summary } from "./create-form";
import { applyStaged, fakeServer } from "./fake-server";
import {
  MirrorFields,
  certificateProblem,
  mirrorsOf,
  mirrorsProblem,
  mirrorsText,
  mirrorsVariable,
  newMirror,
  trustedOf,
  trustedText,
  trustedVariable,
  type MirrorRow,
} from "./NodeSite";
import type { Cluster, ClusterClass } from "./types";

const body = "MIIBTjCB9aADAgECAgEBMAoGCCqGSM49BAMCMA8xDTALBgNVBAMTBGdvb2QwHhcN";
const certificate = `-----BEGIN CERTIFICATE-----\n${body}\n-----END CERTIFICATE-----\n`;

const mirror = (over: Partial<MirrorRow> = {}): MirrorRow => ({
  registry: "docker.io",
  endpoints: "https://mirror.example:5000\nhttp://plain.example",
  caBundle: "",
  ...over,
});

describe("the registry mirror settings", () => {
  it("send each registry with its endpoints and a normalised authority, and nothing for blank rows", () => {
    expect(mirrorsVariable([])).toBeUndefined();
    expect(mirrorsVariable([newMirror()])).toBeUndefined();
    expect(mirrorsVariable([mirror(), newMirror()])).toStrictEqual([
      { registry: "docker.io", endpoints: ["https://mirror.example:5000", "http://plain.example"] },
    ]);
    const crlf = certificate.replaceAll("\n", "\r\n");
    expect(mirrorsVariable([mirror({ registry: " * ", caBundle: `  ${crlf}  ` })])).toStrictEqual([
      { registry: "*", endpoints: ["https://mirror.example:5000", "http://plain.example"], caBundle: certificate },
    ]);
  });

  it("read the variable back and say where the images come from", () => {
    expect(mirrorsOf(undefined)).toStrictEqual([]);
    const sent = mirrorsVariable([mirror({ caBundle: certificate })]);
    expect(mirrorsOf(sent)).toStrictEqual([mirror({ caBundle: certificate })]);
    expect(mirrorsText([])).toBe("None");
    expect(mirrorsText([mirror(), mirror({ registry: "ghcr.io" })])).toBe("docker.io, ghcr.io");
  });

  it("hold the rows to the schema", () => {
    expect(mirrorsProblem([mirror()])).toBeUndefined();
    expect(mirrorsProblem([mirror({ registry: "*", endpoints: "https://harbor.example/proxy/v2" })])).toBeUndefined();
    expect(mirrorsProblem([mirror({ registry: "registry.example:5000", caBundle: certificate })])).toBeUndefined();
    expect(mirrorsProblem([mirror({ registry: "Docker.io" })])).toMatch(/^Mirror 1: enter a lower-case registry/u);
    expect(mirrorsProblem([mirror({ endpoints: "" })])).toBe("Mirror 1: add an endpoint");
    for (const endpoints of [
      "mirror.example",
      "ftp://mirror.example",
      "https://mirror.example/path",
      "https://u:p@m.example",
      "https://m'.example",
    ]) {
      expect(mirrorsProblem([mirror({ endpoints })])).toMatch(/http:\/\/ or https:\/\/ addresses/u);
    }
    expect(
      mirrorsProblem([
        mirror({
          endpoints: "https://a.example\nhttps://b.example\nhttps://c.example\nhttps://d.example\nhttps://e.example",
        }),
      ]),
    ).toBe("Mirror 1: at most 4 endpoints");
    expect(mirrorsProblem([mirror(), mirror({ endpoints: "https://other.example" })])).toBe(
      "Mirror 2: each registry once",
    );
    expect(mirrorsProblem([mirror({ caBundle: "not a certificate" })])).toBe("Mirror 1: paste pem certificates only");
    expect(mirrorsProblem(Array.from({ length: 17 }, (_, i) => mirror({ registry: `r${i}.example` })))).toBe(
      "At most 16 mirrors",
    );
  });
});

describe("the trusted authorities", () => {
  it("send one bundle, read it back and count its certificates", () => {
    expect(trustedVariable("")).toBeUndefined();
    expect(trustedVariable("  \n")).toBeUndefined();
    expect(trustedVariable(certificate.trim())).toStrictEqual([certificate]);
    expect(trustedOf([certificate])).toBe(certificate);
    expect(trustedText("")).toBe("None");
    expect(trustedText(certificate)).toBe("1 certificate");
    expect(trustedText(certificate + certificate)).toBe("2 certificates");
  });

  it("accept certificates and refuse anything else", () => {
    expect(certificateProblem("")).toBeUndefined();
    expect(certificateProblem(certificate + certificate)).toBeUndefined();
    expect(certificateProblem(certificate.replaceAll("\n", "\r\n"))).toBeUndefined();
    expect(certificateProblem("hello")).toBe("Paste PEM certificates only");
    expect(certificateProblem(`${certificate}EOF`)).toBe("Paste PEM certificates only");
    expect(certificateProblem("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")).toBe(
      "Paste PEM certificates only",
    );
  });
});

describe("the wizard's site settings", () => {
  const cls: ClusterClass = {
    name: "kubehenge-rke2",
    distribution: "rke2",
    versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
    variables: [
      { name: "storage", required: true, schema: { type: "string" } },
      { name: "registryMirrors", required: false, schema: { type: "array" } },
      { name: "trustedCAs", required: false, schema: { type: "array" } },
    ],
  };
  const bare: ClusterClass = { ...cls, variables: cls.variables.filter((v) => v.name === "storage") };

  it("go into the request, the summary and the problems only for a class that defines them", () => {
    const form = { ...newForm(), rancherRegister: false };
    const set = { ...form, registryMirrors: [mirror()], trustedCAs: certificate };
    expect(request(form, cls).variables).not.toHaveProperty("registryMirrors");
    expect(request(form, cls).variables).not.toHaveProperty("trustedCAs");
    expect(request(set, cls).variables["registryMirrors"]).toStrictEqual([
      { registry: "docker.io", endpoints: ["https://mirror.example:5000", "http://plain.example"] },
    ]);
    expect(request(set, cls).variables["trustedCAs"]).toStrictEqual([certificate]);
    expect(request(set, bare).variables).not.toHaveProperty("registryMirrors");
    expect(Object.fromEntries(summary(set, cls))).toMatchObject({
      "Registry mirrors": "docker.io",
      "Trusted CAs": "1 certificate",
    });
    expect(Object.fromEntries(summary(form, cls))).not.toHaveProperty("Registry mirrors");
    expect(Object.fromEntries(summary(set, bare))).not.toHaveProperty("Trusted CAs");
    const wrong = { ...form, registryMirrors: [mirror({ endpoints: "x" })], trustedCAs: "x" };
    expect(fieldProblems("Network", wrong, cls)).toMatchObject({
      registryMirrors: expect.stringContaining("Mirror 1") as unknown,
      trustedCAs: "Paste PEM certificates only",
    });
    expect(fieldProblems("Network", wrong, bare)).not.toHaveProperty("trustedCAs");
  });
});

describe("the mirror fields", () => {
  it("add and remove rows and change each one's registry, endpoints and authority", async () => {
    const changes: MirrorRow[][] = [];
    const user = userEvent.setup({ delay: null });
    const record = (rows: MirrorRow[]): void => {
      changes.push(rows);
    };
    const { rerender } = render(<MirrorFields value={[]} onChange={record} />);
    await user.click(screen.getByRole("button", { name: "Add mirror" }));
    expect(changes.at(-1)).toStrictEqual([newMirror()]);
    rerender(<MirrorFields value={[mirror(), mirror({ registry: "ghcr.io" })]} onChange={record} />);
    const registries = screen.getAllByLabelText("Registry");
    expect(registries).toHaveLength(2);
    await user.type(within(screen.getByRole("group", { name: "Mirror 2" })).getByLabelText("Registry"), "x");
    expect(changes.at(-1)?.[1]?.registry).toBe("ghcr.iox");
    await user.click(screen.getByRole("button", { name: "Remove mirror 1" }));
    expect(changes.at(-1)).toStrictEqual([mirror({ registry: "ghcr.io" })]);
    rerender(<MirrorFields value={[mirror()]} onChange={record} error="Mirror 1: add an endpoint" />);
    expect(screen.getByRole("alert")).toHaveTextContent(/Mirror 1: add an endpoint/u);
  });
});

const cluster: Cluster = {
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
  variables: {},
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

describe("a cluster's site options", () => {
  it("save registry mirrors, refusing a malformed endpoint first", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit the registry mirrors" }));
    const dialog = screen.getByRole("dialog", { name: "Edit registry mirrors" });
    const save = within(dialog).getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    await user.click(within(dialog).getByRole("button", { name: "Add mirror" }));
    await user.type(within(dialog).getByLabelText("Registry"), "docker.io");
    await user.type(within(dialog).getByLabelText("Endpoints"), "mirror.example");
    await user.click(save);
    expect(within(dialog).getByText(/Mirror 1: endpoints are http/u)).toBeInTheDocument();
    expect(calls.filter((c) => c.key === "PATCH /api/clusters/clusters/web")).toHaveLength(0);
    await user.clear(within(dialog).getByLabelText("Endpoints"));
    await user.type(within(dialog).getByLabelText("Endpoints"), "https://mirror.example:5000");
    await user.click(save);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { registryMirrors: [{ registry: "docker.io", endpoints: ["https://mirror.example:5000"] }] },
    });
  });

  it("save trusted CAs and clear the variable with an empty field", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const held = { ...cluster, variables: { trustedCAs: [certificate] } };
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: held },
      "PATCH /api/clusters/clusters/web": { body: held },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(screen.findByRole("cell", { name: "1 certificate" })).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Edit the trusted CAs" }));
    const dialog = screen.getByRole("dialog", { name: "Edit trusted CAs" });
    const field = within(dialog).getByLabelText("Trusted CAs");
    await user.clear(field);
    await user.type(field, "junk");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(within(dialog).getByText("Paste PEM certificates only")).toBeInTheDocument();
    await user.clear(field);
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { trustedCAs: null },
    });
  });
});

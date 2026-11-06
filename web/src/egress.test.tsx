import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fieldProblems, newForm, request, summary } from "./create-form";
import {
  EgressFields,
  egressOf,
  egressProblems,
  egressText,
  egressVariable,
  newEgress,
  type EgressChoice,
} from "./EgressProxy";
import { applyStaged, fakeServer } from "./fake-server";
import type { Cluster, ClusterClass } from "./types";

const own = (over: Partial<EgressChoice> = {}): EgressChoice => ({
  ...newEgress(),
  mode: "own",
  url: "http://proxy.example:3128",
  ...over,
});

const entries = (n: number): string => Array.from({ length: n }, (_, i) => `h${i}.example`).join(", ");

describe("the egress proxy settings", () => {
  it("send nothing for the management proxy, the address and exceptions for an own one, and only disabled for none", () => {
    expect(egressVariable(newEgress())).toBeUndefined();
    expect(egressVariable({ ...own(), mode: "management" })).toBeUndefined();
    expect(egressVariable(own())).toStrictEqual({ url: "http://proxy.example:3128" });
    expect(
      egressVariable(own({ url: " https://proxy.example/ ", noProxy: ".corp, 10.0.0.0/8\nlocal.example  x" })),
    ).toStrictEqual({ url: "https://proxy.example/", noProxy: [".corp", "10.0.0.0/8", "local.example", "x"] });
    expect(egressVariable({ ...own({ noProxy: ".corp" }), mode: "none" })).toStrictEqual({ disabled: true });
  });

  it("read the variable back", () => {
    expect(egressOf(undefined)).toStrictEqual(newEgress());
    expect(egressOf({})).toStrictEqual(newEgress());
    expect(egressOf({ disabled: true })).toStrictEqual({ ...newEgress(), mode: "none" });
    expect(egressOf({ url: "http://proxy.example:3128", noProxy: [".corp", "10.0.0.0/8"] })).toStrictEqual(
      own({ noProxy: ".corp, 10.0.0.0/8" }),
    );
    const sent = egressVariable(own({ noProxy: "a.example" }));
    expect(egressOf(sent)).toStrictEqual(own({ noProxy: "a.example" }));
  });

  it("say where the traffic goes", () => {
    expect(egressText(newEgress())).toBe("Management proxy");
    expect(egressText({ ...newEgress(), mode: "none" })).toBe("No proxy");
    expect(egressText(own())).toBe("http://proxy.example:3128");
    expect(egressText(own({ noProxy: ".corp" }))).toBe("http://proxy.example:3128, 1 exception");
    expect(egressText(own({ noProxy: ".corp, .lan" }))).toBe("http://proxy.example:3128, 2 exceptions");
  });

  it("hold the address to the schema", () => {
    expect(egressProblems(own())).toStrictEqual({});
    expect(egressProblems(own({ url: "https://proxy.example" }))).toStrictEqual({});
    expect(egressProblems(own({ url: "https://proxy.example/" }))).toStrictEqual({});
    const wrong = "Enter an http:// or https:// address without credentials or path";
    const urls = [
      "",
      "proxy.example",
      "socks5://proxy.example",
      "http://alice:hunter2@proxy.example",
      "http://proxy.example/pac",
      "http://proxy.example?x=1",
      "http://pro xy.example",
    ];
    const accepted = urls.filter((url) => egressProblems(own({ url })).url !== wrong);
    expect(accepted).toStrictEqual([]);
    const credentials = egressProblems(own({ url: "http://alice:hunter2@proxy.example" }));
    expect(JSON.stringify(credentials)).not.toContain("hunter2");
  });

  it("hold the exceptions to 64 entries, each split at commas and spaces", () => {
    const full = own({ noProxy: entries(64) });
    const over = own({ noProxy: entries(65) });
    expect(egressProblems(full)).toStrictEqual({});
    expect(egressProblems(over).noProxy).toBe("At most 64 exceptions");
    // A comma or space ends an entry, so no entry holds one.
    expect(egressVariable(own({ noProxy: "a,b c\nd" }))).toHaveProperty("noProxy", ["a", "b", "c", "d"]);
  });

  it("check nothing unless the proxy is the cluster's own", () => {
    expect(egressProblems({ mode: "management", url: "junk", noProxy: "" })).toStrictEqual({});
    expect(egressProblems({ mode: "none", url: "junk", noProxy: "" })).toStrictEqual({});
  });

  it("go into the wizard's request, summary and problems only for a class that defines the variable", () => {
    const cls: ClusterClass = {
      name: "kubehenge-rke2",
      distribution: "rke2",
      versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
      variables: [
        { name: "storage", required: true, schema: { type: "string" } },
        { name: "egressProxy", required: false, schema: { type: "object" } },
      ],
    };
    const bare: ClusterClass = { ...cls, variables: cls.variables.filter((v) => v.name !== "egressProxy") };
    const form = { ...newForm(), rancherRegister: false };
    const withOwn = { ...form, egress: own({ noProxy: ".corp" }) };
    const withNone = { ...form, egress: { ...newEgress(), mode: "none" as const } };
    const wrongUrl = { ...form, egress: own({ url: "x" }) };
    const tooMany = { ...form, egress: own({ noProxy: entries(65) }) };
    expect(request(form, cls).variables).not.toHaveProperty("egressProxy");
    expect(request(withOwn, cls).variables["egressProxy"]).toStrictEqual({
      url: "http://proxy.example:3128",
      noProxy: [".corp"],
    });
    expect(request(withNone, cls).variables["egressProxy"]).toStrictEqual({ disabled: true });
    expect(request(withOwn, bare).variables).not.toHaveProperty("egressProxy");
    const rows = Object.fromEntries(summary(form, cls));
    expect(rows).toMatchObject({ "Egress proxy": "Management proxy" });
    const ownRows = Object.fromEntries(summary(withOwn, cls));
    expect(ownRows).toMatchObject({ "Egress proxy": "http://proxy.example:3128, 1 exception" });
    expect(Object.fromEntries(summary(form, bare))).not.toHaveProperty("Egress proxy");
    expect(fieldProblems("Add-ons", wrongUrl, cls)).toHaveProperty("egress.url");
    expect(fieldProblems("Add-ons", tooMany, cls)).toHaveProperty("egress.noProxy");
    expect(fieldProblems("Add-ons", wrongUrl, bare)).toStrictEqual({});
  });
});

describe("the egress proxy fields", () => {
  it("choose between three proxies and ask for the address and exceptions of an own one", async () => {
    const changes: EgressChoice[] = [];
    const user = userEvent.setup({ delay: null });
    const record = (e: EgressChoice): void => {
      changes.push(e);
    };
    const { rerender } = render(<EgressFields value={newEgress()} onChange={record} />);
    expect(screen.getByRole("radio", { name: "Management proxy" })).toBeChecked();
    expect(screen.queryByLabelText("Proxy address")).not.toBeInTheDocument();
    await user.click(screen.getByRole("radio", { name: "Own proxy" }));
    expect(changes.at(-1)).toMatchObject({ mode: "own" });
    await user.click(screen.getByRole("radio", { name: "No proxy" }));
    expect(changes.at(-1)).toMatchObject({ mode: "none" });

    rerender(<EgressFields value={own()} onChange={record} problems={{ url: "Bad address" }} />);
    expect(screen.getByRole("radio", { name: "Own proxy" })).toBeChecked();
    expect(screen.getByLabelText("Proxy address")).toHaveValue("http://proxy.example:3128");
    expect(screen.getByLabelText("Exceptions")).toHaveValue("");
    expect(screen.getByText("Bad address")).toBeInTheDocument();
    await user.type(screen.getByLabelText("Exceptions"), "x");
    expect(changes.at(-1)).toMatchObject({ noProxy: "x" });
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

describe("a cluster's egress proxy option", () => {
  it("lists the management proxy and saves an own one", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit the egress proxy" }));
    const dialog = screen.getByRole("dialog", { name: "Edit egress proxy" });
    const save = within(dialog).getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    await user.click(within(dialog).getByRole("radio", { name: "Own proxy" }));
    expect(save).toBeDisabled();
    await user.type(within(dialog).getByLabelText("Proxy address"), "http://alice:pw@proxy.example:3128");
    expect(within(dialog).getByText(/without credentials or path/u)).toBeInTheDocument();
    expect(save).toBeDisabled();
    await user.clear(within(dialog).getByLabelText("Proxy address"));
    await user.type(within(dialog).getByLabelText("Proxy address"), "http://proxy.example:3128");
    await user.type(within(dialog).getByLabelText("Exceptions"), ".corp, 10.0.0.0/8");
    await user.click(save);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { egressProxy: { url: "http://proxy.example:3128", noProxy: [".corp", "10.0.0.0/8"] } },
    });
  });

  it("loads the saved choice and clears the variable to return to the management proxy", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const held = { ...cluster, variables: { egressProxy: { url: "http://proxy.example:3128", noProxy: [".corp"] } } };
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: held },
      "PATCH /api/clusters/clusters/web": { body: held },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(
      screen.findByRole("cell", { name: "http://proxy.example:3128, 1 exception" }),
    ).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Edit the egress proxy" }));
    const dialog = screen.getByRole("dialog", { name: "Edit egress proxy" });
    expect(within(dialog).getByLabelText("Proxy address")).toHaveValue("http://proxy.example:3128");
    expect(within(dialog).getByLabelText("Exceptions")).toHaveValue(".corp");
    await user.click(within(dialog).getByRole("radio", { name: "Management proxy" }));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { egressProxy: null },
    });
  });

  it("sends only disabled for no proxy", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit the egress proxy" }));
    const dialog = screen.getByRole("dialog", { name: "Edit egress proxy" });
    await user.click(within(dialog).getByRole("radio", { name: "No proxy" }));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { egressProxy: { disabled: true } },
    });
  });
});

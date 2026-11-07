import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState, type JSX } from "react";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { applyStaged, fakeServer } from "./fake-server";
import { LogsFields, logsProblem, logsVariable, newLogs, type Logs } from "./Logs";
import { newOutput, outputProblems, outputVariable, outputsOf, type LogOutput } from "./output-model";

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
  variables: {
    monitoring: { enabled: true },
    logs: { enabled: true, collector: "node", retentionDays: 7, sizeGiB: 10 },
  },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

const output = (patch: Partial<LogOutput>): LogOutput => ({
  ...newOutput(),
  name: "siem",
  endpoint: "tls://s.example.com:6514",
  ...patch,
});

describe("log outputs model", () => {
  it("reads what the variable holds and writes it without defaults", () => {
    const held = [
      { name: "a", kind: "loki", endpoint: "https://l.example.com/push", auth: "basic", streams: ["audit", "bogus"] },
      { name: "b", kind: "nonsense", endpoint: "x" },
      "junk",
    ];
    const read = outputsOf(held);
    expect(read).toHaveLength(2);
    expect(read[0]).toMatchObject({ kind: "loki", auth: "basic", streams: ["audit"] });
    expect(read[1]).toMatchObject({ kind: "syslog", auth: "none", streams: [], caBundle: "" });
    expect(outputVariable(output({}))).toStrictEqual({
      name: "siem",
      kind: "syslog",
      endpoint: "tls://s.example.com:6514",
    });
    expect(outputVariable(output({ streams: ["audit"], auth: "bearer", caBundle: "pem" }))).toStrictEqual({
      name: "siem",
      kind: "syslog",
      endpoint: "tls://s.example.com:6514",
      caBundle: "pem",
      streams: ["audit"],
      auth: "bearer",
    });
    expect(outputsOf(undefined)).toStrictEqual([]);
  });

  it("refuses what the webhook refuses", () => {
    const bad = (patch: Partial<LogOutput>, key: string): void => {
      const keys = Object.keys(outputProblems([output(patch)]));
      expect(keys).toContain(key);
    };
    bad({ name: "" }, "0.name");
    bad({ name: "Upper" }, "0.name");
    bad({ name: "a".repeat(21) }, "0.name");
    bad({ endpoint: "" }, "0.endpoint");
    bad({ endpoint: "https://s.example.com:6514" }, "0.endpoint");
    bad({ endpoint: "tls://s.example.com" }, "0.endpoint");
    bad({ kind: "loki", endpoint: "tcp://s.example.com:1" }, "0.endpoint");
    bad({ kind: "loki", endpoint: "https://u:p@s.example.com/x" }, "0.endpoint");
    bad({ kind: "loki", endpoint: "https://s.example.com/x?t=1" }, "0.endpoint");
    bad({ caBundle: "pem", endpoint: "tcp://s.example.com:1" }, "0.caBundle");
    bad({ caBundle: "not pem" }, "0.caBundle");
    bad({ kind: "otlp", endpoint: "http://s.example.com", auth: "basic" }, "0.auth");
    expect(outputProblems([output({}), output({})])).toStrictEqual({ "1.name": "Already used" });
    expect(outputProblems(Array.from({ length: 5 }, (_, i) => output({ name: `o${i}` })))).toHaveProperty("count");
    expect(outputProblems([output({ kind: "otlp", endpoint: "https://o.example.com", auth: "bearer" })])).toStrictEqual(
      {},
    );
  });

  it("clears the held outputs when none are left", () => {
    const held = { enabled: true, outputs: [{ name: "a" }], extra: 1 };
    expect(logsVariable({ ...newLogs(), enabled: true }, held)).toStrictEqual({
      extra: 1,
      enabled: true,
      collector: "node",
      retentionDays: 7,
      sizeGiB: 10,
    });
    const next: Logs = { ...newLogs(), enabled: true, outputs: [output({})] };
    expect(logsVariable(next, held)["outputs"]).toStrictEqual([outputVariable(output({}))]);
    expect(logsProblem({ ...next, outputs: [output({ endpoint: "" })] })).toBe("Enter an endpoint");
  });
});

function Harness({ showAll = false }: { showAll?: boolean }): JSX.Element {
  const [logs, setLogs] = useState<Logs>({ ...newLogs(), enabled: true });
  return <LogsFields value={logs} onChange={setLogs} showAll={showAll} />;
}

describe("the log outputs fields", () => {
  it("start closed, add outputs and show a problem only once touched", async () => {
    const user = userEvent.setup({ delay: null });
    const { container } = render(<Harness />);
    const details = container.querySelector("details");
    expect(details).not.toHaveAttribute("open");
    expect(screen.getByText("Log outputs")).toBeInTheDocument();

    await user.click(screen.getByText("Log outputs"));
    await user.click(screen.getByRole("button", { name: "Add output" }));
    const first = screen.getByRole("group", { name: "Output 1" });
    expect(within(first).queryByText(/Lower-case letters/u)).not.toBeInTheDocument();
    expect(within(first).queryByText("Enter an endpoint")).not.toBeInTheDocument();
    await user.type(within(first).getByLabelText("Name"), "Bad Name");
    expect(within(first).getByText(/Lower-case letters/u)).toBeInTheDocument();
    expect(within(first).queryByText("Enter an endpoint")).not.toBeInTheDocument();
    expect(within(first).queryByLabelText("Authentication")).not.toBeInTheDocument();

    await user.selectOptions(within(first).getByLabelText("Kind"), "otlp");
    expect(within(first).getByLabelText("Authentication")).toBeInTheDocument();
    await user.selectOptions(within(first).getByLabelText("Kind"), "syslog");
    expect(within(first).queryByLabelText("Authentication")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Add output" }));
    expect(screen.getByRole("group", { name: "Output 2" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Add output" }));
    await user.click(screen.getByRole("button", { name: "Add output" }));
    expect(screen.getByRole("button", { name: "Add output" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Remove Output 4" }));
    expect(screen.getByRole("button", { name: "Add output" })).toBeEnabled();
  });

  it("show every problem once the step was tried", async () => {
    const user = userEvent.setup({ delay: null });
    render(<Harness showAll />);
    await user.click(screen.getByText("Log outputs"));
    await user.click(screen.getByRole("button", { name: "Add output" }));
    expect(screen.getByText("Enter an endpoint")).toBeInTheDocument();
  });

  it("tell a new cluster to store the credential later", async () => {
    const user = userEvent.setup({ delay: null });
    render(<Harness />);
    await user.click(screen.getByText("Log outputs"));
    await user.click(screen.getByRole("button", { name: "Add output" }));
    const first = screen.getByRole("group", { name: "Output 1" });
    await user.selectOptions(within(first).getByLabelText("Kind"), "loki");
    await user.type(within(first).getByLabelText("Name"), "cloud");
    await user.selectOptions(within(first).getByLabelText("Authentication"), "basic");
    expect(within(first).getByText(/Store its credential in Options/u)).toBeInTheDocument();
  });
});

describe("a cluster's log outputs", () => {
  it("are added in Options with their credential stored beside", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
      "GET /api/clusters/clusters/web/credentials/log-cloud": {
        body: { purpose: "log-cloud", set: false, canChange: true },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit logs" }));
    const dialog = screen.getByRole("dialog", { name: "Edit logs" });
    await user.click(within(dialog).getByText("Log outputs"));
    await user.click(within(dialog).getByRole("button", { name: "Add output" }));
    const first = within(dialog).getByRole("group", { name: "Output 1" });
    await user.type(within(first).getByLabelText("Name"), "cloud");
    await user.selectOptions(within(first).getByLabelText("Kind"), "loki");
    await user.type(within(first).getByLabelText("Endpoint"), "https://loki.example.com/loki/api/v1/push");
    await user.selectOptions(within(first).getByLabelText("Authentication"), "bearer");
    await user.click(within(first).getByLabelText("Audit"));
    await expect(within(first).findByText("Not set")).resolves.toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: {
        logs: {
          enabled: true,
          collector: "node",
          retentionDays: 7,
          sizeGiB: 10,
          outputs: [
            {
              name: "cloud",
              kind: "loki",
              endpoint: "https://loki.example.com/loki/api/v1/push",
              streams: ["audit"],
              auth: "bearer",
            },
          ],
        },
      },
    });
  });

  it("keep Save off while an output is wrong", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Edit logs" }));
    const dialog = screen.getByRole("dialog", { name: "Edit logs" });
    await user.click(within(dialog).getByText("Log outputs"));
    await user.click(within(dialog).getByRole("button", { name: "Add output" }));
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
  });
});

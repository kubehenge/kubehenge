import { render, renderHook, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState, type JSX } from "react";
import { describe, expect, it, vi } from "vitest";

import { newForm } from "./create-form";
import { fakeServer } from "./fake-server";
import { AdvancedFields, formOf, settings, setupFieldProblems, useTimeZoneProblem, type SetupForm } from "./Setup";
import { fromCluster } from "./start-from";
import type { Cluster } from "./types";

const pem = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----";
const noProblem = vi.fn<() => string | undefined>();

function Fields(): JSX.Element {
  const [form, setForm] = useState<SetupForm>(formOf());
  return (
    <AdvancedFields
      form={form}
      set={(k, v) => setForm((f) => ({ ...f, [k]: v }))}
      shown={noProblem}
      fixed={false}
      open
      warnings={[]}
      onOpen={vi.fn<() => void>()}
    />
  );
}

describe("the site settings in Setup", () => {
  it("say, before saving, how many clusters take the site CA and when", async () => {
    const user = userEvent.setup({ delay: null });
    const calls = fakeServer({
      "POST /api/setup/site-ca-roll": { body: { inherits: 3, now: 2, windows: 1, waiting: 0 } },
    });
    render(<Fields />);
    await user.click(screen.getByLabelText("Site CA"));
    await user.paste(pem);
    await expect(screen.findByText("Replaces nodes (2 now, 1 at window).")).resolves.toBeInTheDocument();
    expect(calls.find((c) => c.key === "POST /api/setup/site-ca-roll")?.body).toStrictEqual({ siteCA: `${pem}\n` });
  });

  it("put that note after the field's hint, and carry no placeholder in the time zone", async () => {
    const user = userEvent.setup({ delay: null });
    fakeServer({ "POST /api/setup/site-ca-roll": { body: { inherits: 1, now: 1, windows: 0, waiting: 0 } } });
    render(<Fields />);
    await user.click(screen.getByLabelText("Site CA"));
    await user.paste(pem);
    const note = await screen.findByText("Replaces nodes (1 now).");
    const hint = screen.getByText(/PEM, the default for every connection/u);
    expect(hint).toContainElement(note);
    expect(hint.textContent).toMatch(/^PEM, the default for every connection/u);
    expect(screen.getByLabelText("Time zone")).not.toHaveAttribute("placeholder");
  });

  it("says what clearing the site CA replaces", async () => {
    const user = userEvent.setup({ delay: null });
    const calls = fakeServer({
      "POST /api/setup/site-ca-roll": { body: { inherits: 3, now: 3, windows: 0, waiting: 0 } },
    });
    render(<Fields />);
    await user.click(screen.getByLabelText("Site CA"));
    await user.paste(pem);
    await expect(screen.findByText("Replaces nodes (3 now).")).resolves.toBeInTheDocument();
    await user.clear(screen.getByLabelText("Site CA"));
    await waitFor(() => {
      expect(calls.findLast((c) => c.key === "POST /api/setup/site-ca-roll")?.body).toStrictEqual({ siteCA: "" });
    });
  });

  it("asks nothing and says nothing for text that is no certificate", async () => {
    const user = userEvent.setup({ delay: null });
    const calls = fakeServer({});
    render(<Fields />);
    await user.click(screen.getByLabelText("Site CA"));
    await user.paste("junk");
    expect(calls).toHaveLength(0);
    expect(screen.queryByText(/Replaces nodes/u)).not.toBeInTheDocument();
  });

  it("take a VM ID range, send it as start and end and hold it to Proxmox VE's limits", async () => {
    const user = userEvent.setup({ delay: null });
    render(<Fields />);
    await user.type(screen.getByLabelText("VM ID range"), "10000-10999");
    expect(screen.getByLabelText("VM ID range")).toHaveValue("10000-10999");
    const form = { ...formOf(), vmIDRange: "10000-10999" };
    expect(settings(form).workloadVMIDRange).toStrictEqual({ start: 10_000, end: 10_999 });
    expect(settings(formOf()).workloadVMIDRange).toBeUndefined();
    expect(formOf({ ...settings(form), workloadVMIDRange: { start: 100, end: 200 } }).vmIDRange).toBe("100-200");
    for (const range of ["2000-1000", "99-1000", "1000-1000000000", "1000"]) {
      expect(setupFieldProblems({ ...form, vmIDRange: range }).vmIDRange).toBeDefined();
    }
    expect(setupFieldProblems(form).vmIDRange).toBeUndefined();
  });

  it("take a time zone", async () => {
    const user = userEvent.setup({ delay: null });
    render(<Fields />);
    await user.type(screen.getByLabelText("Time zone"), "Europe/Zurich");
    expect(screen.getByLabelText("Time zone")).toHaveValue("Europe/Zurich");
  });

  it("let the server judge a time zone, as saving does", async () => {
    const refusal = 'Unknown time zone "Europe/zurich". Use an IANA name, such as Europe/Zurich.';
    const calls = fakeServer(
      { "POST /api/setup/time-zone": { status: 422, body: { error: refusal } } },
      { "X-Request-Id": "5698a75970c120c4" },
    );
    const { result, rerender } = renderHook(({ zone }) => useTimeZoneProblem(zone), { initialProps: { zone: "" } });
    expect(result.current).toBeUndefined();
    rerender({ zone: "Europe/zurich" });
    // The field shows the server's words without the request id.
    await waitFor(() => expect(result.current).toBe(refusal));
    expect(calls.map((c) => c.body)).toStrictEqual([{ timeZone: "Europe/zurich" }]);
    fakeServer({ "POST /api/setup/time-zone": { body: {} } });
    rerender({ zone: "Factory" });
    await waitFor(() => expect(result.current).toBeUndefined());
  });
});

describe("a cluster copied into the wizard", () => {
  it("keeps the load balancer it had, whatever the address range says", () => {
    const source = {
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
      controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
      workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
      workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
      variables: {},
      pool: "/pool/kubehenge/default.web",
      allowed: { change: true, kubeconfig: true },
    } satisfies Cluster;
    expect([fromCluster(source, newForm()).loadBalancerPicked, newForm().loadBalancerPicked]).toStrictEqual([
      true,
      false,
    ]);
  });
});

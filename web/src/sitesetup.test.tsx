import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState, type JSX } from "react";
import { describe, expect, it, vi } from "vitest";

import { newForm } from "./create-form";
import { fakeServer } from "./fake-server";
import { AdvancedFields, formOf, type SetupForm } from "./Setup";
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
    await expect(screen.findByText("Replaces nodes: 2 now, 1 at window.")).resolves.toBeInTheDocument();
    expect(calls.find((c) => c.key === "POST /api/setup/site-ca-roll")?.body).toStrictEqual({ siteCA: `${pem}\n` });
  });

  it("says what clearing the site CA replaces", async () => {
    const user = userEvent.setup({ delay: null });
    const calls = fakeServer({
      "POST /api/setup/site-ca-roll": { body: { inherits: 3, now: 3, windows: 0, waiting: 0 } },
    });
    render(<Fields />);
    await user.click(screen.getByLabelText("Site CA"));
    await user.paste(pem);
    await expect(screen.findByText("Replaces nodes: 3 now.")).resolves.toBeInTheDocument();
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

  it("take a time zone", async () => {
    const user = userEvent.setup({ delay: null });
    render(<Fields />);
    await user.type(screen.getByLabelText("Time zone"), "Europe/Zurich");
    expect(screen.getByLabelText("Time zone")).toHaveValue("Europe/Zurich");
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

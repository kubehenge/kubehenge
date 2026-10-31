import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { JSX } from "react";
import { describe, expect, it } from "vitest";

import { fakeServer, held, reads, type Answer } from "./fake-server";
import { FleetProvider, useFleet } from "./fleet";
import type { Cluster } from "./types";

const cluster = (name: string, deleting = false): Cluster => ({
  namespace: "default",
  name,
  class: "kubehenge-kubeadm",
  distribution: "kubeadm",
  version: "v1.36.1",
  generation: 1,
  created: new Date().toISOString(),
  deleting,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [],
  variables: {},
  pool: `/pool/kubehenge/default.${name}`,
  allowed: { change: true, kubeconfig: true },
});

function Listed({ shown }: { shown: Cluster }): JSX.Element {
  const fleet = useFleet();
  return (
    <>
      <ul>
        {fleet?.clusters?.map((c) => (
          <li key={c.name}>{`${c.name}${c.deleting ? " deleting" : ""}`}</li>
        ))}
      </ul>
      <button type="button" onClick={() => fleet?.show(shown)}>
        Show
      </button>
    </>
  );
}

// firstThen answers the first request at once and every later one as `then` does.
function firstThen(first: Answer, then: () => Promise<Answer>): () => Promise<Answer> {
  let asked = 0;
  return async () => (asked++ === 0 ? first : then());
}

// Rereads offers the fleet's read beside showing the cluster as being deleted.
function Rereads(): JSX.Element {
  const fleet = useFleet();
  return (
    <>
      <Listed shown={cluster("web", true)} />
      <button
        type="button"
        onClick={() => {
          void fleet?.reload();
        }}
      >
        Read
      </button>
    </>
  );
}

describe("a cluster a request returned", () => {
  it("shows in the fleet before the fleet is read again", async () => {
    const calls = fakeServer({
      "GET /api/clusters": { body: [cluster("web")] },
      "GET /api/fleet/health": { body: [] },
      "GET /api/runs": { body: [] },
    });
    render(
      <FleetProvider>
        <Listed shown={cluster("db")} />
      </FleetProvider>,
    );
    await screen.findByText("web");
    await userEvent.click(screen.getByRole("button", { name: "Show" }));
    expect(screen.getByText("db")).toBeInTheDocument();
    expect(reads(calls, "GET /api/clusters")).toBe(1);
  });

  it("replaces the one it was, and a read asked before cannot take it back", async () => {
    const late = held();
    fakeServer({
      "GET /api/clusters": firstThen({ body: [cluster("web")] }, late.route),
      "GET /api/fleet/health": { body: [] },
      "GET /api/runs": { body: [] },
    });
    render(
      <FleetProvider>
        <Rereads />
      </FleetProvider>,
    );
    await screen.findByText("web");
    await userEvent.click(screen.getByRole("button", { name: "Read" }));
    await userEvent.click(screen.getByRole("button", { name: "Show" }));
    expect(screen.getByText("web deleting")).toBeInTheDocument();
    late.answer({ body: [cluster("web")] });
    await expect(screen.findByText("web deleting")).resolves.toBeInTheDocument();
    expect(screen.queryByText("web")).not.toBeInTheDocument();
  });
});

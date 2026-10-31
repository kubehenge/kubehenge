import { act, render, screen, waitFor } from "@testing-library/react";
import type { JSX } from "react";
import { describe, expect, it, vi } from "vitest";

import { fakeServer, reads } from "./fake-server";
import { FleetProvider, useFleet } from "./fleet";

// A stream of the server's events that the test sends on.
class FakeEventSource extends EventTarget {
  static readonly CLOSED = 2;
  static opened: FakeEventSource[] = [];
  readyState = 1;
  readonly url: string;
  constructor(url: string) {
    super();
    this.url = url;
    FakeEventSource.opened.push(this);
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

// streams has the page open fake streams, and returns those it opens.
function streams(): FakeEventSource[] {
  FakeEventSource.opened = [];
  vi.stubGlobal("EventSource", FakeEventSource);
  return FakeEventSource.opened;
}

const fleetRoutes = {
  "GET /api/clusters": { body: [] },
  "GET /api/fleet/health": { body: [] },
  "GET /api/runs": { body: [] },
};

function Count(): JSX.Element {
  return <p>{`${useFleet()?.clusters?.length ?? "no"} clusters`}</p>;
}

const shownFleet = async (): Promise<() => void> => {
  const shown = render(
    <FleetProvider>
      <Count />
    </FleetProvider>,
  );
  await screen.findByText("0 clusters");
  return shown.unmount;
};

const tell = (stream: FakeEventSource | undefined): void => {
  act(() => {
    stream?.dispatchEvent(new Event("clusters"));
  });
};

describe("the server's word of a change", () => {
  it("has the fleet read again at once", async () => {
    const opened = streams();
    const calls = fakeServer(fleetRoutes);
    await shownFleet();
    expect(opened.map((s) => s.url)).toStrictEqual(["/api/events"]);

    tell(opened[0]);
    await waitFor(() => expect(reads(calls, "GET /api/clusters")).toBe(2));
    expect(reads(calls, "GET /api/runs")).toBe(2);
  });

  it("is not listened for before there is a fleet", () => {
    const opened = streams();
    render(
      <FleetProvider reads={false}>
        <Count />
      </FleetProvider>,
    );
    expect(opened).toHaveLength(0);
  });

  it("stops being listened for when nothing shows the fleet", async () => {
    const opened = streams();
    fakeServer(fleetRoutes);
    const unmount = await shownFleet();
    unmount();
    expect(opened[0]?.readyState).toBe(FakeEventSource.CLOSED);
  });

  it("waits while the tab is hidden", async () => {
    const opened = streams();
    vi.spyOn(document, "hidden", "get").mockReturnValue(true);
    const calls = fakeServer(fleetRoutes);
    await shownFleet();
    tell(opened[0]);
    expect(reads(calls, "GET /api/clusters")).toBe(1);
  });
});

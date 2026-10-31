import { render, screen } from "@testing-library/react";
import type { JSX } from "react";
import { describe, expect, it } from "vitest";

import { api } from "./api";
import { fakeServer } from "./fake-server";
import { pollCache, pollIdle, usePoll, type Kept } from "./ui";

const cache = pollCache<string>();
// A race of nothing never settles: a read that is still out.
const unanswered = (): Promise<string> => Promise.race<string>([]);
const answered = (): Promise<string> => Promise.resolve("Read before");

function Shown({ load, kept }: { load: () => Promise<string>; kept: Kept<string> }): JSX.Element {
  const { data } = usePoll(load, pollIdle, kept);
  return <p>{data ?? "Nothing"}</p>;
}

describe("a view's last answer", () => {
  it("shows at once when the view opens again, while it is read again", async () => {
    const kept = { cache, key: "web" };
    const first = render(<Shown load={answered} kept={kept} />);
    await screen.findByText("Read before");
    first.unmount();

    render(<Shown load={unanswered} kept={kept} />);
    expect(screen.getByText("Read before")).toBeInTheDocument();
  });

  it("shows the seed before anything is kept", () => {
    render(<Shown load={unanswered} kept={{ cache, key: "db", seed: "From the list" }} />);
    expect(screen.getByText("From the list")).toBeInTheDocument();
  });

  it("is gone after signing out", async () => {
    fakeServer({ "DELETE /api/session": { status: 204 } });
    const kept = { cache, key: "web" };
    const first = render(<Shown load={answered} kept={kept} />);
    await screen.findByText("Read before");
    first.unmount();

    await api.signOut();
    render(<Shown load={unanswered} kept={kept} />);
    expect(screen.getByText("Nothing")).toBeInTheDocument();
  });
});

import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { api } from "./api";
import { fakeServer } from "./fake-server";
import { ErrorBox, errorMessage } from "./ui";

const httpBadGateway = 502;

// failure is what an error box is given for a failed call whose answer has body and headers.
async function failure(body: unknown, headers: Record<string, string> = {}): Promise<string> {
  fakeServer({ "GET /api/clusters": { status: httpBadGateway, body } }, headers);
  try {
    await api.clusters();
  } catch (err) {
    return errorMessage(err);
  }
  throw new Error("the call did not fail");
}

describe("error references", () => {
  it("shows the request's id under the message", async () => {
    render(
      <ErrorBox message={await failure({ error: "the management cluster did not answer", requestId: "0a1b2c" })} />,
    );
    const box = screen.getByRole("alert");
    expect(box).toHaveTextContent("The management cluster did not answer.");
    expect(box).toHaveTextContent("Reference: 0a1b2c");
  });

  it("takes the id from the header when the body has none", async () => {
    render(<ErrorBox message={await failure({ error: "not found" }, { "X-Request-Id": "ffee01" })} />);
    expect(screen.getByRole("alert")).toHaveTextContent("Reference: ffee01");
  });

  it("shows no reference without an id", async () => {
    render(<ErrorBox message={await failure({ error: "not found" })} />);
    const box = screen.getByRole("alert");
    expect(box).toHaveTextContent("Not found.");
    expect(box).not.toHaveTextContent("Reference");
  });
});

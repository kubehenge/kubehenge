import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { OIDCProviders, oidcProblems, parseScopes } from "./OIDCProviders";
import type { OIDCProvider } from "./types";

describe("identity providers", () => {
  it("keeps the scopes as typed, commas and spaces included", async () => {
    const changes: OIDCProvider[][] = [];
    const onChange = vi.fn<(v: OIDCProvider[]) => void>((v) => {
      changes.push(v);
    });
    const user = userEvent.setup({ delay: null });
    render(<OIDCProviders initial={[{ issuerURL: "https://sso.example", clientID: "k8s" }]} onChange={onChange} />);
    const scopes = screen.getByLabelText("Scopes");
    expect(scopes).toHaveValue("");
    expect(scopes).toHaveAttribute("placeholder", "email, profile");
    await user.type(scopes, "email, groups");
    expect(scopes).toHaveValue("email, groups");
    expect(changes.at(-1)?.[0]?.scopes).toStrictEqual(["email", "groups"]);
  });

  it("parses scopes and finds the same issuer twice, spaces or not", () => {
    expect(parseScopes(" email,profile  groups ")).toStrictEqual(["email", "profile", "groups"]);
    expect(
      oidcProblems([
        { issuerURL: "https://sso.example", clientID: "a" },
        { issuerURL: "https://sso.example ", clientID: "b" },
      ]),
    ).toStrictEqual(["Identity provider 2: that issuer is listed already."]);
  });
});

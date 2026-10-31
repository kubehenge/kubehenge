import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fakeServer } from "./fake-server";
import { readTheme, setTheme } from "./theme";

// serve starts each test from the system's theme, whatever the one before it picked.
function serve(): void {
  setTheme("system");
  window.history.replaceState(null, "", "/");
  fakeServer({
    "GET /api/session": { body: { user: "alice@pve", csrf: "c1" } },
    "GET /api/clusters": { body: [] },
    "GET /api/runs": { body: [] },
  });
}

describe("the theme", () => {
  it("goes round the system's, light and dark from the top bar, and keeps the choice", async () => {
    serve();
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Theme: System" }));
    expect(document.documentElement.dataset["theme"]).toBe("light");
    await user.click(screen.getByRole("button", { name: "Theme: Light" }));
    expect(document.documentElement.dataset["theme"]).toBe("dark");
    expect(readTheme()).toBe("dark");
    await user.click(screen.getByRole("button", { name: "Theme: Dark" }));
    expect(document.documentElement.dataset["theme"]).toBeUndefined();
    expect(localStorage.getItem("kubehenge-theme")).toBeNull();
  });

  it("is picked from the palette too", async () => {
    serve();
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await screen.findByRole("button", { name: "Theme: System" });
    await user.keyboard("{Control>}k{/Control}");
    await user.type(await screen.findByRole("combobox"), "dark theme");
    await user.keyboard("{Enter}");
    expect(document.documentElement.dataset["theme"]).toBe("dark");
    expect(screen.getByRole("button", { name: "Theme: Dark" })).toBeInTheDocument();
    setTheme("system");
  });
});

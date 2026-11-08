import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { Said } from "./ui";

const shown = (text: string): { html: string; text: string | null } => {
  const { container } = render(<Said text={text} />);
  return { html: container.innerHTML, text: container.textContent };
};

describe("a message the server wrote", () => {
  it("sets one backtick span as code", () => {
    expect(shown("Run `qm unlock 100` first.").html).toBe("Run <code>qm unlock 100</code> first.");
  });

  it("sets two spans as code and keeps the text between", () => {
    expect(shown("Use `a` or `b`.").html).toBe("Use <code>a</code> or <code>b</code>.");
  });

  it("leaves an unpaired backtick as it is", () => {
    expect(shown("It's a `stray one").html).toBe("It's a `stray one");
    expect(shown("`a` and `b").html).toBe("<code>a</code> and `b");
  });

  it("shows text without backticks as it is", () => {
    expect(shown("Nothing marked.").html).toBe("Nothing marked.");
    expect(shown("").text).toBe("");
  });
});

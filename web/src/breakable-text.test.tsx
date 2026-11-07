import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { NodeTags } from "./PoolNodes";
import { BreakableText } from "./ui";

describe("labels and taints in narrow cells", () => {
  it("allow a line break after each separator and nowhere else", () => {
    const { container } = render(<BreakableText text="dedicated=db:NoSchedule/x" />);
    expect(container.innerHTML).toBe("dedicated=<wbr>db:<wbr>NoSchedule/<wbr>x");
    expect(container.textContent).toBe("dedicated=db:NoSchedule/x");
  });

  it("add no break to text without a separator", () => {
    const { container } = render(<BreakableText text="team" />);
    expect(container.innerHTML).toBe("team");
  });

  it("mark the breaks of a pool's tags", () => {
    const { container } = render(
      <NodeTags
        nodes={{
          labels: [{ key: "team", value: "a" }],
          taints: [{ key: "dedicated", value: "db", effect: "NoSchedule" }],
        }}
      />,
    );
    const tags = [...container.querySelectorAll("li")].map((li) => li.innerHTML);
    expect(tags).toStrictEqual(["team=<wbr>a", "dedicated=<wbr>db:<wbr>NoSchedule"]);
  });
});

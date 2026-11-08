import { describe, expect, it } from "vitest";

import { coresText } from "./create-form";
import { sizeProblem } from "./pool-machine";

describe("counting cores", () => {
  it("says core for one and cores for the rest", () => {
    expect([1, 2, 4].map((n) => coresText(n))).toStrictEqual(["1 core", "2 cores", "4 cores"]);
  });

  it("agrees in the least a pool takes", () => {
    const size = { cores: 0, memoryMiB: 2048, diskGiB: 20 };
    expect(sizeProblem(size, { ...size, cores: 1 })).toBe("At least 1 core");
    expect(sizeProblem(size, { ...size, cores: 2 })).toBe("At least 2 cores");
  });
});

import type { TestingLibraryMatchers } from "@testing-library/jest-dom/matchers";

type DomMatchers<R> = TestingLibraryMatchers<unknown, R>;

// jest-dom's own vitest entry augments the one-parameter Assertion of older vitest releases, which
// vitest 5's two-parameter one refuses. Assertion and the asymmetric matchers both extend Matchers.
declare module "vitest" {
  // A merged declaration repeats every type parameter, T included, and adds its members by extending.
  // oxlint-disable-next-line typescript/no-empty-interface, typescript/no-empty-object-type
  interface Matchers<R extends void | Promise<void> = void | Promise<void>, T = unknown> extends DomMatchers<R> {}
}

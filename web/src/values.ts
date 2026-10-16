// Readers for what a cluster's variables hold. They are whatever its Cluster object carries, which
// kubectl changes as well as this interface, so a value of another shape reads as absent.

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** record is an object's fields, or none when the value is no object. */
export const record = (v: unknown): Record<string, unknown> => (isRecord(v) ? v : {});

/** records are the objects in a list, or none when the value is no list. */
export const records = (v: unknown): Record<string, unknown>[] =>
  Array.isArray(v) ? v.filter((x): x is Record<string, unknown> => isRecord(x)) : [];

export const text = (v: unknown): string | undefined => (typeof v === "string" ? v : undefined);

export const flag = (v: unknown): boolean | undefined => (typeof v === "boolean" ? v : undefined);

export const quantity = (v: unknown): number | undefined => (typeof v === "number" ? v : undefined);

export const texts = (v: unknown): string[] | undefined =>
  Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : undefined;

export const quantities = (v: unknown): number[] | undefined =>
  Array.isArray(v) ? v.filter((x): x is number => typeof x === "number") : undefined;

/** oneOf tells whether a value is one of the choices, such as the options of a select. */
export function oneOf<T extends string>(choices: readonly T[], v: unknown): v is T {
  return choices.some((c) => c === v);
}

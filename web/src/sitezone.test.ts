import { describe, expect, it } from "vitest";

import { newForm, siteZoneOf, withSiteZone } from "./create-form";
import type { ClusterClass } from "./types";
import { browserZone, defaultZone, setSiteZone } from "./ui";

const classWith = (annotations: Record<string, string>): ClusterClass => ({
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: { pattern: ".*" },
  annotations,
  variables: [],
});

describe("the site time zone", () => {
  it("is the classes' annotation, empty when they carry none", () => {
    expect(siteZoneOf([])).toBe("");
    expect(siteZoneOf([classWith({})])).toBe("");
    expect(
      siteZoneOf([classWith({ "kubehenge.dev/time-zone": "" }), classWith({ "kubehenge.dev/time-zone": "UTC" })]),
    ).toBe("UTC");
    expect(siteZoneOf([classWith({ "kubehenge.dev/time-zone": "Europe/Zurich" })])).toBe("Europe/Zurich");
  });

  it("is where a new schedule starts, the browser's until the site sets one", () => {
    expect(defaultZone()).toBe(browserZone());
    setSiteZone("Europe/Zurich");
    expect(defaultZone()).toBe("Europe/Zurich");
    setSiteZone("");
    expect(defaultZone()).toBe(browserZone());
  });

  it("moves the schedules still in the starting zone and leaves the ones the user set", () => {
    const form = newForm();
    const was = form.etcdFields.timeZone;
    const moved = withSiteZone(form, was, "Asia/Tokyo");
    expect(moved.etcdFields.timeZone).toBe("Asia/Tokyo");
    expect(moved.backupFields.timeZone).toBe("Asia/Tokyo");
    const own = withSiteZone(
      { ...form, etcdFields: { ...form.etcdFields, timeZone: "America/Lima" } },
      was,
      "Asia/Tokyo",
    );
    expect(own.etcdFields.timeZone).toBe("America/Lima");
    expect(own.backupFields.timeZone).toBe("Asia/Tokyo");
  });
});

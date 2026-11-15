import { describe, expect, it } from "vitest";

import { backupFieldsOf } from "./Backups";
import { fieldsOf } from "./ClusterUpgrades";
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

  it("starts the settings of a cluster that has none in the site's zone, and keeps what is in effect", () => {
    setSiteZone("Europe/Zurich");
    try {
      expect(backupFieldsOf({ enabled: false }).timeZone).toBe("Europe/Zurich");
      expect(backupFieldsOf({ enabled: true }).timeZone).toBe("UTC");
      expect(backupFieldsOf({ enabled: true, timeZone: "Asia/Tokyo" }).timeZone).toBe("Asia/Tokyo");
      expect(fieldsOf({ mode: "off", nodeImages: false }).timeZone).toBe("Europe/Zurich");
      expect(fieldsOf({ mode: "patch" }).timeZone).toBe("UTC");
      expect(fieldsOf({ mode: "patch", window: { timeZone: "Asia/Tokyo" } }).timeZone).toBe("Asia/Tokyo");
    } finally {
      setSiteZone("");
    }
  });
});

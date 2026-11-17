import { describe, expect, it } from "vitest";

import { backupFieldsOf } from "./Backups";
import { fieldsOf } from "./ClusterUpgrades";
import { newForm, withSiteZone } from "./create-form";
import { etcdFieldsOf } from "./EtcdSnapshots";
import type { ClusterClass } from "./types";
import { browserZone, siteZoneOf } from "./ui";

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

  it("starts a new form in the browser's zone", () => {
    expect(newForm().siteZone).toBe(browserZone());
  });

  it("moves the schedules still in the starting zone and leaves the ones the user set", () => {
    const form = newForm();
    const moved = withSiteZone(form, "Asia/Tokyo");
    expect(moved.siteZone).toBe("Asia/Tokyo");
    expect(moved.etcdFields.timeZone).toBe("Asia/Tokyo");
    expect(moved.backupFields.timeZone).toBe("Asia/Tokyo");
    const own = withSiteZone({ ...form, etcdFields: { ...form.etcdFields, timeZone: "America/Lima" } }, "Asia/Tokyo");
    expect(own.etcdFields.timeZone).toBe("America/Lima");
    expect(own.backupFields.timeZone).toBe("Asia/Tokyo");
  });

  it("starts the settings of a cluster that has none in the zone given, and keeps what is in effect", () => {
    expect(backupFieldsOf({ enabled: false }, "Europe/Zurich").timeZone).toBe("Europe/Zurich");
    expect(backupFieldsOf({ enabled: true }, "Europe/Zurich").timeZone).toBe("UTC");
    expect(backupFieldsOf({ enabled: true, timeZone: "Asia/Tokyo" }, "Europe/Zurich").timeZone).toBe("Asia/Tokyo");
    expect(etcdFieldsOf({ enabled: false }, "Europe/Zurich").timeZone).toBe("Europe/Zurich");
    expect(fieldsOf({ mode: "off", nodeImages: false }, "Europe/Zurich").timeZone).toBe("Europe/Zurich");
    expect(fieldsOf({ mode: "patch" }, "Europe/Zurich").timeZone).toBe("UTC");
    expect(fieldsOf({ mode: "patch", window: { timeZone: "Asia/Tokyo" } }, "Europe/Zurich").timeZone).toBe(
      "Asia/Tokyo",
    );
  });
});

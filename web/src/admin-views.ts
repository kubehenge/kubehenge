/** The pages of Administration, each a few things an administrator looks after together. */
export const adminViews = [
  { path: "", label: "Management cluster", words: "management settings apply remove" },
  { path: "appliance", label: "Appliance", words: "appliance upgrades repair credentials" },
  { path: "backups", label: "Backups", words: "backups etcd snapshots restore management" },
  { path: "templates", label: "Templates", words: "templates vm build refresh" },
  { path: "certificates", label: "Certificates", words: "certificates dns-01 token" },
] as const;

export type AdminView = (typeof adminViews)[number]["path"];

export const adminPath = (view: AdminView): string => (view === "" ? "/administration" : `/administration/${view}`);

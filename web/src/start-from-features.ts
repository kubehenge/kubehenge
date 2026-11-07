import type { Form } from "./create-form";
import { etcdFieldsOf, etcdSnapshotsOf } from "./EtcdSnapshots";
import { logsOf } from "./Logs";
import { oidcProviders } from "./OIDCProviders";
import { podSecurityOf } from "./PodSecurity";
import type { Cluster } from "./types";
import { flag, record, text } from "./values";

/** Features are the add-ons, security and etcd settings a new cluster takes from another. */
export type Features = Pick<
  Form,
  | "cni"
  | "headlamp"
  | "headlampView"
  | "monitoring"
  | "logs"
  | "podSecurity"
  | "cis"
  | "istio"
  | "rancherRegister"
  | "oidc"
  | "etcdSnapshots"
  | "etcdFields"
>;

/** featuresOf reads the features of a cluster's variables v, with base for what v leaves unset. */
export function featuresOf(v: Cluster["variables"], base: Features): Features {
  const headlamp = record(v["headlamp"]);
  // A cluster that never had etcd snapshots set leaves a new one at the default.
  const etcd = v["etcdSnapshots"] === undefined ? undefined : etcdSnapshotsOf(v["etcdSnapshots"]);
  return {
    cni: text(v["cni"]) ?? "",
    headlamp: flag(headlamp["enabled"]) === true,
    headlampView: text(headlamp["access"]) === "view",
    monitoring: flag(record(v["monitoring"])["enabled"]) === true,
    logs: logsOf(v["logs"]),
    podSecurity: podSecurityOf(v["podSecurity"]),
    cis: flag(v["cis"]) === true,
    istio: flag(record(v["istio"])["enabled"]) === true,
    rancherRegister: flag(record(v["rancher"])["register"]) !== false,
    oidc: oidcProviders(v["oidc"]),
    etcdSnapshots: etcd?.enabled ?? base.etcdSnapshots,
    etcdFields: etcd === undefined ? base.etcdFields : etcdFieldsOf(etcd),
  };
}

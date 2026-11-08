import { alertingOf } from "./Alerting";
import type { Form } from "./create-form";
import { etcdQuotaOf } from "./EtcdSize";
import { etcdFieldsOf, etcdSnapshotsOf } from "./EtcdSnapshots";
import { logsOf } from "./Logs";
import { labelsLines, labelsOf } from "./namespace-labels";
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
  | "alerting"
  | "logs"
  | "podSecurity"
  | "cis"
  | "etcdQuotaGiB"
  | "istio"
  | "rancherRegister"
  | "oidc"
  | "etcdSnapshots"
  | "etcdFields"
  | "namespaceLabels"
  | "existingCertManager"
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
    alerting: alertingOf(v["monitoring"]),
    logs: logsOf(v["logs"]),
    podSecurity: podSecurityOf(v["podSecurity"]),
    cis: flag(v["cis"]) === true,
    etcdQuotaGiB: etcdQuotaOf(v["etcd"]),
    istio: flag(record(v["istio"])["enabled"]) === true,
    rancherRegister: flag(record(v["rancher"])["register"]) !== false,
    oidc: oidcProviders(v["oidc"]),
    etcdSnapshots: etcd?.enabled ?? base.etcdSnapshots,
    etcdFields: etcd === undefined ? base.etcdFields : etcdFieldsOf(etcd),
    namespaceLabels: labelsLines(labelsOf(v["namespaceLabels"])),
    existingCertManager: flag(record(v["useExisting"])["certManager"]) === true,
  };
}

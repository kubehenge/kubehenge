// The interface's API, as pkg/ui serves it.

export interface Realm {
  realm: string;
  type: string;
  comment?: string;
  default?: boolean;
}

export interface SecondFactor {
  totp: boolean;
  yubico: boolean;
  recovery?: number[];
}

export interface Session {
  user: string;
  csrf: string;
  secondFactor?: SecondFactor;
  /** Port of the shared Headlamp on this host name, when one runs. */
  headlamp?: number;
  /** Whether the user may create clusters: VM.Allocate on the workload pool. */
  canCreate?: boolean;
  /** Whether the user may install Rancher and trust identity providers of their own: Sys.Modify on the workload pool. */
  canTrust?: boolean;
  /** Whether the user holds Sys.Audit on /, which a cluster's token gets with the HA group labels. */
  canAudit?: boolean;
  /** Whether the user holds Permissions.Modify on /, which giving a cluster's CSI plugin root@pam needs. */
  canModifyPermissions?: boolean;
  /** The namespace the interface creates clusters in unless the creator chooses another. */
  namespace?: string;
  /** Every namespace the interface creates and changes clusters in, namespace first. */
  namespaces?: string[];
}

/** A cluster, by where it lives. */
export interface ClusterRef {
  namespace: string;
  name: string;
}

/** What the interface tells of a credential it stores: never its values. */
export interface StoredCredential {
  purpose: string;
  set: boolean;
  /** The Proxmox VE user who last stored it, and when. */
  setBy?: string;
  setAt?: string;
  /** Whether the user may store, replace or remove it. */
  canChange: boolean;
}

export interface Schema {
  type?: string;
  format?: string;
  description?: string;
  default?: unknown;
  enum?: unknown[];
  minimum?: number;
  maximum?: number;
  multipleOf?: number;
  properties?: Record<string, Schema>;
  required?: string[];
}

export interface ClassVariable {
  name: string;
  required: boolean;
  schema: Schema;
}

export interface ClusterClass {
  name: string;
  distribution: string;
  variables: ClassVariable[];
  annotations?: Record<string, string>;
  versionFormat: { pattern: string };
  /** Why no cluster of the class can be built yet: the template its machines clone is missing. */
  noTemplate?: string;
}

export interface Condition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  since: string;
}

export interface Replicas {
  desired: number;
  ready: number;
  available: number;
  upToDate: number;
}

export interface WorkerPool {
  name: string;
  class: string;
  replicas: number;
}

export interface Machine {
  name: string;
  node?: string;
  /** The Proxmox VE node its VM runs on, where the user may see the VM. */
  proxmoxNode?: string;
  role: "control-plane" | "worker";
  version?: string;
  phase: string;
  ready: boolean;
  /** What keeps it from being ready, as Cluster API sums it up. */
  waiting?: string;
  addresses?: string[];
  created: string;
}

/** A section of the health page from one source: what was read, or why it was not. */
export interface HealthSection<T> {
  items: T[];
  error?: string;
  /** The list stopped at the interface's limit. */
  truncated?: boolean;
}

export type Severity = "error" | "warning" | "progress";

export interface Attention {
  severity: Severity;
  /** The part of the page with the details. */
  section: string;
  what: string;
  message?: string;
  since?: string;
  pod?: { namespace: string; name: string };
}

export interface HealthVM {
  id: number;
  node: string;
  status: string;
  /** The share of its cores in use, from 0 to 1. */
  cpu: number;
  cores: number;
  memory: number;
  maxMemory: number;
  /** Seconds. */
  uptime: number;
  /** The CPU load of the last hour, a point a minute; null where it did not run. */
  load?: (number | null)[];
}

export interface HealthNode {
  name: string;
  machine?: string;
  role: "control-plane" | "worker";
  pool?: string;
  /** Whether the node was read inside the cluster; the fields up to pods need it. */
  joined: boolean;
  ready: boolean;
  since?: string;
  message?: string;
  pressure?: string[];
  unschedulable?: boolean;
  taints?: string[];
  kubelet?: string;
  /** Millicores the pods on it ask for, of what it offers. */
  cpu?: { requested: number; allocatable: number };
  /** Bytes the pods on it ask for, of what it offers. */
  memory?: { requested: number; allocatable: number };
  pods: number;
  vm?: HealthVM;
}

export interface UnwellPod {
  namespace: string;
  name: string;
  node?: string;
  severity: Severity;
  reason: string;
  message?: string;
  restarts: number;
  since?: string;
  /** The unwell container first. */
  containers: string[];
}

export interface PendingVolume {
  namespace: string;
  name: string;
  storageClass?: string;
  since: string;
}

export interface AddonRelease {
  name: string;
  release: string;
  namespace?: string;
  chart: string;
  version?: string;
  status: string;
  revision?: number;
  ready: boolean;
  message?: string;
  since?: string;
}

export interface PoolState {
  name: string;
  desired: number;
  ready: number;
  available: number;
  upToDate: number;
  rollingOut: boolean;
  message?: string;
}

export interface HealthCheck {
  name: string;
  expected: number;
  healthy: number;
  /** Whether it may replace machines now; it stops when too many fail. */
  allowed: boolean;
  message?: string;
  remediating?: string[];
}

export interface HealthEvent {
  source: "workload" | "management";
  type: string;
  reason: string;
  message: string;
  object: string;
  count: number;
  last: string;
}

export interface ClusterHealth {
  read: string;
  verdict: "healthy" | "changing" | "degraded" | "unreachable";
  attention: Attention[];
  /** Why nothing from inside the cluster shows, when nothing does. */
  workload?: string;
  nodes: HealthSection<HealthNode>;
  pods: HealthSection<UnwellPod>;
  volumes: HealthSection<PendingVolume>;
  addons: HealthSection<AddonRelease>;
  pools: HealthSection<PoolState>;
  controlPlane: { kind?: string; conditions: Condition[]; error?: string };
  healthChecks: HealthSection<HealthCheck>;
  events: HealthSection<HealthEvent>;
  /** Why the nodes show no figures from Proxmox VE, when they show none. */
  vms?: string;
  /** Whether the user may read the pods' logs here: Sys.Modify on the cluster's pool. */
  logs: boolean;
}

export interface PodLogs {
  container: string;
  /** The lines are from the container's last run, as a crash-looping one has none of its own yet. */
  previous: boolean;
  lines: string[];
}

export interface Cluster {
  namespace: string;
  name: string;
  class: string;
  distribution: string;
  version: string;
  created: string;
  createdBy?: string;
  autoUpgraded?: string;
  deleting: boolean;
  phase: string;
  available: boolean;
  conditions: Condition[];
  endpoint?: string;
  pods?: string[];
  services?: string[];
  controlPlane: Replicas;
  workers: Replicas;
  workerPools: WorkerPool[];
  variables: Record<string, unknown>;
  pool: string;
  allowed: { change: boolean; kubeconfig: boolean };
  machines?: Machine[];
  /** The cluster's name in the shared Headlamp. */
  headlamp?: string;
  /** How each peer the cluster lists for Cluster Mesh stands, while it is on. */
  mesh?: MeshPeer[];
  /** Which template builds the nodes run and when they next move; absent for Talos. */
  nodeImage?: NodeImage;
}

/** The VM template builds a cluster's nodes are cloned from. */
export interface NodeImage {
  /** The nodes move to new builds inside the maintenance window. */
  refreshes: boolean;
  /** The nodes run the newest builds offered by then. */
  since: string;
  builds: { template: string; build: string; built: string }[];
  /** The last refresh, as "<build> to <build> at <time>". */
  last?: string;
  /** The coming refresh: the build it takes, none while it is not built yet, and its earliest start. */
  next?: { build?: string; due: string };
}

/** A cluster another lists to link with in a Cluster Mesh; refused says why the two are not linked. */
export interface MeshPeer {
  name: string;
  refused?: string;
}

/** An OpenID Connect provider a cluster's API server accepts ID tokens from. */
export interface OIDCProvider {
  issuerURL: string;
  clientID: string;
  usernameClaim?: string;
  usernamePrefix?: string;
  groupsClaim?: string;
  groupsPrefix?: string;
  scopes?: string[];
  certificateAuthority?: string;
}

export interface Storage {
  storage: string;
  node: string;
  shared: boolean;
  content: string[];
  avail: number;
  total: number;
  usable: boolean;
  /** Whether the user may give a cluster's token Datastore.Allocate there. */
  allocatable: boolean;
}

export interface PveNode {
  node: string;
  online: boolean;
}

/** HARule is an enabled HA node-affinity rule of Proxmox VE, offered as a place for a cluster. */
export interface HARule {
  rule: string;
  comment?: string;
  nodes: string[];
  negative?: boolean;
  strict?: boolean;
  /** The nodes the user sees that the rule keeps to: its own, or every other one for a negative rule. */
  allowed: string[];
}

export interface HARules {
  /** Whether the user may read the rules, which takes Sys.Audit on /, or is offered them. */
  readable: boolean;
  /** The rules are offered by Kubehenge, as an administrator allowed: their names and nodes only. */
  offered?: boolean;
  rules: HARule[];
}

export interface Bridge {
  bridge: string;
  node: string;
  vlanAware: boolean;
  comment?: string;
  usable: boolean;
}

export interface ManagementSettings {
  /** Proxmox VE nodes the management cluster's VMs are placed on in turn. */
  nodes: string[];
  imageDatastore: string;
  vmDatastore: string;
  bridge: string;
  vlan?: number;
  nodeAddresses: string[];
  apiAddress: string;
  gateway: string;
  dnsServers: string[];
  workloadDatastores: string[];
  workloadBridges: string[];
  bootstrapDatastore: string;
  registryMirrors?: Record<string, string[]>;
  controllerImage?: string;
  podSubnets?: string[];
  serviceSubnets?: string[];
  clusterOIDC?: OIDCProvider[];
  /** Proxmox VE HA keeps the clusters' control-plane VMs on separate nodes. */
  proxmoxHA?: boolean;
  /** Those who may create or change a cluster are offered the HA rules' names and nodes without Sys.Audit on /. */
  offerHARules?: boolean;
  /** Clusters may choose the CSI plugin's newest release, with Datastore.Allocate on their storage. */
  clusterDatastoreAllocate?: boolean;
  backupTargets?: BackupTarget[];
  /** The weekly build of the Ubuntu VM templates; absent, it runs with its defaults. */
  templateRefresh?: TemplateRefreshSettings;
  /** Clusters may choose the cloud controller manager's HA group labels, with Sys.Audit on / for their token. */
  clusterSysAudit?: boolean;
  /** Clusters may take volume snapshots, with their CSI plugin signed in as root@pam. */
  clusterVolumeSnapshots?: boolean;
  /** More namespaces the interface creates and manages clusters in, beside its own. */
  extraClusterNamespaces?: string[];
}

/** When and where the Ubuntu VM templates are built again; empty fields take the defaults. */
export interface TemplateRefreshSettings {
  enabled: boolean;
  weekday?: string;
  time?: string;
  timeZone?: string;
  storage?: string;
  address?: string;
  gateway?: string;
  dns?: string[];
  creatorBuilds?: boolean;
}

/** An S3 store clusters back up to, each under a folder of its own beneath the prefix. */
export interface BackupTarget {
  name: string;
  /** The store's https:// URL; none for AWS S3 itself. */
  endpoint?: string;
  bucket: string;
  region?: string;
  prefix?: string;
  caCertificate?: string;
  /** Keeps the AWS SDK from sending the checksums some stores refuse. */
  noChecksums?: boolean;
}

/** A backup Velero made of a cluster, or is making. */
export interface BackupRun {
  name: string;
  phase: string;
  /** Made by the schedule, rather than asked for. */
  scheduled: boolean;
  /** The Proxmox VE user who asked for it. */
  by?: string;
  started?: string;
  completed?: string;
  expires?: string;
  items: number;
  total: number;
  warnings: number;
  errors: number;
  failure?: string;
}

/** A restore of one of a cluster's backups, with how far its volumes have come. */
export interface RestoreRun {
  name: string;
  backup: string;
  phase: string;
  by?: string;
  /** The namespaces it brings back; "*" for every one. */
  namespaces?: string[];
  started?: string;
  completed?: string;
  items: number;
  total: number;
  warnings: number;
  errors: number;
  failure?: string;
  volumes: number;
  volumesDone: number;
  bytesDone: number;
  bytesTotal: number;
}

/** What the backups view shows of a cluster. */
export interface ClusterBackups {
  enabled: boolean;
  target?: string;
  /** False while Velero cannot be read, with the reason in message. */
  reachable: boolean;
  message?: string;
  /** The backup location's phase: Available once Velero reaches the target. */
  location?: string;
  locationMessage?: string;
  lastBackup?: string;
  backups: BackupRun[];
  restores: RestoreRun[];
  /** VM.Allocate on the cluster's pool. */
  canBackUp: boolean;
  /** Sys.Modify on the cluster's pool. */
  canRestore: boolean;
}

export interface Job {
  user: string;
  kind?: "apply" | "remove" | "template";
  started: string;
  finished?: string;
  state: "running" | "succeeded" | "failed";
  error?: string;
  // Null until the job writes its first line.
  log?: string[] | null;
  /** How many lines the whole log has, when log holds only the last of them. */
  lines?: number;
}

export interface SetupStatus {
  appliance: boolean;
  ready: boolean;
  allowed: boolean;
  job?: Job;
  settings?: ManagementSettings;
  /** Whether a Proxmox VE backup job includes the appliance; absent when not known. */
  backup?: boolean;
  /** When the interface's certificate runs out; for administrators only. */
  certificateExpires?: string;
  /** The appliance's guest ID in Proxmox VE; for administrators only. */
  applianceVMID?: number;
  /** The Proxmox VE API's certificate goes unverified (--proxmox-insecure); for administrators only. */
  proxmoxInsecure?: boolean;
  /** An earlier run left its encrypted state, so the setup takes that run's passphrase. */
  resumes?: boolean;
}

export type UpgradePolicy = "off" | "patch" | "minor";

/** The Kubernetes versions a distribution offers: the newest of each minor, newest first. */
export interface Versions {
  /** What a new cluster may start on: the minor versions Kubernetes still patches. */
  versions: string[];
  /** The minor versions before, only the next step of a cluster already on one. */
  older?: string[];
  /** The one its publisher calls stable. */
  stable?: string;
  /** Why there are none to offer; the version is then typed in. */
  error?: string;
}

/** A VM template in Kubehenge's template pool. */
export interface VMTemplate {
  vmid: number;
  node: string;
  name: string;
  tags: string[];
  /** The template a build is one of; a Talos template's own name. */
  line: string;
  /** The build's tag and when it was made; absent for a Talos template. */
  build?: string;
  built?: string;
  /** The newest build of its template. */
  newest?: boolean;
  /** The cluster that clones from it, which keeps it from being deleted. */
  inUse?: string;
}

/** The weekly template build: its schedule, what was asked for, and how the last run went. */
export interface TemplateRefresh {
  weekday: string;
  time: string;
  timeZone: string;
  soakHours: number;
  creatorBuilds: boolean;
  next?: string;
  /** Builds asked for that have not started: "refresh", or a missing template. */
  requested: string[];
  last?: {
    state: "running" | "succeeded" | "failed";
    trigger?: string;
    started?: string;
    finished?: string;
    lines: string[];
    error?: string;
    log: string[];
  };
}

export interface Templates {
  templates: VMTemplate[];
  /** The last template build. */
  job?: Job;
  /** The weekly build, when it is on. */
  refresh?: TemplateRefresh;
}

/** Whether a cluster creator may have a missing template built, and which builds are under way. */
export interface TemplateBuilds {
  allowed: boolean;
  /** Request keys not started yet, such as kubeadm-v1.37.1 or cloud-image. */
  requested: string[];
  building: string[];
}

export interface TemplateRequest {
  /** rke2 is the cloud image RKE2 and k3s share. */
  distribution: "rke2" | "kubeadm";
  kubernetesVersion?: string;
  node: string;
  storage: string;
  importStorage: string;
  isoStorage?: string;
  bridge: string;
  vlan?: number;
  buildAddress?: string;
  buildGateway?: string;
  buildDNS?: string[];
}

export interface Upgrades {
  version: string;
  available: string[];
  error?: string;
  /** The hour is in timeZone, an IANA time zone. */
  settings: { policy: UpgradePolicy; hour: number; timeZone: string };
  status?: {
    state: "updating" | "succeeded" | "current" | "failed";
    from?: string;
    to?: string;
    time: string;
    error?: string;
  };
}

export interface CreateRequest {
  /** One of the session's namespaces; unset, its first. */
  namespace?: string;
  name: string;
  class: string;
  version: string;
  controlPlaneReplicas: number;
  workerReplicas: number;
  variables: Record<string, unknown>;
  pods?: string[];
  services?: string[];
}

export interface ChangeRequest {
  version?: string;
  controlPlaneReplicas?: number;
  workerPools?: Record<string, number>;
  variables?: Record<string, unknown>;
}

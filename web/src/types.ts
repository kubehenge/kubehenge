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
  /** The namespace the interface creates clusters in. */
  namespace?: string;
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
  /** Whether the user may read the rules, which takes Sys.Audit on /. */
  readable: boolean;
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
  /** The cluster that clones from it, which keeps it from being deleted. */
  inUse?: string;
}

export interface Templates {
  templates: VMTemplate[];
  /** The last template build. */
  job?: Job;
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

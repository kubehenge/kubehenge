# Installing Kubehenge

Kubehenge runs as a small appliance on Proxmox VE: an unprivileged LXC container with Kubehenge's web interface. From the interface, an administrator creates the management cluster (a few Talos Linux VMs running Cluster API and Kubehenge's controller), and from then on clusters are created from the interface or with kubectl. Those who keep their infrastructure as code can apply the management cluster with Terraform/OpenTofu instead ([below](#alternative-terraformopentofu)).

1. Check the [requirements](#requirements).
2. [Install the appliance](#install-the-appliance) with one line in the Proxmox VE shell.
3. [Sign in](#sign-in) and [set up the management cluster](#set-up-the-management-cluster).
4. [Create the VM templates](#create-the-vm-templates) clusters are cloned from.

Then create clusters: [clusters.md](clusters.md).

## Requirements

### Proxmox VE

- **Proxmox VE 9.** The installer refuses any other release. Kubehenge is built for x86-64.
- **Root in the shell of one node** to run the installer.
- **An administrator** to set up the management cluster: a user with the Administrator role on `/` ([what it needs](#who-may-set-it-up)).

### Storage

| For | Content type | Notes |
| --- | --- | --- |
| The appliance's disk, 8 GB | `rootdir` | The first active storage that allows it, unless `--storage` names one. |
| The container template | `vztmpl`, on `local` | The installer downloads Debian's standard container template there through `pveam` if it is missing. |
| The Talos disk image, and the images VM templates start from | `import` | Proxmox VE's `local` storage does not allow `import` until you add it (Datacenter, Storage, Edit, Content). |
| The management cluster's disks | `images` | 32 GB per node. |
| Cloud-init images | `iso` | Cluster API keeps each new cluster VM's cloud-init ISO here and gets full control of the storage, so a dedicated one is best. |
| Clusters' disks and persistent volumes | `images` | One or more storages, chosen when the management cluster is set up. |
| VM templates | `images` | Shared storage in a Proxmox VE cluster, so that clones can start on any node. |

### Network and addresses

Addresses are IPv4. Kubehenge gives every node of every cluster a static address, so no DHCP server is needed for them.

- **The appliance** takes one address on a bridge, by DHCP or static (`--ip`). Give it one that does not change (a static `--ip`, or a DHCP reservation), or a DNS name with `--name`: its certificate and the addresses identity providers send people back to name it. Should the address change, [renew the certificate](#the-appliances-certificate).
- **The Proxmox VE API** is reached on port 8006, at a name or address of the Proxmox VE cluster that the API's certificate is valid for ([below](#the-proxmox-ve-api)). The appliance uses it, and passes it on to Cluster API, to Kubehenge's controller and to every cluster's cloud controller manager and CSI plugin. The appliance, the management cluster's nodes and every cluster's nodes must reach it.
- **The management cluster** needs one free address per node (one node, or three for high availability) and one more for the virtual IP of its Kubernetes API, all in one subnet on one bridge, with a gateway and DNS servers. Its VMs can run on several Proxmox VE nodes, which then all need that bridge in the same layer-2 network. The appliance reaches its nodes on ports 50000 (Talos API) and 6443 (Kubernetes API).
- **Each cluster** needs a range of free addresses for its nodes and one more, outside that range, for its control plane's virtual IP, in the same subnet. Leave spare addresses in the range: an upgrade or a node replacement creates the new node before it removes the old one. The management cluster reaches each cluster's virtual IP on port 6443.
- **Pod and service ranges** must stay clear of the nodes' networks. Kubehenge picks clear ranges when none are given, and refuses ranges that overlap.
- **The VM that prepares a template** takes an address by DHCP on its bridge, or a static one given when the template is built ([below](#create-the-vm-templates)).

### Internet access

| Who | Reaches |
| --- | --- |
| The node running the installer | GitHub, for Kubehenge's release and OpenTofu; the container template mirror, through `pveam` |
| The appliance | GitHub, for the release index and upgrades; the OpenTofu registry, the Helm chart repositories and the Talos Image Factory, while it applies the management cluster; while it makes VM templates, the Talos Image Factory (`factory.talos.dev`), Ubuntu's cloud images (`cloud-images.ubuntu.com`, for their signed checksums) and `pkgs.k8s.io` (for its signing key) |
| The Proxmox VE nodes | the Talos Image Factory and Ubuntu's cloud images, which Proxmox VE downloads disk images from |
| The management cluster's nodes | container registries and the Cluster API providers' release files; RKE2's and k3s's release channels, for automatic upgrades, and their releases' checksum files and install scripts on GitHub, read once per release; the add-on chart repositories: `ghcr.io` (the Proxmox VE cloud controller manager and CSI plugin), `helm.cilium.io`, `docs.tigera.io`, `kubernetes-sigs.github.io` (Headlamp), `releases.rancher.com` and `charts.jetstack.io` |
| Clusters' nodes | container registries; on RKE2 and k3s, GitHub (`github.com` and `raw.githubusercontent.com`), for the release and its images, which a node installs when it boots |
| The VM that prepares a template | Ubuntu's and Kubernetes' package mirrors; for kubeadm, `registry.k8s.io`, GitHub (for cosign) and Sigstore's `tuf-repo-cdn.sigstore.dev`, to check the images' signatures |

The management cluster's nodes can pull through registry mirrors instead ([setup](#the-settings), Advanced).

### Sizes

| What | vCPUs | Memory | Disk |
| --- | --- | --- | --- |
| The appliance | 2 | 1 GiB, and 512 MiB swap | 8 GB |
| Each management cluster node (one or three) | 4 | 4 GiB | 32 GB |
| A cluster's control-plane node, by default | 2 | 4 GiB | 30 GiB |
| A cluster's worker, by default | 2 | 4 GiB | 40 GiB |

## Install the appliance

In the shell of the Proxmox VE node that should run it, as root:

```sh
curl -fsSLO https://github.com/kubehenge/kubehenge/releases/latest/download/install.sh && sh install.sh
```

The same with a static address:

```sh
sh install.sh --ip 192.0.2.40/24 --gateway 192.0.2.1
```

`install.sh` carries the version of its release and the SHA-256 checksums of the release archive and of the OpenTofu release it installs, and checks each download against them before using it. To check the script itself before running it, see [Verifying a release](#verifying-a-release).

### Options

| Option | Default | What it does |
| --- | --- | --- |
| `--ctid <id>` | the next free ID | The container's ID. |
| `--storage <storage>` | the first active storage that takes container disks | Where the container's disk goes. |
| `--bridge <bridge>` | `vmbr0` | The bridge the container attaches to. |
| `--proxmox-url <URL>` | the best name or address of this Proxmox VE cluster that the API's certificate is valid for ([below](#the-proxmox-ve-api)) | The Proxmox VE API as the appliance and the clusters' VMs reach it, such as `https://pve.example.org:8006`. Each checks the certificate the API serves, so the name or address has to be one that certificate is valid for; the installer checks that the appliance gets an answer and stops otherwise. |
| `--proxmox-ca <file>` | the Proxmox VE cluster's CA, else the CA in this node's trust store, else the CA the API serves once you say yes to it ([below](#the-proxmox-ve-api)) | The CA that signed the API's certificate, such as your organisation's where it replaced Proxmox VE's own. The file may hold other certificates too, such as a whole trust store: the appliance is given only those the API's certificate rests on. The appliance, the management cluster and the clusters' VMs check the API against it; the installer stops before it creates anything unless the API's certificate is one this CA signed for the URL. |
| `--proxmox-insecure` | off | The appliance, the management cluster (OpenTofu's and Cluster API's Proxmox VE providers, Kubehenge's controller) and every cluster's cloud controller manager and CSI plugin reach the Proxmox VE API without verifying its certificate. The Administration page shows that it is on. Not with `--proxmox-ca`. |
| `--ip dhcp` or `--ip <address>/<prefix>` | `dhcp` | The container's address, such as `192.0.2.40/24`. |
| `--gateway <address>` | none | The default gateway, with a static `--ip`. |
| `--dns <address>,...` | this node's name servers | The name servers the container asks, such as `192.0.2.53,192.0.2.54`: needed where the node's own are not reachable from the container's network, such as a resolver listening only on the node's loopback address. |
| `--hostname <name>` | `kubehenge` | The container's host name, also written into its certificate. |
| `--name <DNS name>` | none | A DNS name, such as `kubehenge.example.org`, that people reach the appliance by instead of its address. It goes into the certificate and into the addresses identity providers send people back to; point it at the appliance's address in your DNS. |
| `--cert <file> --key <file>` | a certificate signed by the Proxmox VE cluster's CA | Serves a certificate of your own instead, such as one from your organisation's CA. It must be valid for `--name` (or the address, without one) and its key must match. |
| `--release <file> --sha256 <hex>` | the published release the script names | Installs a release archive from a file instead, checked against the given checksum. OpenTofu is still downloaded. |
| `--pve-entry` or `--no-pve-entry` | asked | Whether to add a Kubernetes entry to this node's Proxmox VE interface ([below](#the-proxmox-ve-interface-entry)). Without either, the installer asks; run without a terminal, it leaves the entry out. |
| `--upgrade --ctid <id>` | | Upgrades an installed appliance instead ([upgrading.md](upgrading.md#from-the-proxmox-ve-shell)). |
| `--certificate --ctid <id>` | | Replaces an installed appliance's certificate instead ([below](#the-appliances-certificate)), keeping its name unless `--name` gives another or `--no-name` drops it. |

### What it does

1. Checks that it runs as root on a Proxmox VE 9 node, and asks whether to add the Kubernetes entry to this node's Proxmox VE interface, unless `--pve-entry` or `--no-pve-entry` answered already.
2. Downloads Kubehenge's release archive and OpenTofu, and checks both against the checksums it carries. The container template comes through `pveam`, which checks it against its signed index.
3. Chooses the Proxmox VE API's address and the CA its certificate is checked against ([below](#the-proxmox-ve-api)), asking on the terminal where it needs your word, and checks that the API serves a certificate valid for that address.
4. Creates an unprivileged Debian container with nesting enabled, started at boot and tagged `kubehenge`, and waits for its IPv4 address.
5. Makes a certificate for the container's host name, address and `--name`, signed by the Proxmox VE cluster's own CA (self-signed when the node cannot read that CA's key) and valid for 825 days, the longest browsers accept from a private CA; or checks the one given with `--cert`. The CA's key never leaves the node.
6. Installs Kubehenge, OpenTofu, the management-cluster module, the shared Headlamp and the service units into the container; adds the CA the API is checked against to the container's trust store; switches off the template's SSH and mail servers; starts the interface on ports 443 and 8443 (the shared Headlamp) as an unprivileged user.
7. If you said yes, adds the Kubernetes entry to this node's Proxmox VE interface.
8. Prints the address and the certificate's fingerprint:

   ```text
   Kubehenge is running in container 105: https://192.0.2.40/
   Certificate SHA-256 fingerprint: …
   Valid until: …
   Sign in with a Proxmox VE administrator to create the management cluster.
   ```

If a step up to starting the interface fails, the installer removes the container it created, so running it again starts clean. A failure to add the interface entry leaves the working appliance in place and says so.

### The Proxmox VE API

The appliance, the management cluster and every cluster reach the Proxmox VE API at one URL and check the certificate it serves. Without `--proxmox-url`, the installer takes the names and addresses the certificate of this node's API (`/etc/pve/local/pveproxy-ssl.pem`, else `pve-ssl.pem`) is valid for, and keeps only those of this Proxmox VE cluster's nodes: an address of one of them, or a name whose every address is one of theirs. A certificate that also names other hosts, such as one from your organisation's CA covering several machines, never leads the appliance to another host. A wildcard name counts for this node's own name (`hostname -f`) where it covers it. Of what is left, the installer takes, in this order:

1. a name for several of the cluster's nodes, such as a DNS name with the address of each;
2. this node's own name;
3. another name of this node;
4. this node's address on the appliance's bridge, then its other addresses;
5. a name of this node without a domain, which resolves only where a search domain supplies one;
6. a name, then an address, of another node of the cluster.

A name counts as it resolves on this node; the appliance and the clusters' VMs have to resolve it the same way, through DNS. Where more than one is as good as the best, the installer lists them on the terminal and asks which, the best first and the default; without a terminal it takes the best and names it. Where the certificate is valid for none of the cluster's names and addresses, the installer stops and says what it names: give the URL with `--proxmox-url`, by a name or address the certificate is valid for.

The CA the certificate is checked against is, without `--proxmox-ca`:

1. the Proxmox VE cluster's own CA, `/etc/pve/pve-root-ca.pem`, where it signed the certificate;
2. else the CA in this node's trust store (`/etc/ssl/certs/ca-certificates.crt`) the certificate rests on, such as your organisation's CA once it is installed there; the installer names it, and the appliance is given that CA alone, not the whole store;
3. else the CA the API serves with its certificate: its root where it serves one, else the topmost CA it serves, else the certificate itself. The installer shows its subject, issuer and SHA-256 fingerprint and asks whether to trust it; check the fingerprint against your CA before you say yes. Without a terminal it stops, naming the fingerprint: give the CA with `--proxmox-ca`.

Where none of them is the answer, the installer stops before it creates anything. `--proxmox-ca` gives the CA instead; `--proxmox-insecure` has nothing checked.

### The appliance's certificate

The interface warns administrators 30 days before its certificate runs out. Renew it in the shell of the node the appliance runs on (every node of a Proxmox VE cluster can read the cluster's CA key), with the installer of the release the appliance runs or a newer one:

```sh
sh install.sh --certificate --ctid 105
```

The same command makes a new certificate after the appliance's address changed, gives it a DNS name with `--name kubehenge.example.org`, or installs one of your own with `--cert <file> --key <file>`. It keeps a name given before unless `--name` gives another, or `--no-name` drops it for the appliance's address again; it restarts the interface and the shared Headlamp, and prints the new fingerprint. It does not restart while the management cluster is being applied or removed or a VM template is being built; run it again once that has finished. `--upgrade` renews a certificate the installer made once it has less than 60 days left; one of your own it only warns about.

A new name or address changes where identity providers send people back: update the redirect URLs registered for OpenID realms (`https://<name>/api/openid/callback`) and for the shared Headlamp (`https://<name>:8443/oidc-callback`).

### The Proxmox VE interface entry

An optional extra adds a **Kubernetes** button beside Create VM and Create CT, and a **Kubernetes** item under Datacenter, both opening the appliance in a new tab. Kubehenge works the same without it. The appliance's installer asks whether to add it on the node it runs on; each node serves its own interface, so on every other node run the entry's own installer, found in `share/kubehenge/pve-entry/` of the release archive:

```sh
sh install.sh --url https://192.0.2.40
```

A `pve-manager` update replaces the page the entry changes; an apt hook checks after every package run and, when an update removed the entry, puts it back and logs it (`journalctl -t kubehenge-pve-entry`). If a Proxmox VE release changes its interface so the entry no longer fits, the interface works as shipped, without the entry. More in `pve-entry/README.md`.

### Verifying a release

`install.sh` checks what it downloads, and the appliance installs only releases listed in an index signed with the project's release key, so the one thing left to check yourself is the script, before you run it. A release carries the means to check more: each of its files and each image Kubehenge runs was built by the release workflow from the tagged source on a GitHub-hosted runner (SLSA Build Level 3), and carries signed build provenance and an SPDX SBOM, kept with the repository and, for the files, in `kubehenge_<version>.intoto.jsonl` beside them.

With the [GitHub CLI](https://cli.github.com), for the release you install (`<version>` without the `v`):

```sh
gh release download v<version> --repo kubehenge/kubehenge --dir kubehenge-release
cd kubehenge-release
sha256sum -c kubehenge_<version>_SHA256SUMS
for file in install.sh kubehenge_<version>_linux_amd64.tar.gz; do
  gh attestation verify "$file" --repo kubehenge/kubehenge \
    --bundle kubehenge_<version>.intoto.jsonl \
    --signer-workflow kubehenge/kubehenge/.github/workflows/release-build.yml \
    --source-ref refs/tags/v<version> --deny-self-hosted-runners
done
```

The checksums prove the files are the ones the release lists; the attestation proves the release workflow built them, from this repository at that tag, with the steps in `release-build.yml`, which a checksum cannot: a checksum made beside a changed file agrees with it perfectly. `kubehenge_<version>_linux_amd64.spdx.json` lists every file in the archive with its SHA-256 and every package in it at its exact version, for a vulnerability scanner such as Grype or Trivy; add `--predicate-type https://spdx.dev/Document/v2.3` to the command above to check the SBOM attested to the archive instead of its provenance.

The images carry the same provenance and SBOM, kept beside them in the registry, and a Sigstore signature, which cosign checks:

```sh
image=ghcr.io/kubehenge/kubehenge:v<version>
gh attestation verify "oci://$image" --repo kubehenge/kubehenge \
  --signer-workflow kubehenge/kubehenge/.github/workflows/release-build.yml \
  --source-ref refs/tags/v<version> --deny-self-hosted-runners
cosign verify "$image" \
  --certificate-identity https://github.com/kubehenge/kubehenge/.github/workflows/release-build.yml@refs/tags/v<version> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

How releases are built and signed is in [releasing.md](releasing.md).

## Sign in

Open the address the installer printed. Its certificate is signed by the Proxmox VE cluster's CA, so a browser that trusts Proxmox VE's own certificates trusts it too; otherwise, compare the fingerprint with the one the installer printed.

Sign in with a Proxmox VE account, from any realm: Proxmox VE users, PAM, LDAP, Active Directory and OpenID realms all work, and there is no second user directory.

- **Realm and password.** Choose the realm, or type `user@realm` as the user name.
- **Second factor.** An account with TOTP, Yubico OTP or recovery keys is asked for one, as Proxmox VE's own sign-in asks. A WebAuthn key is bound to Proxmox VE's own address, so its users sign in through an OpenID realm or another factor.
- **OpenID realms** send the browser to the identity provider, which returns it to `https://<appliance>/api/openid/callback`. Register that address as a redirect URL with the provider of the realm's client.
- **Sessions** end eight hours after sign-in, however active, and whenever the interface restarts, as an upgrade or a certificate renewal makes it: the interface keeps them in memory only. After five failed attempts within 15 minutes, further sign-ins for that user and from that address are refused for a while.

Every user sees and changes only what their Proxmox VE privileges allow ([clusters.md](clusters.md#who-may-do-what)).

## Set up the management cluster

On first sign-in, an administrator sees the setup page. It creates the management cluster with the `management-cluster/` module and OpenTofu, inside the appliance.

### Who may set it up

The signed-in user needs these privileges on `/`, which the Administrator role holds:

`Sys.Modify`, `Permissions.Modify`, `User.Modify`, `Realm.AllocateUser`, `Pool.Allocate`, `VM.Allocate`, `Datastore.AllocateSpace`, `Datastore.AllocateTemplate`

Proxmox VE checks every call of the run again on its own. Anyone without them sees that Kubehenge is not set up yet. The same privileges open the Administration page later: Kubehenge's upgrades, repair access, the VM templates, the management cluster's settings (applying them again or changing them) and removal. They also bring the warnings about the [backup](#back-up-the-appliance) and the [certificate](#the-appliances-certificate).

The run acts with your own Proxmox VE session: the interface renews your ticket and hands it to OpenTofu through its environment only; it is never stored, and no administrator token is created. A ticket lasts two hours, so the run has to finish within that.

### The settings

| Field | What it is |
| --- | --- |
| Proxmox VE nodes | The nodes the management cluster's VMs run on, taken in turn: with three nodes for three VMs, the management cluster keeps running when one node fails. The storages and bridge below are offered only when every chosen node has them. |
| VM disks | Storage for the management cluster's disks. |
| Talos image | Storage the Talos disk image is downloaded to, on each chosen node; it must allow the `import` content type. |
| Cloud-init images | Storage where Cluster API keeps each new cluster VM's cloud-init ISO; it must allow `iso`. Cluster API gets full control of it. |
| Bridge | Bridge the management cluster's VMs attach to. |
| VLAN tag | Optional, 1 to 4094. |
| Node addresses | One IPv4 address, or three for high availability, each with its prefix length: `192.0.2.11/24`. All in one network, the gateway's, and none of them the network's own or broadcast address. |
| API address | The virtual IP of the management cluster's Kubernetes API: an unused IPv4 address in the nodes' network, neither a node address nor the gateway. It moves to a healthy node when one fails. |
| Gateway | The nodes' default gateway, an IPv4 address. |
| DNS servers | IP addresses, one per line or separated by commas. |
| What clusters may use | The storages and bridges clusters may put their disks on and attach to, from any node; at least one of each. The page says which nodes have each, or that a storage is shared. Cluster API's token gets rights on exactly these, and the ClusterClasses accept no others, from the interface or kubectl. |
| Identity providers | OpenID Connect providers every new cluster trusts unless it names its own ([clusters.md](clusters.md#signing-in-with-openid-connect)). A change applies to clusters created afterwards. |
| Proxmox VE HA | Off by default. When on, Proxmox VE HA keeps each cluster's control-plane VMs on separate nodes and restarts them on another node after a node fails, for every cluster on shared storage that does not opt out ([clusters.md](clusters.md#machines)). See [below](#proxmox-ve-ha) before switching it on. |
| HA rules for cluster creators | Off by default. When on, everyone who may create or change a cluster can place it on the nodes of one of Proxmox VE's HA node-affinity rules without `Sys.Audit` on `/` of their own ([clusters.md](clusters.md#machines)). See [below](#ha-rules-for-cluster-creators). |
| CSI plugin | Off by default. When on, clusters may choose the CSI plugin's newest release ([below](#the-csi-plugins-newest-release)). |
| Advanced: Registry mirrors | `registry=endpoint`, one per line, such as `ghcr.io=https://mirror.example.org:5000`: endpoints the management cluster's nodes try before the registry itself. |
| Advanced: Pod ranges, Service ranges | The management cluster's own. When empty, the first of a few private ranges clear of the nodes' network, the API address and the gateway is taken: `10.244.0.0/16` and `10.96.0.0/12` when they are clear. |
| State passphrase | Encrypts the management cluster's state on the appliance ([below](#the-state-passphrase)); asked for twice on the first run. |

Choose no more Proxmox VE nodes than there are node addresses; an offline node cannot be newly chosen. Once the management cluster runs, its node addresses (more can only be added after them), API address, gateway, pod and service ranges and VM disks stay as they are ([upgrading.md](upgrading.md#in-the-appliance)).

#### Proxmox VE HA

Proxmox VE lets only `Sys.Console` on `/` create HA resources and rules, so with this setting the controller gets a second user and API token holding `Sys.Audit` and `Sys.Console` on `/`. That privilege amounts to a root shell on every node: whoever reads the controller's Secret in the management cluster, or holds its admin kubeconfig, can take over the Proxmox VE cluster. The token is used for HA resources and rules alone, and the controller touches only those it made, but the privilege cannot be narrowed further. Switching the setting on or off takes `Sys.Console` on `/` of you as well; switching it off takes the controller's token away, then removes every HA resource and rule Kubehenge made, with your session. Every node that runs an HA resource arms Proxmox VE's watchdog and restarts itself when it loses quorum.

#### HA rules for cluster creators

Reading Proxmox VE's HA rules takes `Sys.Audit` on `/`, which reaches the whole cluster's configuration, so most people who create clusters lack it and cannot place a cluster by a rule's nodes. With this setting, Kubehenge reads the rules for them.

- **What it grants:** every 30 seconds Kubehenge's controller reads the enabled node-affinity rules and the nodes with its own API token, which holds `Sys.Audit` on `/` without propagation from the start, so Proxmox VE grants nothing new and the HA token is not used. It keeps each rule's name and the nodes it allows in the ConfigMap `kubehenge-ha-rules` beside the ClusterClasses, never a rule's comment, resources or strictness, and leaves out the rules Kubehenge keeps for its own clusters. The interface's account may read that ConfigMap only while the setting is on, and hands its rules to those with `VM.Allocate` on the workload pool or on a cluster's pool, who may create or change a cluster; those who hold `Sys.Audit` on `/` read the rules from Proxmox VE as before.
- **What it risks:** everyone who may create or change a cluster learns the rules' names and which nodes they cover, which might tell them how the nodes are grouped (by rack, hardware or tenant). Name rules with that in mind. Nothing else of Proxmox VE's configuration reaches them.
- **Switching it off** has the controller remove the ConfigMap and takes the interface's access to it away.

#### The CSI plugin's newest release

The CSI plugin's newer releases need `Datastore.Allocate` on a cluster's storage, which covers every volume on it and its definition, so clusters run its last release without it unless this setting is on. With it, a cluster may choose the newest release on a storage of its own ([clusters.md](clusters.md#persistent-volumes)); its token then holds `Datastore.Allocate` there. The setting creates a role, `<name>-cluster-datastore`, holding `Datastore.Allocate`, `Datastore.AllocateSpace` and `Datastore.Audit`, and gives Kubehenge's controller `Datastore.Allocate` on each storage for clusters that the management cluster's disks, its Talos image and the cloud-init images are not on, so that it can grant the role there. Switching it off removes the role, and with it every cluster's grant; those clusters go back to the earlier release.

The appliance keeps the module's other settings at their defaults: the management cluster is named `kubehenge`, its nodes have 4 vCPUs, 4 GiB of memory and a 32 GB disk, and it creates the pools `kubehenge` (clusters), `kubehenge-templates` (VM templates) and `kubehenge-management` (its own VMs).

### The state passphrase

The management cluster's state holds its secrets, among them its admin kubeconfig and talosconfig. The appliance encrypts it with a key derived from the passphrase: at least 16 printable characters, on one line. The passphrase is never stored. Every later apply, the repair access on the Administration page and removal ask for it again, and without it the state cannot be read. Keep it in a password manager.

A first run that failed part of the way keeps its state, which records what it made, so the next try asks once for that run's passphrase rather than a new one. After a [removal](uninstalling.md#in-the-appliance) the emptied state goes, and the next setup chooses a passphrase afresh.

**A lost passphrase cannot be recovered**, and nothing stands in for it. The management cluster and every cluster keep running, and the interface still creates, changes and deletes clusters with an account of its own. What is lost is applying the management cluster again (it stays at the versions it has), repair access, and removal from the page. To start over:

1. Delete every cluster in the interface, and wait until they are gone, so that the management cluster cleans up their Proxmox VE objects.
2. In Proxmox VE, delete the VMs in the pools `kubehenge-management` (the management cluster) and `kubehenge-templates` (the VM templates), then the pools `kubehenge-management`, `kubehenge-templates` and `kubehenge`, the users `kubehenge-cluster-api@pve` and `kubehenge-provisioner@pve` (their API tokens go with them), the group `kubehenge-clusters`, and the roles named `kubehenge-…`.
3. [Remove the appliance](uninstalling.md#remove-the-appliance) and install it again, which starts from an empty state.

### While it runs

Applying takes ten to twenty minutes. The page shows the step the run is at and follows its log; closing it does not stop the run. If it fails, the page shows the error and the log, and the settings stay filled in for another try. When it succeeds, the interface switches to the cluster list, and administrators get an **Administration** link in the top bar.

The run creates, on Proxmox VE:

- the three pools;
- a user and API token for Cluster API, allowed to manage VMs in the clusters' pool, clone templates from the template pool, use the chosen storages and bridges and write cloud-init images, and nothing of the management cluster's own VMs;
- a user and API token for Kubehenge's controller, which creates each cluster's own pool, user and token ([architecture.md](architecture.md#each-clusters-own-corner-of-proxmox-ve));
- a group for the clusters' users, and the roles all of these use;
- the Talos disk image, and the management cluster's VMs, tagged `kubehenge` and `management`.

In the management cluster it installs cert-manager, the Cluster API Operator with Cluster API and its providers for Proxmox VE, kubeadm, RKE2, k3s, IP address management and Helm add-ons, and the `kubehenge` chart: Kubehenge's ClusterClasses and its controller. The kubeadm, RKE2 and k3s controllers run only while a cluster uses their distribution ([architecture.md](architecture.md#management-cluster)).

### Back up the appliance

The appliance holds the only copy of the management cluster's state. Add it to a Proxmox VE backup job (Datacenter, Backup), by its ID, by a pool it is in, or with all guests on its node. Once the management cluster exists, and until a backup job includes the appliance, administrators with `Sys.Audit` on `/` see a warning.

## Create the VM templates

Every cluster's nodes are clones of a VM template that `kubehenge template create` makes through the Proxmox VE API ([templates.md](templates.md)):

| Distribution | Template |
| --- | --- |
| RKE2 and k3s | one template, shared: `kubehenge template create rke2` |
| kubeadm | one per Kubernetes version: `kubehenge template create kubeadm --kubernetes-version v1.36.5` |

Create them after the management cluster: they go into its template pool, `kubehenge-templates`, which the setup creates. A kubeadm cluster can only be created at, or upgraded to, a version that has a template.

On the Administration page, **VM templates** lists them, each with a cluster that clones from it, and **Build a template** makes one with your own Proxmox VE session, as the setup does:

| Field | What it is |
| --- | --- |
| For | RKE2 and k3s (one template, shared), or kubeadm with its Kubernetes version, such as `v1.36.5`. |
| Node | The node the image is prepared on. |
| Template disk | Where the template's disk goes. Shared storage, listed first, lets clones start on any node of a Proxmox VE cluster. |
| Downloaded image | A storage with the `import` content type, for the downloaded cloud image. |
| Cloud-init image | A storage with the `iso` content type, used while the image is prepared; the node's first one when left empty. |
| Bridge, VLAN tag | Where the VM that prepares the image attaches; it needs to reach Ubuntu's and Kubernetes' package mirrors. |
| Static address | Optional: an address with prefix length, a gateway and DNS servers for that VM, instead of DHCP. |

A build takes a few minutes; the page follows its log, and only one runs at a time, never during a run of the management cluster or an upgrade of the appliance. A template a cluster still clones from cannot be deleted: the shared one while any RKE2 or k3s cluster exists, a kubeadm one while a cluster or one of its machines runs its version.

The same is possible from any Linux machine that reaches the Proxmox VE API, with `kubehenge template create` and an API token ([templates.md](templates.md)). Keep the defaults of `--os` and `--pool` there: the ClusterClasses select templates of the default Ubuntu release, and Cluster API may clone only from the template pool.

## Alternative: Terraform/OpenTofu

The management cluster can be applied with Terraform/OpenTofu instead of from the appliance: the same module, `management-cluster/`, with every variable open. The web interface comes with the appliance only, so clusters are then created and looked after with kubectl ([clusters.md](clusters.md#with-kubectl)).

Take the module from the release archive, `kubehenge_<version>_linux_amd64.tar.gz`: its `share/kubehenge/` holds `management-cluster/` with the `charts/` it installs beside it, and its chart runs the release's controller image by digest. `bin/kubehenge` in the same archive is the command for the VM templates.

### Before you apply

- **Proxmox VE 9**, and the `import` content type on the storage named in `image_datastore`.
- **An API token with administrator rights**, since the module creates users, roles, pools and VMs: for example `pveum user token add root@pam kubehenge --privsep 0`. Or an administrator's session: leave `proxmox_api_token` unset and put a ticket and its CSRF token from `POST /access/ticket` in `PROXMOX_VE_AUTH_TICKET` and `PROXMOX_VE_CSRF_PREVENTION_TOKEN`; the apply must then finish within the ticket's two hours.
- **The Proxmox VE CA certificate**, `/etc/pve/pve-root-ca.pem` on any node unless you replaced the API's certificate. The machine running Terraform/OpenTofu must trust it too (add it to the system's trust store).
- **The addresses and network access** [above](#network-and-addresses). The machine running Terraform/OpenTofu takes the appliance's place: it reaches the nodes on ports 50000 and 6443.

### Apply

```sh
cd share/kubehenge/management-cluster
cp terraform.tfvars.example terraform.tfvars   # then fill it in
export TF_VAR_proxmox_api_token='root@pam!kubehenge=...'
export TF_VAR_proxmox_ca_certificate="$(cat pve-root-ca.pem)"
tofu init -lockfile=readonly
tofu apply
tofu output -raw kubeconfig > kubeconfig
tofu output -raw talosconfig > talosconfig
```

`-lockfile=readonly` holds the providers to the checksums in the module's `.terraform.lock.hcl`, as the appliance does. The module is tested with OpenTofu, which the appliance runs too; it declares Terraform 1.11 or newer as well. The state holds the cluster's secrets and its administrator credentials: keep it in an encrypted backend, or use OpenTofu's state encryption.

### Variables

| Variable | Default | What it is |
| --- | --- | --- |
| `proxmox_endpoint` | required | The Proxmox VE API, such as `https://pve.example.org:8006`. Every cluster's nodes reach Proxmox VE at this address too. |
| `proxmox_api_token` | none | `user@realm!name=secret`, with administrator rights. Unset, a session from the environment is used. |
| `proxmox_ca_certificate` | required | PEM of the CA that signed the API's certificate. Cluster API's Proxmox VE provider verifies the API against it. |
| `proxmox_insecure` | `false` | Skip verifying the API's certificate: in this configuration's Proxmox VE provider, Cluster API's Proxmox VE provider, Kubehenge's controller and the clusters' cloud controller manager and CSI plugin. |
| `proxmox_nodes` | required | The nodes the management cluster's VMs run on, taken in turn, such as `["pve1", "pve2", "pve3"]`. Each needs the bridge and both storages below. A VM given another node is migrated there. |
| `image_datastore` | `local` | Storage the Talos image is downloaded to, on each node; it must allow `import`. |
| `vm_datastore` | `local-lvm` | Storage for the management cluster's disks. |
| `bridge`, `vlan_id` | `vmbr0`, none | Bridge and VLAN tag of the management cluster's VMs. |
| `cluster_name` | `kubehenge` | The management cluster's name, also the prefix of its VMs' names and of the users, roles and group the module creates. |
| `node_addresses` | required | One, three or five addresses with prefix length, such as `["192.0.2.11/24"]`. |
| `api_address` | required | The Kubernetes API's virtual IP, in the nodes' subnet and unused. |
| `gateway`, `dns_servers` | required | The nodes' gateway and DNS servers. |
| `pod_subnets`, `service_subnets` | `["10.244.0.0/16"]`, `["10.96.0.0/12"]` | The management cluster's own ranges. The plan refuses ranges that overlap the nodes' network. |
| `node_cores`, `node_memory_mb`, `node_disk_gb` | `4`, `4096`, `32` | Size of each node. |
| `cpu_type` | `x86-64-v2-AES` | QEMU CPU type; Talos needs at least x86-64-v2. `host` spares the nodes mitigations their CPU does not need, but lets them migrate only between nodes with the same CPU ([templates.md](templates.md#how-clusters-find-them)). |
| `workload_pool` | `kubehenge` | Pool for the clusters' VMs, each cluster in a pool of its own under it. A pool name, nested at most one level. |
| `template_pool` | `kubehenge-templates` | Pool of the VM templates. |
| `management_pool` | `kubehenge-management` | Pool of the management cluster's own VMs, out of Cluster API's reach. |
| `workload_datastores` | `["local-lvm"]` | Storages clusters may use. |
| `workload_bridges` | `["vmbr0"]` | Bridges clusters may attach to, with any VLAN on them. |
| `bootstrap_datastore` | `local` | Storage for cloud-init ISOs; Cluster API gets full control of it. |
| `registry_mirrors` | `{}` | Registry mirrors for the management cluster's nodes, by registry name: endpoint URLs tried in order before the registry itself. A mirror of factory.talos.dev, ghcr.io or registry.k8s.io serves the images' cosign signatures too, which the nodes require on Sidero's and Kubernetes' images. |
| `cluster_oidc` | `[]` | OpenID Connect providers every new cluster trusts unless it names its own: up to eight, each with `issuer_url` (https) and `client_id`, and optionally `username_claim`, `username_prefix`, `groups_claim`, `groups_prefix`, `scopes` and `certificate_authority`. |
| `version_overrides` | `{}` | Versions to use instead of the tested set, by its keys, to hold a component back. A provider held back needs its image in `provider_images` and its files' digests in `provider_manifests`. Lowering a version on a running cluster is not supported. |
| `provider_images` | `{}` | Controller images by digest to run instead of the tested ones, by `"<kind>/<name>"`, such as `"InfrastructureProvider/proxmox"`. |
| `provider_manifests` | `{}` | The SHA-256 of provider files outside the tested set, by their address under `https://github.com/`, such as `"kubernetes-sigs/cluster-api/releases/download/v1.14.2/core-components.yaml"`: `metadata.yaml` and the components file of each provider held back. |
| `controller_image` | the chart's | Kubehenge's controller image by digest, as `repository@sha256:<digest>`. A release's chart names the image it was released with; a checkout's names none, so a module taken from git needs one. |
| `proxmox_ha` | `false` | Keep each cluster's control-plane VMs on separate nodes with Proxmox VE HA, giving the controller a token with `Sys.Audit` and `Sys.Console` on `/` ([above](#proxmox-ve-ha)). After setting it back to false, remove the HA resources and rules whose comment starts with `Kubehenge cluster`, as the appliance does: without its token the controller cannot. |
| `offer_ha_rules` | `false` | Offer Proxmox VE's HA node-affinity rules, each one's name and nodes, to those with `VM.Allocate` on the workload pool or a cluster's pool who lack `Sys.Audit` on `/`, read with the controller's own token ([above](#ha-rules-for-cluster-creators)). Asks nothing new of Proxmox VE. |
| `cluster_datastore_allocate` | `false` | Let clusters choose the CSI plugin's newest release, with `Datastore.Allocate` for the cluster's token on a storage of its own, granted by the controller ([above](#the-csi-plugins-newest-release)). |

Outputs: `kubeconfig` and `talosconfig` (sensitive), `api_endpoint`, and `proxmox_objects`, which lists everything the module made on Proxmox VE.

Then create the [VM templates](#create-the-vm-templates) with `bin/kubehenge` from the archive, on any Linux machine that reaches the Proxmox VE API:

```sh
export KUBEHENGE_PROXMOX_URL=https://pve.example.org:8006
export KUBEHENGE_PROXMOX_CA=pve-root-ca.pem
export KUBEHENGE_PROXMOX_TOKEN='root@pam!kubehenge=...'
bin/kubehenge template create rke2 --node pve1 --storage shared-storage
```

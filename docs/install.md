# Installing Kubehenge

Kubehenge runs on Proxmox VE as a small appliance, an unprivileged LXC container that serves Kubehenge's web interface. From the interface, an administrator creates the management cluster (one or three Talos Linux VMs running Cluster API and Kubehenge's controller). From then on, clusters are created from the interface or with kubectl. Those who keep their infrastructure as code can apply the management cluster with Terraform/OpenTofu instead ([below](#alternative-terraformopentofu)).

1. Check the [requirements](#requirements).
2. [Install the appliance](#install-the-appliance) with one line in the Proxmox VE shell.
3. [Sign in](#sign-in) and [set up the management cluster](#set-up-the-management-cluster).
4. [Create the VM templates](#create-the-vm-templates) clusters are cloned from.

Then [create clusters](clusters.md).

## Requirements

### Proxmox VE

- **Proxmox VE 9 on x86-64.** The installer refuses any other release. Kubehenge has no build for other architectures.
- **Root in the shell of one node** to run the installer.
- **An administrator** to set up the management cluster, a user with the Administrator role on `/` ([what it needs](#who-may-set-it-up)).

### Storage

| For | Content type | Notes |
| --- | --- | --- |
| The appliance's disk, 8 GB | `rootdir` | The first active storage that allows it, unless `--storage` names one. |
| The container template | `vztmpl`, on `local` | The installer downloads Debian's standard container template there through `pveam` if it is missing. |
| The Talos disk image, and the images VM templates start from | `import` | Proxmox VE's `local` storage does not allow `import` until you add it (Datacenter, Storage, Edit, Content). |
| The management cluster's disks | `images` | 32 GB per node. |
| Cloud-init images | `iso` | Cluster API keeps each new cluster VM's cloud-init ISO here and gets full control of the storage. Give it a directory storage of its own with the content type `ISO image` alone (Datacenter, Storage, Add, Directory). In the shell, that is `pvesm add dir kubehenge-iso --path /var/lib/kubehenge-iso --content iso`. Choose it as **Cloud-init images** in the setup. |
| Clusters' disks and persistent volumes | `images` | One or more storages, chosen when the management cluster is set up. |
| VM templates | `images` | Shared storage in a Proxmox VE cluster, so that clones can start on any node. |

### Network and addresses

Addresses are IPv4. Kubehenge gives every node of every cluster a static address, with no DHCP server needed.

- **The appliance** takes one address on a bridge, by DHCP or static (`--ip`).
  - Its certificate and the redirect URLs registered with identity providers contain it. Give it an address that does not change (a static `--ip` or a DHCP reservation), or a DNS name with `--name`.
  - After a change, [renew the certificate](#the-appliances-certificate).
- **The Proxmox VE API** is reached on port 8006, at a name or address its certificate is valid for ([below](#the-proxmox-ve-api)). The appliance, the management cluster's nodes and every cluster's nodes must reach it.

#### Addresses for clusters

- **The management cluster** needs one free address per node (one, or three for high availability) and one for its Kubernetes API's virtual IP. All are in one subnet on one bridge, with a gateway and DNS servers.
  - Its VMs can run on several Proxmox VE nodes. Each of those needs that bridge, in the same layer-2 network.
  - The appliance reaches its nodes on ports 50000 (Talos API) and 6443 (Kubernetes API).
- **Each cluster** needs a range of free addresses for its nodes. Its control plane's virtual IP is one more, in the same subnet but outside the range.
  - Leave spare addresses in the range, because an upgrade or a node replacement creates the new node before it removes the old one.
  - The management cluster reaches the virtual IP on port 6443.
- **Pod and service ranges** must stay clear of the nodes' networks. Kubehenge refuses overlapping ranges and picks clear ones when given none.
- **The VM that prepares a template** takes an address by DHCP on its bridge, or a static one given when the template is built ([below](#create-the-vm-templates)).

### Internet access

| Who | Reaches |
| --- | --- |
| The node running the installer | GitHub, for Kubehenge's release and OpenTofu. The container template mirror, through `pveam`. |
| The appliance | GitHub, for the release index and upgrades. While it applies the management cluster: the OpenTofu registry, `quay.io` (cert-manager's chart) and the Talos Image Factory. While it makes VM templates: the Talos Image Factory (`factory.talos.dev`), the node OSes' image mirrors for their signed checksums (`repo.almalinux.org`, `dl.rockylinux.org`), and the package repositories' signing keys (`pkgs.k8s.io`, `rpm.rancher.io`). |
| The Proxmox VE nodes | the Talos Image Factory and the node OSes' image mirrors, which Proxmox VE downloads disk images from |
| The management cluster's nodes | Container registries and the Cluster API providers' release files. RKE2's and k3s's release channels, for automatic upgrades, and their releases' checksum files and install scripts on GitHub, read once per release. The add-on chart repositories: `ghcr.io` (the Proxmox VE cloud controller manager and CSI plugin), `helm.cilium.io`, `docs.tigera.io`, `kubernetes-sigs.github.io` (Headlamp), `releases.rancher.com` and `charts.jetstack.io`. |
| Clusters' nodes | Container registries. On RKE2 and k3s, GitHub (`github.com` and `raw.githubusercontent.com`), for the release and its images, which a node installs when it boots. |
| The VM that prepares a template | Its node OS's package mirrors and `quay.io` (for the image builder's tool image, by digest). For RKE2 and k3s, `rpm.rancher.io` (their SELinux policies). For kubeadm, `pkgs.k8s.io`, the node OS's EPEL mirror (containerd), `registry.k8s.io`, GitHub (for cosign) and Sigstore's `tuf-repo-cdn.sigstore.dev`, to check the images' signatures. |

The management cluster's nodes can pull through registry mirrors instead ([setup](#network), Advanced).

### Sizes

| What | vCPUs | Memory | Disk |
| --- | --- | --- | --- |
| The appliance | 2 | 1 GiB, and 512 MiB swap | 8 GB |
| Each management cluster node (one or three) | 4 | 4 GiB | 32 GB |
| A cluster's control-plane node, by default | 2 | 4 GiB | 30 GiB |
| A cluster's worker, by default | 2 | 4 GiB | 40 GiB |

The appliance, a one-node management cluster and a cluster at the create wizard's defaults (one control plane, one worker) take about 110 GB of disk. They take 10 vCPUs and 13 GiB of memory, beside the appliance's swap. Three management cluster nodes add 8 vCPUs, 8 GiB and 64 GB. Add-ons raise a cluster's machines to what they need ([clusters.md](clusters.md#machines)).

Each node OS template build takes 12 GiB on the template storage. The two newest builds of each stay. While one is made, the VM that prepares it takes 2 vCPUs, up to 4 GiB of memory and 20 GiB of disk more.

## Install the appliance

In the shell of the Proxmox VE node that should run it, as root:

```sh
curl -fsSLO https://github.com/kubehenge/kubehenge/releases/latest/download/install.sh && sh install.sh
```

The same with a static address:

```sh
sh install.sh --ip 192.0.2.40/24 --gateway 192.0.2.1
```

`install.sh` carries the version of its release and the SHA-256 checksums of the release archive and of the OpenTofu release it installs. It checks each download against them before using it. To check the script itself before running it, see [Verifying a release](#verifying-a-release).

### Options

| Option | Default | What it does |
| --- | --- | --- |
| `--ctid <id>` | the next free ID | The container's ID. |
| `--storage <storage>` | the first active storage that takes container disks | Where the container's disk goes. |
| `--bridge <bridge>` | `vmbr0` | The bridge the container attaches to. |
| `--ip dhcp` or `--ip <address>/<prefix>` | `dhcp` | The container's address, such as `192.0.2.40/24`. |
| `--gateway <address>` | none | The default gateway, with a static `--ip`. |
| `--dns <address>,...` | this node's name servers | The name servers the container asks, such as `192.0.2.53,192.0.2.54`. Needed where the node's own are out of the container's reach, such as a resolver on the node's loopback address. |
| `--hostname <name>` | `kubehenge` | The container's host name, also written into its certificate. |
| `--name <DNS name>` | none | A DNS name, such as `kubehenge.example.org`, that people reach the appliance by instead of its address. It goes into the certificate and into the redirect URLs for identity providers. Point it at the appliance's address in your DNS. |
| `--cert <file> --key <file>` | a certificate signed by the Proxmox VE cluster's CA | Serves a certificate of your own instead, such as one from your organisation's CA. It must be valid for `--name` (or the address, without one) and its key must match. |
| `--proxmox-url <URL>` | chosen from the API's certificate ([below](#the-proxmox-ve-api)) | The Proxmox VE API as the appliance and the clusters' VMs reach it, such as `https://pve.example.org:8006`. The API's certificate must be valid for it. The installer stops unless the appliance gets an answer there. |
| `--proxmox-ca <file>` | found by the installer ([below](#the-ca)) | The CA that signed the API's certificate, such as your organisation's where it replaced Proxmox VE's own. The file may be a whole trust store, and the appliance gets only the certificates the API's rests on. The installer stops before it creates anything unless this CA signed the API's certificate for the URL. |
| `--proxmox-insecure` | off | Nothing checks the Proxmox VE API's certificate. That covers OpenTofu, Cluster API, Kubehenge's controller, the template builds and every cluster's cloud controller manager and CSI plugin. The Administration page shows that it is on. Not with `--proxmox-ca`. |
| `--release <file> --sha256 <hex>` | the published release the script names | Installs a release archive from a file instead, checked against the given checksum. OpenTofu is still downloaded. |
| `--pve-entry` or `--no-pve-entry` | asked | Whether to add a Kubernetes entry to this node's Proxmox VE interface ([below](#the-proxmox-ve-interface-entry)). Run without a terminal, it leaves the entry out. |
| `--upgrade --ctid <id>` | | Upgrades an installed appliance instead ([upgrading.md](upgrading.md#from-the-proxmox-ve-shell)). |
| `--certificate --ctid <id>` | | Replaces an installed appliance's certificate instead ([below](#the-appliances-certificate)). |

### What it does

1. Checks that it runs as root on a Proxmox VE 9 node. Unless `--pve-entry` or `--no-pve-entry` answered already, it asks whether to add the interface entry.
2. Downloads Kubehenge's release archive and OpenTofu, and checks both against the checksums it carries. `pveam` checks the container template against its signed index.
3. Chooses the Proxmox VE API's address and CA ([below](#the-proxmox-ve-api)), and asks on the terminal where it needs your word. It checks that the API's certificate is valid for that address.
4. Creates an unprivileged Debian container with nesting enabled, started at boot and tagged `kubehenge`, and waits for its IPv4 address.
5. Makes a certificate for the container's host name, address and `--name`, or checks the one given with `--cert`.
   - A made certificate is signed by the Proxmox VE cluster's CA, whose key never leaves the node. It is self-signed when the node cannot read that key.
   - It is valid for 825 days, the longest browsers accept from a private CA.
6. Sets up the container.
   - It installs Kubehenge, OpenTofu, the management-cluster module, the service units and the shared Headlamp, one Headlamp (a Kubernetes web interface) for every cluster.
   - It adds the API's CA to the container's trust store and switches off the template's SSH and mail servers.
   - It starts the interface on ports 443 and 8443 (the shared Headlamp) as an unprivileged user.
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

The appliance, the management cluster and every cluster reach the Proxmox VE API at one URL and check the certificate it serves. Without `--proxmox-url`, the installer reads the names and addresses in this node's API certificate (`/etc/pve/local/pveproxy-ssl.pem`, else `pve-ssl.pem`). It keeps an address only if it belongs to one of this cluster's nodes. It keeps a name only if every address it resolves to belongs to one.

A wildcard name counts for this node's own name (`hostname -f`) where it covers it. Of what is left, the installer takes, in this order:

1. a name for several of the cluster's nodes, such as a DNS name with the address of each.
2. this node's own name.
3. another name of this node.
4. this node's address on the appliance's bridge, then its other addresses.
5. a name of this node without a domain, which resolves only where a search domain supplies one.
6. a name, then an address, of another node of the cluster.

A name counts as it resolves on this node. The appliance and the clusters' VMs have to resolve it the same way, through DNS. Where several tie, the installer lists them on the terminal with the first as the default, and asks which. Without a terminal it takes the first and names it.

Where the certificate is valid for none of the cluster's names and addresses, the installer stops and says what it names. Then give `--proxmox-url` with a name or address the certificate is valid for.

#### The CA

The CA the certificate is checked against is, without `--proxmox-ca`:

1. the Proxmox VE cluster's own CA, `/etc/pve/pve-root-ca.pem`, where it signed the certificate.
2. else the CA in this node's trust store (`/etc/ssl/certs/ca-certificates.crt`) the certificate rests on, such as your organisation's CA once it is installed there.
   - The installer names it. The appliance gets that CA alone, not the whole store.
3. else the CA the API serves with its certificate: its root where it serves one, else the topmost CA it serves, else the certificate itself.
   - The installer shows its subject, issuer and SHA-256 fingerprint and asks whether to trust it. Check the fingerprint against your CA before you say yes.
   - Without a terminal it stops and names the fingerprint.

Where none of them applies, or you do not trust the CA it shows, the installer stops before it creates anything. Then give the CA with `--proxmox-ca`, or check nothing with `--proxmox-insecure`.

### The appliance's certificate

The interface warns administrators 30 days before its certificate runs out. Renew it in the shell of the appliance's node, with the installer of the appliance's release or a newer one:

```sh
sh install.sh --certificate --ctid 105
```

The same command makes a new certificate after the appliance's address changed. `--name kubehenge.example.org` gives the appliance a DNS name. `--cert <file> --key <file>` installs a certificate of your own.

It keeps a name given before unless `--name` gives another, or `--no-name` drops it for the appliance's address again. It restarts the interface and the shared Headlamp, and prints the new fingerprint. It does not restart while the management cluster is being applied or removed or a VM template is being built. Run it again once that has finished.

`--upgrade` renews a certificate the installer made once it has less than 60 days left. It only warns about a certificate of your own.

A renewal keeps a name from a certificate this cluster's CA signed. It keeps any other name only where this node's DNS resolves it to the appliance's address, and otherwise drops it and says so. `--name` keeps it. The address is the one the container's configuration gives it, or with DHCP the one the container reports.

After a new name or address, update the redirect URLs registered for OpenID realms (`https://<name>/api/openid/callback`) and for the shared Headlamp (`https://<name>:8443/oidc-callback`).

### The Proxmox VE interface entry

The entry adds a **Kubernetes** button beside Create VM and Create CT, and a **Kubernetes** item under Datacenter. Both open the appliance in a new tab. Kubehenge works the same without it. Each node serves its own interface, and the appliance's installer adds the entry only on its own node. On every other node, run the entry's own installer from `share/kubehenge/pve-entry/` in the release archive:

```sh
sh install.sh --url https://192.0.2.40
```

After every package run, an apt hook puts back an entry that a `pve-manager` update removed, and logs it (`journalctl -t kubehenge-pve-entry`). If a Proxmox VE release changes its interface so the entry no longer fits, the interface works as shipped, without the entry. Run the entry's installer again with another `--url` to change the address it opens, or with `--remove` to take it out of a node. More in [its README](https://github.com/kubehenge/kubehenge/blob/main/appliance/pve-entry/README.md).

### Verifying a release

`install.sh` checks what it downloads. The appliance installs only releases listed in an index signed with the project's release key. What is left to check yourself is the script.

The release workflow built each of its files and each image Kubehenge runs from the tagged source, on a GitHub-hosted runner (SLSA Build Level 3). Each carries signed build provenance and an SPDX SBOM. They are stored as GitHub attestations of the repository and, for the files, in `kubehenge_<version>.intoto.jsonl` beside them.

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

The checksums prove the files are the ones the release lists. The attestation proves the release workflow built them, from this repository at that tag, with the steps in `release-build.yml`.

`kubehenge_<version>_linux_amd64.spdx.json` lists every file in the archive with its SHA-256 and every package in it at its exact version. A vulnerability scanner such as Grype or Trivy can read it. Add `--predicate-type https://spdx.dev/Document/v2.3` to the command above to check the SBOM attested to the archive instead of its provenance.

#### The images

The images carry the same provenance and SBOM in the registry, and a Sigstore signature that cosign checks:

```sh
image=ghcr.io/kubehenge/kubehenge:v<version>
gh attestation verify "oci://$image" --repo kubehenge/kubehenge \
  --signer-workflow kubehenge/kubehenge/.github/workflows/release-build.yml \
  --source-ref refs/tags/v<version> --deny-self-hosted-runners
cosign verify "$image" \
  --certificate-identity https://github.com/kubehenge/kubehenge/.github/workflows/release-build.yml@refs/tags/v<version> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The Cluster API providers for Talos come from Kubehenge's forks ([below](#what-the-run-creates)), each signed and attested by its own release workflow. The management cluster's nodes pull their images only with that signature. `make verify-providers`, from a checkout of the tag, checks their files and images against the digests the tag pins, with the GitHub CLI and cosign.

How releases are built and signed is in [releasing.md](releasing.md).

## Sign in

Open the address the installer printed. A browser that trusts Proxmox VE's certificates trusts it, as the Proxmox VE cluster's CA signed it. Otherwise, compare the fingerprint the browser shows with the one the installer printed.

Sign in with a Proxmox VE account of any realm: Proxmox VE, PAM, LDAP, Active Directory or OpenID. There is no second user directory.

- **Realm and password.** Choose the realm, or type `user@realm` as the user name.
- **Second factor.** An account with TOTP, Yubico OTP or recovery keys is asked for one.
  - WebAuthn keys are bound to Proxmox VE's own address and do not work here. Their users sign in with another factor or an OpenID realm.
- **OpenID realms** return from the identity provider to `https://<appliance>/api/openid/callback`. Register it as a redirect URL with the provider of the realm's client.
- **Sessions** end eight hours after sign-in, however active, or when the interface restarts on an upgrade or a certificate renewal.
- **Five failed sign-ins** for one user from one address within 15 minutes refuse that user from there for the rest of them.
- **Twenty failed sign-ins** from one address in that time refuse the address, whatever users they name.

Every user sees and changes only what their Proxmox VE privileges allow ([clusters.md](clusters.md#who-may-do-what)).

## Set up the management cluster

On first sign-in, an administrator sees the setup page. It creates the management cluster with the `management-cluster/` module and OpenTofu, inside the appliance.

The page goes through five steps: **Placement**, **Network**, **Clusters**, **Services** and **Passphrase**. It fills in what the first chosen node reports about itself, all of which can be changed:

- A shared storage for the VM disks, or else the one with the most free space.
- The bridge the node reaches its gateway through, with that gateway.
- The node's own DNS servers.
- The network the node addresses go in.

For clusters, it offers the same storage and bridge to start with. Administration shows the same settings on one page, with one section per step.

### Who may set it up

The signed-in user needs these privileges on `/`, which the Administrator role holds:

`Sys.Modify`, `Permissions.Modify`, `User.Modify`, `Group.Allocate`, `Realm.AllocateUser`, `Pool.Allocate`, `VM.Allocate`, `Datastore.AllocateSpace`, `Datastore.AllocateTemplate`

Proxmox VE checks every call of the run again on its own. Anyone without them sees that Kubehenge is not set up yet. The same privileges open Administration later, in five pages:

- **Management cluster**, its settings (applying them again or changing them) and its removal.
- **Appliance**, Kubehenge's upgrades and repair access.
- **Backups**, the [management cluster's etcd snapshots](#back-up-the-management-clusters-etcd).
- **Templates**, the VM templates and their weekly build.
- **Certificates**, the DNS-01 credential for every cluster.

They also bring the warnings about the [backup](#back-up-the-appliance) and the [certificate](#the-appliances-certificate).

The run acts with your own Proxmox VE session, with no administrator token created. The interface renews your ticket and hands it to OpenTofu through its environment only, never storing it. A ticket lasts two hours, and the run has to finish within them.

The one exception is automatic upgrades of the management cluster, off by default. With them on, the appliance keeps the state passphrase and an Administrator token of a user of its own ([upgrading.md](upgrading.md#automatically-after-kubehenge)).

### The settings

#### Placement

| Field | What it is |
| --- | --- |
| Proxmox VE nodes | The nodes the management cluster's VMs run on, taken in turn, no more than there are node addresses. With three nodes for three VMs, the management cluster keeps running when one node fails. An offline node cannot be newly chosen. The storages and bridge below are offered only when every chosen node has them. |
| VM disks | Storage for the management cluster's disks. |
| Talos image | Storage the Talos disk image is downloaded to on each chosen node, with the `import` content type. |
| Cloud-init images | Storage for the clusters' cloud-init ISOs, with the `iso` content type ([above](#storage)). |

#### Network

| Field | What it is |
| --- | --- |
| Bridge | Bridge the management cluster's VMs attach to. |
| VLAN tag | Optional, 1 to 4094. |
| Node addresses | One IPv4 address, or three for high availability, each with its prefix length: `192.0.2.11/24`. All in one network, the gateway's, and none of them the network's own or broadcast address. |
| API address | The virtual IP of the management cluster's Kubernetes API. An unused IPv4 address in the nodes' network, neither a node address nor the gateway. It moves to a healthy node when one fails. |
| Gateway | The nodes' default gateway, an IPv4 address. |
| DNS servers | IP addresses, one per line or separated by commas. |
| Advanced: Registry mirrors | `registry=endpoint`, one per line, such as `ghcr.io=https://mirror.example.org:5000`. The management cluster's nodes try them before the registry itself. |
| Advanced: Pod ranges, Service ranges | The management cluster's own. When empty, `10.244.0.0/16` and `10.96.0.0/12`. Where those overlap the nodes' network, the API address or the gateway, the first clear one of a few other private ranges. |

#### Clusters

| Field | What it is |
| --- | --- |
| Available to clusters | The storages and bridges clusters may put their disks on and attach to, from any node. At least one of each. The page says which nodes have each, or that a storage is shared. Cluster API's token gets rights on these alone. The ClusterClasses refuse any others, from the interface or kubectl. |
| Proxmox VE HA | Off by default. When on, Proxmox VE HA keeps each cluster's control-plane VMs on separate nodes and restarts them elsewhere after a node fails. This holds for every cluster on shared storage that does not opt out ([clusters.md](clusters.md#machines)). Read [the details](install-settings.md#proxmox-ve-ha) first. |
| HA rule placements | Off by default. When on, everyone who may create or change a cluster can place it on the nodes of a Proxmox VE HA node-affinity rule, without `Sys.Audit` on `/` of their own ([clusters.md](clusters.md#machines), [details](install-settings.md#ha-rule-placements)). |
| CSI plugin | Off by default. When on, clusters may choose the CSI plugin's newest release ([details](install-settings.md#csi-plugin)). |
| Template refresh | On by default. The node OS VM templates are built again every week with their node OS's updates. Clusters' nodes move to each build in their maintenance windows once it is a day old ([templates.md](templates.md#the-weekly-build), [details](install-settings.md#what-the-template-builds-may-do)). |
| Day, Time, Time zone | When the build runs. Sunday at 01:00 UTC unless changed. |
| Template disks | The storage for clusters the builds go to. The first one by default. |
| Build address, Build gateway, Build DNS servers | A static address for the VM that prepares an image. Empty, that VM takes DHCP on the first bridge for clusters. |
| Let creators request builds | Off by default, recommended. When on, anyone with `VM.Allocate` on the workload pool may ask the create wizard for a missing RKE2, k3s or kubeadm template of their node OS. The build runs with the refresh's token, never theirs. |
| HA group labels | On by default. Each cluster's nodes are labelled with Proxmox VE's HA groups unless the cluster switches them off. Its token then reads the Proxmox VE cluster's configuration. Untick it to give no cluster that read ([details](install-settings.md#ha-group-labels)). |
| Nodes' boot logs | On by default. The controller reads kubeadm, RKE2 and k3s nodes' boot logs through their guest agents (files only). A node that never joins then shows why. Untick it to take that privilege away ([details](install-settings.md#nodes-boot-logs)). |
| kubectl changes | Off by default. When on, Activity also lists each change people make to the management cluster with kubectl or its API, with their user ([details](install-settings.md#kubectl-changes)). |
| Cluster namespaces | Off by default. The interface creates and manages clusters in `clusters` only. When on, cluster creators choose among `clusters` and the namespaces named here, separated by commas, such as one per team ([clusters.md](clusters.md#more-namespaces-for-clusters)). A namespace that does not exist yet is created. One taken off the list keeps its clusters. |

#### Services

| Field | What it is |
| --- | --- |
| Headlamp sign-in | Off by default, recommended. When on, the shared Headlamp opens each cluster signed in as cluster-admin for those who may download its admin kubeconfig ([clusters.md](clusters.md#open-a-cluster-in-headlamp)). Read [the details](install-settings.md#headlamp-sign-in) first. |
| Headlamp actions | Off by default. When on, the shared Headlamp offers the Helm app catalog and the Prometheus charts it ships with. Both act with each user's own credentials in the cluster ([clusters.md](clusters.md#open-a-cluster-in-headlamp), [details](install-settings.md#headlamp-actions)). |
| Identity providers | OpenID Connect providers every new cluster trusts unless it names its own ([clusters.md](clusters.md#signing-in-with-openid-connect)). A change applies to clusters created afterwards. |
| Backup targets | S3 stores for the clusters' Velero backups and etcd snapshots, for clusters that switch either on ([details](install-settings.md#backup-targets)). A changed target reaches the clusters that back up to it within ten minutes. One taken away leaves their Velero as it is. |
| etcd restores | Off by default. The interface then shows the steps to restore a cluster's etcd by hand. When on, Kubehenge restores it itself through the VMs' guest agents ([details](install-settings.md#etcd-restores)). |

The **Passphrase** step asks twice for the [state passphrase](#the-state-passphrase), which encrypts the management cluster's state on the appliance.

Once the management cluster runs, these stay as they are ([upgrading.md](upgrading.md#in-the-appliance)):

- its node addresses (more can only be added after them)
- its API address and gateway
- its pod and service ranges
- its VM disks

What each opt-in setting grants and risks, and the fields of a backup target, are in [Setup settings in depth](install-settings.md).

The appliance keeps the module's other settings at their defaults:

- The management cluster is named `kubehenge`.
- Its nodes have 4 vCPUs, 4 GiB of memory and a 32 GB disk.
- It creates the pools `kubehenge` (clusters), `kubehenge-templates` (VM templates) and `kubehenge-management` (its own VMs).

### The state passphrase

The management cluster's state holds its secrets, among them its admin kubeconfig and talosconfig. The appliance encrypts it with a key derived from the passphrase, which is at least 16 printable characters on one line. It is not stored, unless automatic upgrades of the management cluster are switched on ([upgrading.md](upgrading.md#automatically-after-kubehenge)).

Every later apply, the repair access on the Administration page and removal ask for it again. Without it the state cannot be read. Keep it in a password manager.

A first run that failed part of the way keeps its state, which records what it made. The next try asks once for that run's passphrase rather than a new one. After a [removal](uninstalling.md#in-the-appliance) the emptied state goes, and the next setup chooses a passphrase afresh.

#### Losing the passphrase

A lost passphrase cannot be recovered. The management cluster and every cluster keep running. The interface still creates, changes and deletes clusters. What is lost is applying the management cluster again (it stays at the versions it has), repair access, and removal from the page. To start over:

1. Delete every cluster in the interface and wait until they are gone, so that the management cluster cleans up their Proxmox VE objects.
2. In Proxmox VE, delete what the setup made:
   - the VMs in the pools `kubehenge-management` (the management cluster) and `kubehenge-templates` (the VM templates), then the pools `kubehenge-management`, `kubehenge-templates` and `kubehenge`.
   - the users `kubehenge-cluster-api@pve`, `kubehenge-provisioner@pve` and those of the settings that were on. Their API tokens go with them.
     - `kubehenge-template-builder@pve` (template refresh)
     - `kubehenge-ha@pve` (Proxmox VE HA)
     - `kubehenge-etcd-restore@pve` (etcd restores)
     - `kubehenge-upgrader@pve` (automatic upgrades of the management cluster)
   - the groups `kubehenge-clusters` and, where HA group labels were on, `kubehenge-clusters-audit`.
   - the roles named `kubehenge-…`.
   - the Talos disk image, `talos-<version>-<id>-nocloud-amd64.qcow2`, on the Talos image storage of each node that ran the management cluster.
3. [Remove the appliance](uninstalling.md#remove-the-appliance) and install it again.

### The management cluster's credentials

The management cluster's admin kubeconfig and talosconfig hold client certificates that last a year. Only an apply in their last month renews them, since only the passphrase opens the state. The interface warns administrators 30 days before they run out. Apply the settings in **Administration** to renew them.

### While it runs

Applying takes ten to twenty minutes. The page shows the step the run is at and follows its log. Closing it does not stop the run.

If it fails, the page shows the error and the log, with the settings still filled in for another try. When it succeeds, the interface switches to the cluster list. Administrators get an **Administration** link in the top bar.

#### What the run creates

On Proxmox VE:

- the three pools.
- a user and API token for Cluster API, with no rights on the management cluster's own VMs. It may manage VMs in the clusters' pool, clone templates from the template pool, use the chosen storages and bridges and write cloud-init images.
- a user and API token for Kubehenge's controller, which creates each cluster's own pool, user and token ([architecture.md](architecture.md#each-clusters-own-corner-of-proxmox-ve)).
- while the template refresh is on, a user and API token for the weekly template build ([details](install-settings.md#what-the-template-builds-may-do)).
- with [Proxmox VE HA](install-settings.md#proxmox-ve-ha) on, a user and API token for its HA resources and rules.
- with [etcd restores](install-settings.md#etcd-restores) on, a user and API token for restores through the guest agent.
- a group for the clusters' users, and the roles all of these use.
- with [HA group labels](install-settings.md#ha-group-labels) on, a second group holding `Sys.Audit` on `/`.
- the Talos disk image, and the management cluster's VMs, tagged `kubehenge` and `management`.

In the management cluster it installs:

- cert-manager.
- the Cluster API Operator with Cluster API and its providers for Proxmox VE, kubeadm, RKE2, k3s, Talos, IP address management and Helm add-ons.
  - The Talos providers come from Kubehenge's forks, [cluster-api-bootstrap-provider-talos](https://github.com/kubehenge/cluster-api-bootstrap-provider-talos) and [cluster-api-control-plane-provider-talos](https://github.com/kubehenge/cluster-api-control-plane-provider-talos).
- the `kubehenge` chart, with Kubehenge's ClusterClasses and its controller.

The kubeadm, RKE2, k3s and Talos controllers run only while a cluster uses their distribution ([architecture.md](architecture.md#management-cluster)).

### Activity

**Administration, Activity** lists who did what through the interface, newest first, over the last day to 90 days. That covers sign-ins, refused sign-ins and every change, with the cluster it touched. An action that ended in an error is marked failed. Below it are the management cluster's events of the last day. It needs `Sys.Audit` on `/`.

Each cluster's **Activity** view lists what was done to that cluster, for everyone who sees it ([clusters.md](clusters.md#the-cluster-page)).

The appliance keeps the record in its own folder for 90 days. Changes made with kubectl or through the management cluster's API show only when **kubectl changes** is on ([details](install-settings.md#kubectl-changes)).

| Command | Does |
| --- | --- |
| `kubehenge appliance activity` | lists the appliance's activity of the last 7 days. `--days` goes back up to 90 |
| `kubehenge appliance activity --cluster namespace/name` | lists one cluster's |

### Back up the appliance

The appliance holds the only copy of the management cluster's state. Add it to a Proxmox VE backup job (Datacenter, Backup) by its ID, its pool or all guests on its node. Once the management cluster exists, and until a backup job includes the appliance, administrators with `Sys.Audit` on `/` see a warning.

### Back up the management cluster's etcd

The management cluster's etcd holds every cluster's objects. **Administration, Backups** snapshots it into one of the [backup targets](install-settings.md#backup-targets) every 6 hours unless set otherwise. It keeps the newest 28.

**Set up…** takes the target, the schedule, an S3 access key for the target and the state passphrase. The first snapshot follows within a minute. **Take snapshot** takes one at once.

The appliance takes each snapshot itself through the nodes' Talos API, with a talosconfig that may take etcd snapshots and nothing else. It encrypts the snapshot before it leaves the appliance. The key that opens the snapshots is kept in the target, encrypted with the state passphrase ([details](install-settings.md#management-cluster-etcd-backups)).

#### Restoring

**Restore…** on a snapshot wipes etcd on every control-plane node of the management cluster and restores it from the snapshot. Every cluster's objects go back to then. Check clusters created or deleted since then in the cluster list, as they no longer match what is in Proxmox VE.

It asks for the state passphrase and counts as a run of the management cluster. It needs only the nodes' Talos API, so it works while the management cluster's own API does not answer.

The appliance's shell has the same commands, with the page's privileges and refusals ([signing in there](upgrading.md#from-the-appliances-shell)):

| Command | Does |
| --- | --- |
| `kubehenge appliance etcd-backups` | shows the target, the schedule, the last snapshot and the next |
| `kubehenge appliance etcd-backups set-up --target … --access-key-id … --secret-access-key-file … --passphrase-file …` | sets the backups up or changes them. `--every-hours` and `--keep` set the schedule |
| `kubehenge appliance etcd-backups off` | switches them off. The snapshots stay in the target |
| `kubehenge appliance etcd-backups snapshots` | lists the snapshots in the target, newest first |
| `kubehenge appliance etcd-backups take` | takes a snapshot and waits until it is stored |
| `kubehenge appliance etcd-backups restore … --passphrase-file …` | restores a snapshot by name and prints the run's log until it ends |

### Notifications

The appliance tells a webhook, an ntfy topic or an email address when something needs a look:

- an item on a cluster's Health page that has stood for 5 minutes, and again once it clears
- an alert firing in a cluster with the monitoring add-on, which shows on its Health page too
- the management cluster not answering, one of its nodes not ready or a controller short of replicas
- a failed run: a setup, a template build, a backup, an etcd snapshot or an upgrade

**Administration, Notifications** sets each target and shows its last sending. **Send test** sends one to each. Both need `Sys.Modify` on `/`.

| Target | Settings |
| --- | --- |
| Webhook | a URL. Each notice is a JSON POST with `key`, `severity` (`error`, `warning`, `resolved` or `info`), `title`, `text`, `cluster`, `link` and `at` |
| ntfy | the topic's URL, such as `https://ntfy.sh/<topic>`, and an access token for a topic that needs one |
| Email | the SMTP server as host:port, the sender and the recipients, comma-separated. A user name and password where the server wants them. Port 465 uses TLS, any other STARTTLS where the server offers it |

More than 5 notices at once arrive as one. `link` opens the cluster's Health page once the appliance knows its own address (`--public-url`).

#### Targets in Git

Each target is a Secret in the interface's namespace (`clusters` unless set otherwise):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: kubehenge-notify-ntfy   # kubehenge-notify-webhook, kubehenge-notify-email
  namespace: clusters
  labels:
    kubehenge.dev/user-credential: notify-ntfy
type: kubehenge.dev/user-credential
stringData:
  url: https://ntfy.sh/my-clusters
  token: tk_example   # optional
```

The webhook's key is `url`. The email's are `server`, `from`, `to`, and optionally `username` and `password`.

In the appliance's shell, `kubehenge appliance notifications` shows each target's last sending and `kubehenge appliance notifications test` sends a test.

## Create the VM templates

Every cluster's nodes are clones of a VM template that `kubehenge template create` makes through the Proxmox VE API ([templates.md](templates.md)):

| Distribution | Template |
| --- | --- |
| RKE2, k3s | one each per node OS: `kubehenge template create rke2 --node pve1`, `kubehenge template create k3s --node-os rocky-10 --node pve1` |
| kubeadm | one per node OS and Kubernetes version: `kubehenge template create kubeadm --kubernetes-version v1.36.5 --node pve1` |
| Talos | one per Talos release, Talos being its own OS: `kubehenge template create talos --talos-version v1.14.5 --node pve1` |

Create them after the management cluster, which makes their pool, `kubehenge-templates`. A kubeadm cluster can only be created at, or upgraded to, a version that has a template on its node OS. A Talos cluster can only be created on a Talos release that has one.

### Building from the interface

Under **Administration**, **Templates**, **VM templates** lists them, each with a cluster that clones from it. **Build template** makes one with your own Proxmox VE session, as the setup does:

| Field | What it is |
| --- | --- |
| Distribution | RKE2, k3s, kubeadm with its Kubernetes version, such as `v1.36.5`, or Talos with its release, such as `v1.14.5`. A Talos template is the Image Factory's image, imported without being booted. The node OS and build VM fields do not apply to it. |
| Node OS | The operating system of the nodes, AlmaLinux 10 unless chosen otherwise ([templates.md](templates.md#node-oses)). |
| Proxmox VE node | The node the image is prepared on. For a node OS above `x86-64-v2`, one whose CPU reaches its level. |
| Template disk | Where the template's disk goes. Shared storage, listed first, lets clones start on any node of a Proxmox VE cluster. |
| Downloaded image | A storage with the `import` content type, for the node OS's downloaded cloud image, or Talos's. |
| Cloud-init image | A storage with the `iso` content type, used while the image is prepared. The node's first one when left empty. |
| Bridge, VLAN tag | Where the VM that prepares the image attaches. It needs to reach the package repositories ([above](#internet-access)). |
| Static address | Optional: an address with prefix length, a gateway and DNS servers for that VM, instead of DHCP. |

A build takes about twenty minutes, most of it composing the node image from the node OS's packages. The page follows its log. Only one runs at a time, never during a run of the management cluster or an upgrade of the appliance.

Each node OS template keeps several builds, listed newest first with the time each was made. Building one that exists makes a new build beside it. A build a cluster still clones from cannot be deleted. For RKE2 and k3s that is the one its nodes run, for kubeadm the one a cluster or machine runs at its version.

Below the list, **Automatic builds** shows when the weekly build next runs and how its last run went, with its log. **Build now** has the management cluster run it at once ([templates.md](templates.md#the-weekly-build)). After the first templates, it keeps them current on its own. There is nothing to build by hand but a kubeadm version no cluster runs yet.

The same is possible from any Linux machine that reaches the Proxmox VE API, with `kubehenge template create` and an API token ([templates.md](templates.md#requirements) lists its privileges). `--node`, the Proxmox VE node the template is built on, is required. Keep the default of `--pool` there, because Cluster API may clone only from the template pool.

## Alternative: Terraform/OpenTofu

The management cluster can be applied with Terraform/OpenTofu instead of from the appliance. It is the same module, `management-cluster/`, with every variable open. The web interface comes with the appliance only, so clusters are then created and looked after with kubectl ([clusters.md](clusters.md#with-kubectl)).

Take the module from the release archive, `kubehenge_<version>_linux_amd64.tar.gz`. Its `share/kubehenge/` holds `management-cluster/` with the `charts/` it installs beside it. Its chart runs the release's controller image by digest. `bin/kubehenge` in the same archive is the command for the VM templates.

### Before you apply

- **Proxmox VE 9**, and the `import` content type on the storage named in `image_datastore`.
- **An API token with administrator rights**, since the module creates users, roles, pools and VMs.
  - For example `pveum user token add root@pam kubehenge --privsep 0`.
  - Or use an administrator's session. Leave `proxmox_api_token` unset and put a ticket and its CSRF token from `POST /access/ticket` in `PROXMOX_VE_AUTH_TICKET` and `PROXMOX_VE_CSRF_PREVENTION_TOKEN`.
  - The apply must then finish within the ticket's two hours.
- **The Proxmox VE CA certificate**, `/etc/pve/pve-root-ca.pem` on any node unless you replaced the API's certificate. The machine running Terraform/OpenTofu must trust it too (add it to the system's trust store).
- **The addresses and network access** [above](#network-and-addresses). The machine running Terraform/OpenTofu takes the appliance's place and must reach the nodes on ports 50000 and 6443.

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

`-lockfile=readonly` holds the providers to the checksums in the module's `.terraform.lock.hcl`, as the appliance does. The module is tested with OpenTofu and also declares Terraform 1.11 or newer. The state holds the cluster's secrets and its administrator credentials. Keep it in an encrypted backend, or use OpenTofu's state encryption.

### Variables

| Variable | Default | What it is |
| --- | --- | --- |
| `proxmox_endpoint` | required | The Proxmox VE API, such as `https://pve.example.org:8006`. Every cluster's nodes reach Proxmox VE at this address too. |
| `proxmox_api_token` | none | `user@realm!name=secret`, with administrator rights. Unset, a session from the environment is used. |
| `proxmox_ca_certificate` | required | PEM of the CA that signed the API's certificate. Cluster API's Proxmox VE provider verifies the API against it. |
| `proxmox_insecure` | `false` | Skip verifying the API's certificate everywhere. That covers this configuration, Cluster API, Kubehenge's controller, the template builds and the clusters' cloud controller manager and CSI plugin. |
| `proxmox_nodes` | required | The nodes the management cluster's VMs run on, taken in turn, such as `["pve1", "pve2", "pve3"]`. Each needs the bridge and both storages below. A VM given another node is migrated there. |
| `image_datastore` | `local` | Storage the Talos image is downloaded to, on each node. It must allow `import`. |
| `vm_datastore` | `local-lvm` | Storage for the management cluster's disks. |
| `bridge`, `vlan_id` | `vmbr0`, none | Bridge and VLAN tag of the management cluster's VMs. |
| `cluster_name` | `kubehenge` | The management cluster's name, also the prefix of its VMs' names and of the users, roles and group the module creates. |
| `node_addresses` | required | One, three or five addresses with prefix length, such as `["192.0.2.11/24"]`. |
| `api_address` | required | The Kubernetes API's virtual IP, in the nodes' subnet and unused. |
| `gateway`, `dns_servers` | required | The nodes' gateway and DNS servers. |
| `pod_subnets`, `service_subnets` | `["10.244.0.0/16"]`, `["10.96.0.0/12"]` | The management cluster's own ranges. The plan refuses ranges that overlap the nodes' network. |
| `node_cores`, `node_memory_mb`, `node_disk_gb` | `4`, `4096`, `32` | Size of each node. |
| `cpu_type` | `x86-64-v2-AES` | QEMU CPU type. Talos needs at least x86-64-v2. `host` spares the nodes mitigations their CPU does not need. It lets them migrate only between nodes with the same CPU ([templates.md](templates.md#how-clusters-find-them)). |

| `workload_pool` | `kubehenge` | Pool for the clusters' VMs, each cluster in a pool of its own under it. A pool name, nested at most one level. |
| `template_pool` | `kubehenge-templates` | Pool of the VM templates. |
| `management_pool` | `kubehenge-management` | Pool of the management cluster's own VMs, out of Cluster API's reach. |
| `workload_datastores` | `["local-lvm"]` | Storages clusters may use. |
| `workload_bridges` | `["vmbr0"]` | Bridges clusters may attach to, with any VLAN on them. |
| `bootstrap_datastore` | `local` | Storage for cloud-init ISOs. Cluster API gets full control of it. |
| `registry_mirrors` | `{}` | Registry mirrors for the management cluster's nodes, by registry name. Each takes endpoint URLs tried in order before the registry itself. A mirror of `factory.talos.dev`, `ghcr.io` or `registry.k8s.io` must serve the images' cosign signatures too. The nodes require them on Sidero's and Kubernetes' images and on the Talos providers'. |
| `backup_targets` | `[]` | S3 stores clusters back up to with Velero ([details](install-settings.md#backup-targets)). Up to sixteen, each with `name` and `bucket`. Optional fields are `endpoint` (https, AWS S3 when unset), `region`, `prefix`, `ca_certificate` and `checksums` (default true). Access keys are stored through the interface, or named per cluster with `existingSecret` ([clusters.md](clusters.md#variables)). |
| `cluster_oidc` | `[]` | OpenID Connect providers every new cluster trusts unless it names its own. Up to eight, each with `issuer_url` (https) and `client_id`. Optional fields are `username_claim`, `username_prefix`, `groups_claim`, `groups_prefix`, `scopes` and `certificate_authority`. |
| `version_overrides` | `{}` | Versions to use instead of the tested set, by its keys, to hold a component back. A provider held back needs its image in `provider_images` and its files' digests in `provider_manifests`. Lowering a version on a running cluster is not supported. |
| `provider_images` | `{}` | Controller images by digest to run instead of the tested ones, by `"<kind>/<name>"`, such as `"InfrastructureProvider/proxmox"`. |
| `provider_manifests` | `{}` | The SHA-256 of provider files outside the tested set, by their address under `https://github.com/`, such as `"kubernetes-sigs/cluster-api/releases/download/v1.14.2/core-components.yaml"`. These are `metadata.yaml` and the components file of each provider held back. |
| `controller_image` | the chart's | Kubehenge's controller image by digest, as `repository@sha256:<digest>`. A release's chart names the image it was released with. A checkout's names none, so a module taken from git needs one. |
| `proxmox_ha` | `false` | Keep each cluster's control-plane VMs on separate nodes with Proxmox VE HA. It uses a token with `Sys.Audit` and `Sys.Console` on `/` that only a short-lived Job per write holds ([details](install-settings.md#proxmox-ve-ha)). After setting it back to false, remove the HA resources and rules whose comment starts with `Kubehenge cluster`, as the appliance does. Without the token the controller cannot. |
| `offer_ha_rules` | `false` | Offer Proxmox VE's HA node-affinity rules (names and nodes) to those with `VM.Allocate` on the workload pool or a cluster's pool who lack `Sys.Audit` on `/`. The controller reads them with its own token and asks nothing new of Proxmox VE ([details](install-settings.md#ha-rule-placements)). |
| `cluster_datastore_allocate` | `false` | Let clusters choose the CSI plugin's newest release. The controller grants the cluster's token `Datastore.Allocate` on a storage of its own ([details](install-settings.md#csi-plugin)). |
| `template_refresh` | [below](#template_refresh-fields) | The weekly build of the node OS VM templates ([templates.md](templates.md#the-weekly-build)) and its own API token ([details](install-settings.md#what-the-template-builds-may-do)). |
| `cluster_sys_audit` | `true` | Give each cluster the cloud controller manager's HA group labels unless it switches them off. This gives the cluster's token `Sys.Audit` on `/`, unpropagated, through a group the controller adds its user to. It reads the Proxmox VE cluster's configuration and changes nothing ([details](install-settings.md#ha-group-labels)). `false` gives no cluster `Sys.Audit` on `/`. |
| `watch` | `stall_minutes = 20` | `stall_minutes` (1 to 1440) is how long Kubehenge's controller waits before it reports a stall on the Cluster and the Health page. That covers a new machine in the roll of a cluster's only control plane becoming ready ([troubleshooting.md](troubleshooting.md#one-control-plane-its-roll-stalled)) and a cluster's deletion finishing ([troubleshooting.md](troubleshooting.md#a-cluster-does-not-finish-deleting)). |
| `node_boot_logs` | `true` | Let the controller read kubeadm, RKE2 and k3s nodes' boot logs through their guest agents (files only). Its token gets `VM.GuestAgent.FileRead` on the workload pool ([details](install-settings.md#nodes-boot-logs)). `false` takes the privilege away. Talos nodes' boot logs are read through their Talos API either way. |
| `extra_cluster_namespaces` | `[]` | Namespaces the interface creates and manages clusters in beside `clusters` ([clusters.md](clusters.md#more-namespaces-for-clusters)). Each is created if missing and kept when taken off the list. `default`, `cert-manager`, `kube-*` and `*-system` are refused. |
| `etcd_restore` | `false` | Have the controller restore clusters' etcd itself through the VMs' guest agents. It uses a token holding `VM.Audit` and `VM.GuestAgent.Unrestricted` on the workload pool that only a short-lived Job per step holds ([details](install-settings.md#etcd-restores)). Without it, the interface shows the steps to restore by hand. |
| `headlamp_sign_in` | `false` | Have the appliance's shared Headlamp sign those who may download a cluster's admin kubeconfig in as cluster-admin. It does so through two ServiceAccounts the controller keeps in every cluster of the interface's namespaces ([details](install-settings.md#headlamp-sign-in)). Without the appliance it does nothing but keep those accounts. |
| `headlamp_actions` | `false` | Have the appliance's shared Headlamp offer the Helm app catalog and the Prometheus charts it ships with. They act with each user's own credentials in the cluster ([details](install-settings.md#headlamp-actions)). Without the appliance it does nothing. |
| `api_audit_log` | `false` | Have the management cluster's API server log each change people make, which the interface's Activity page then lists with its user ([details](install-settings.md#kubectl-changes)). |

#### `template_refresh` fields

| Field | Default | What it is |
| --- | --- | --- |
| `enabled` | `true` | Whether the weekly build runs. |
| `weekday`, `time`, `time_zone` | `Sun`, `01:00`, `UTC` | When it runs. `time` is `HH:MM`. |
| `soak_hours` | `24` | How old a build is before clusters move to it, 0 to 720. |
| `node` | the first of `proxmox_nodes` | The node the builds run on. |
| `storage` | the first of `workload_datastores` | The template disks' storage. |
| `import_datastore`, `iso_datastore` | `image_datastore`, `bootstrap_datastore` | Storages for the downloaded image and the cloud-init ISO. |
| `bridge`, `vlan` | the first of `workload_bridges`, none | Where the VM that prepares an image attaches. |
| `address`, `gateway`, `dns` | DHCP | A static address with prefix, gateway and DNS servers for that VM. |
| `creator_builds` | `false` | Lets those who may create clusters have a missing template built. |

Outputs: `kubeconfig` and `talosconfig` (sensitive), `api_endpoint`, and `proxmox_objects`, which lists everything the module made on Proxmox VE.

Then create the [VM templates](#create-the-vm-templates) with `bin/kubehenge` from the archive, on any Linux machine that reaches the Proxmox VE API:

```sh
export KUBEHENGE_PROXMOX_URL=https://pve.example.org:8006
export KUBEHENGE_PROXMOX_CA=pve-root-ca.pem
export KUBEHENGE_PROXMOX_TOKEN='root@pam!kubehenge=...'
bin/kubehenge template create rke2 --node pve1 --storage shared-storage
```

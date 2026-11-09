### Added

- A release mirror serves the RKE2 and k3s releases in place of GitHub ([install.md](install.md#mirroring-the-releases)). Nodes, the interface and `kubehenge release pin --mirror` read from it.
- An RKE2 or k3s cluster can name its own mirror in `releaseMirror`, in the wizard or Options ([clusters.md](clusters.md#a-clusters-own-release-mirror)). It replaces the management cluster's for its nodes.
- A node whose release download fails names the file and the mirror or GitHub.

### Security

- Release files that do not match their pins are refused before anything is installed.

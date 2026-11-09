### Added

- The Templates page names each template on a down node, and the controller logs it.
- A cluster whose machines would clone from a template on a down node is refused at creation.
- An existing cluster has the condition `TemplateNodeDown`, shown as **Template node down**, until the node is back.
- A node image refresh skips a build whose template is on a down node.
- The cluster keeps its build with the condition `NodeImageHeld`, and the refresh goes on once the node is back.

### Fixed

- A template on a down Proxmox VE node no longer fails the Templates page, the template refresh or the create wizard's versions.

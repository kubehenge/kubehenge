### Added

- A cluster whose machines would clone from a template on a down node is refused at creation. An existing one shows **Template node down** (`TemplateNodeDown`) until the node is back.
- A node image refresh skips a build whose template is on a down node. The cluster keeps its build with the condition `NodeImageHeld` until the node is back.

### Fixed

- A template on a down Proxmox VE node no longer fails the Templates page, the template refresh or the create wizard's versions. The page names each such template.

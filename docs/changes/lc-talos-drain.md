### Added

- The cluster page names the budget and pod that hold a drain.

### Changed

- A Talos upgrade drains a node for its drain timeout (60 minutes by default), not a fixed five.
- A Talos upgrade first waits for the StatefulSets with a ready pod on the node.

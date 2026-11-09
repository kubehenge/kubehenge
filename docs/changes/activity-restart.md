### Fixed

- Activity keeps a kubectl change made before an API server restarted, also when the interface first sees it after the restart.
- Activity follows the management cluster's API servers from setup, so it no longer misses a change made right after.

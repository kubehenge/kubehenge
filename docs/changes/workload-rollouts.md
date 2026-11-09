### Fixed

- A removed machine waits at most 60 minutes for its node to drain and 10 for its volumes to detach, where a rollout could stall for ever. `kubehenge cluster timeouts` changes both.

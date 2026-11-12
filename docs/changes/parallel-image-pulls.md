### Fixed

- Nodes pull up to three images at once, so a pull stuck on one slow registry no longer holds every later image of the node. Nodes roll to pick this up.
- kubeadm nodes cancel a stalled image request after five minutes and retry it.

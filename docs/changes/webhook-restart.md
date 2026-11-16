### Fixed

- Applying the management cluster runs again when one of its webhooks does not answer yet, instead of failing. A Talos upgrade that restarts a node causes this ([troubleshooting](troubleshooting.md#setting-up-the-management-cluster-fails)).

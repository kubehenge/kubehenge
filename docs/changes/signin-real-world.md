### Added

- A user whose only second factor is a security key signs in with a Proxmox VE API token (`Use API token` on the sign-in page). The session has the token's privileges and the usual limits.
- Behind a reverse proxy listed in `--trusted-proxy` (`KUBEHENGE_TRUSTED_PROXIES`), the sign-in limits count the client in `X-Forwarded-For`. `install.sh --upgrade --no-trusted-proxy` removes the list.
- The install guide covers reverse proxies. The interface needs a host name of its own, not a path prefix, and the proxy forwards Headlamp's port 8443 too.

### Fixed

- A tab whose session another tab replaced picks up the new CSRF token after a refused change and asks for a retry. It returns to sign-in when the other tab signed in as someone else.
- OpenID sign-in works from any name the interface answers to, where the provider's return lost the state cookie.

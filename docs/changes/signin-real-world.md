### Added

- A user whose only second factor is a security key signs in with a Proxmox VE API token (`Use API token` on the sign-in page).
- A token session has the token's privileges, a CSRF token and the usual limits.
- The `--trusted-proxy` option of `kubehenge serve` and the installer (`KUBEHENGE_TRUSTED_PROXIES`) lists reverse proxies.
- Behind a listed reverse proxy, the sign-in limits count the client in `X-Forwarded-For`.
- `install.sh --upgrade --no-trusted-proxy` removes the list.
- The install guide covers reverse proxies.
- Behind a reverse proxy the interface needs a host name of its own, not a path prefix, and Headlamp's port 8443 too.

### Fixed

- A tab whose session another tab replaced picks up the new CSRF token after a refused change and asks for a retry.
- Such a tab returns to sign-in when the other tab signed in as someone else.
- OpenID sign-in works from any name the interface answers to.
- An OpenID start under another name moves to `--public-url` first, so the provider's return finds the state cookie.

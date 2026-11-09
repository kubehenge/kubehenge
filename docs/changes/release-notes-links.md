### Fixed

- Links in a release's notes on GitHub open the docs page on kubehenge.dev, not a missing page.
- Links on kubehenge.dev to a heading with an apostrophe, such as "Back up the management cluster's etcd", reach the heading.

### Internal

- `scripts/changelog.sh check` refuses a link to a page outside `docs/` or to a heading its page lacks.
- Each Makefile target has its own `.PHONY` line beside its rule, so pull requests that add targets no longer conflict.

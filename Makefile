TOFU ?= tofu
MODULES := management-cluster
# Every tofu here installs the providers from this mirror alone (`make providers`).
PROVIDERS := dist/cache/providers
export TF_CLI_CONFIG_FILE := $(CURDIR)/$(PROVIDERS)/tofu.tfrc
# Where `make image` pushes the controller's image, `make capmox-image` capmox's and
# `make helm-addon-image` the Add-on Provider for Helm's; KO_FLAGS such as --tags or
# --insecure-registry.
IMAGE ?= ghcr.io/kubehenge/kubehenge
CAPMOX_IMAGE ?= ghcr.io/kubehenge/cluster-api-provider-proxmox
HELM_ADDON_IMAGE ?= ghcr.io/kubehenge/cluster-api-addon-provider-helm
KO_FLAGS ?=
# Where scripts/ko.sh builds the ko every image build runs, beside Go's own directory.
KO_DIR ?= $(subst \,/,$(shell go env GOPATH))/kubehenge-ko

# Independent steps, such as the lint and unit steps of `make test`, run side by side, one per CPU;
# each step's output is printed together once it ends, unless OUTPUT_SYNC=none asks for it as it
# comes, as the end-to-end runs do.
OUTPUT_SYNC ?= target
MAKEFLAGS += --jobs=$(shell getconf _NPROCESSORS_ONLN) --output-sync=$(OUTPUT_SYNC)
# The symbol table and DWARF go: a third of the binary, and stack traces keep their names without them.
LDFLAGS := -s -w -buildid=

.PHONY: build
build: web gateway-api
	CGO_ENABLED=0 go build -trimpath -ldflags='$(LDFLAGS)' -o dist/kubehenge ./cmd/kubehenge

# The Gateway API's standard channel, which the controller embeds and applies to every workload
# cluster: upstream's install file for one release, downloaded again only when the release changes
# and refused unless it matches its SHA-256. Every Go step waits for it, as for the frontend; .keep
# is what lets Go compile pkg/provisioner before it is there.
# renovate: datasource=github-release-attachments depName=kubernetes-sigs/gateway-api
GATEWAY_API_TAG := v1.7.0
GATEWAY_API_SHA256 := 4e2602c4cbe1afbc394d86f4ac6522e1a5263b01133bba1e40a6d1a549e753a1
GATEWAY_API := pkg/provisioner/gatewayapi/standard-install-$(GATEWAY_API_TAG).yaml
$(GATEWAY_API):
	scripts/fetch.sh https://github.com/kubernetes-sigs/gateway-api/releases/download/$(GATEWAY_API_TAG)/standard-install.yaml $@.part
	echo '$(GATEWAY_API_SHA256)  $@.part' | sha256sum -c --quiet - || { rm -f $@.part; exit 1; }
	rm -f pkg/provisioner/gatewayapi/standard-install-*.yaml
	mv $@.part $@

.PHONY: gateway-api
gateway-api: $(GATEWAY_API)

# The Proxmox VE provider's CustomResourceDefinitions, which the chart's test validates the rendered
# ProxmoxMachineTemplate against. The release and SHA-256 are the integration tier's pin, so
# Renovate moves one place.
CAPMOX_TAG := $(shell sed -n 's/^CAPMOX=//p' test/integration/assets.sh)
CAPMOX_SHA256 := $(shell sed -n 's/^CAPMOX_SHA256=//p' test/integration/assets.sh)
CAPMOX_CRDS := test/crds/capmox-$(CAPMOX_TAG).yaml
$(CAPMOX_CRDS):
	scripts/fetch.sh https://github.com/ionos-cloud/cluster-api-provider-proxmox/releases/download/$(CAPMOX_TAG)/infrastructure-components.yaml $@.part
	echo '$(CAPMOX_SHA256)  $@.part' | sha256sum -c --quiet - || { rm -f $@.part; exit 1; }
	rm -f test/crds/capmox-*.yaml
	mv $@.part $@

.PHONY: capmox-crds
capmox-crds: $(CAPMOX_CRDS)

# The Cluster API operator's chart, which the management-cluster module installs from beside it and
# so the appliance's archive carries: the archive of the release management-cluster/versions.tf
# names, downloaded again only when that changes and refused unless it matches the SHA-256 named
# there, which the module checks again before installing it.
CAPI_OPERATOR_TAG := $(shell sed -n 's/^ *chart *= *"\(v[^"@]*\)"$$/\1/p' management-cluster/versions.tf)
CAPI_OPERATOR_SHA256 := $(shell sed -n 's/^ *chart_sha256 *= *"\([0-9a-f]*\)"$$/\1/p' management-cluster/versions.tf)
CAPI_OPERATOR_CHART := management-cluster/charts/cluster-api-operator-$(CAPI_OPERATOR_TAG:v%=%).tgz
$(CAPI_OPERATOR_CHART):
	scripts/fetch.sh https://github.com/kubernetes-sigs/cluster-api-operator/releases/download/$(CAPI_OPERATOR_TAG)/$(notdir $@) $@.part
	echo '$(CAPI_OPERATOR_SHA256)  $@.part' | sha256sum -c --quiet - || { rm -f $@.part; exit 1; }
	rm -f management-cluster/charts/cluster-api-operator-*.tgz
	mv $@.part $@

.PHONY: capi-operator-chart
capi-operator-chart: $(CAPI_OPERATOR_CHART)

# The providers the module's lock file pins, for this machine's tofu, checked against the lock file
# (scripts/providers.sh). OpenTofu's own install downloads them from GitHub without a token, which
# fails for as long as GitHub turns away anonymous requests.
$(PROVIDERS)/tofu.tfrc: management-cluster/.terraform.lock.hcl scripts/providers.sh
	TOFU=$(TOFU) scripts/providers.sh management-cluster/.terraform.lock.hcl $(PROVIDERS)

.PHONY: providers
providers: $(PROVIDERS)/tofu.tfrc

# The frontend's packages, installed again only when the lockfile changes.
web/node_modules/.package-lock.json: web/package-lock.json
	cd web && npm ci --no-fund --no-audit
	touch $@

# The web interface's frontend, built into pkg/ui/dist, which the binary embeds, and built again only
# when a source changes. Vite empties the directory first, so nothing may compile pkg/ui meanwhile:
# every Go step waits for this one. .keep is what lets Go compile pkg/ui before the first build.
WEB_SOURCES := $(shell find web/src -type f) web/index.html web/package.json web/tsconfig.json web/vite.config.ts
pkg/ui/dist/index.html: web/node_modules/.package-lock.json $(WEB_SOURCES)
	cd web && npm run build
	touch pkg/ui/dist/.keep

.PHONY: web
web: pkg/ui/dist/index.html

# The website (kubehenge.dev): docs/ with a home page, built into website/.vitepress/dist.
.PHONY: website
website:
	cd website && npm ci --no-audit --no-fund && npm run build

# Folds the changelog fragments of docs/changes/ into the release's section of docs/changelog.md:
# make changelog VERSION=0.4.3
.PHONY: changelog
changelog:
	sh scripts/changelog.sh fold $(if $(filter command line,$(origin VERSION)),$(VERSION),$(error VERSION is not set: make changelog VERSION=0.4.3))

# Checks over the built website in Playwright's Chromium. First website/check-layers.js: no
# composited layer on the home page over 1.5 windows at a phone and a desktop size, and no motion
# under reduced motion. Then Lighthouse CI on loopback, for mobile and then desktop: the limits in
# website/lighthouserc.cjs against the median of 5 runs. The reports go to website/.vitepress/lighthouse.
.PHONY: website-check
website-check: website
	cd website && npm exec --no -- playwright install chromium
	cd website && npm run layers
	set -e; cd website && rm -rf .vitepress/lighthouse; for form in mobile desktop; do \
		rm -rf .lighthouseci; \
		KUBEHENGE_LIGHTHOUSE=$$form npm exec --no -- lhci autorun --config=lighthouserc.cjs; \
	done

# The links inside the built website (website/lychee.toml): each must land on a page and a heading
# the site made, which the build alone does not check. Run it after `make website`.
.PHONY: website-links
website-links:
	lychee --config website/lychee.toml --root-dir "$$PWD/website/.vitepress/dist" 'website/.vitepress/dist/**/*.html'

# The outside links of the README, docs/ and the built website (lychee.toml). A dead outside link
# fails the nightly run, never a pull request.
.PHONY: links
links: website
	lychee --config lychee.toml README.md docs website/index.md 'website/.vitepress/dist/**/*.html'

# The interface's screenshots in docs/images: `kubehenge serve --demo` in a headless Chromium, the
# one Playwright pins in web/package-lock.json.
.PHONY: screenshots
screenshots: web gateway-api
	CGO_ENABLED=0 go build -trimpath -o dist/screenshots/kubehenge$(shell go env GOEXE) ./cmd/kubehenge
	cd web && npm exec --no -- playwright install --only-shell chromium
	cd web && node screenshots.ts ../dist/screenshots/kubehenge$(shell go env GOEXE) ../docs/images

# The appliance's release archive, which appliance/install.sh installs: the binary, the
# management-cluster module with the charts it installs, the shared Headlamp (its server and
# frontend, taken from its Linux release) and the service units.
# The release keys the appliance verifies upgrades with are the lines of pkg/release/release.pub
# unless RELEASE_PUBLIC_KEYS names others (base64 Ed25519 public keys, comma-separated).
VERSION ?= 0.1.0
RELEASE_PUBLIC_KEYS ?= $(shell tr -s '\r\n' ',' < pkg/release/release.pub 2>/dev/null)
RELEASE_LDFLAGS = $(LDFLAGS) -X github.com/kubehenge/kubehenge/pkg/release.Version=$(VERSION) -X github.com/kubehenge/kubehenge/pkg/release.publicKeys=$(RELEASE_PUBLIC_KEYS)
# The controller image the release's chart runs, by digest (repository@sha256:...); unset, the chart
# names none, and the module needs controller_image.
CONTROLLER_IMAGE ?=
# renovate: datasource=github-release-attachments depName=kubernetes-sigs/headlamp
HEADLAMP_TAG := v0.46.0
HEADLAMP_SHA256 := 1747c5de63963be925ded64dc2a5308144d260e43452855169bc771f03846434
HEADLAMP_VERSION := $(HEADLAMP_TAG:v%=%)
HEADLAMP_TARBALL := dist/cache/Headlamp-$(HEADLAMP_VERSION)-linux-x64.tar.gz
UNITS := kubehenge.service kubehenge-update.service kubehenge-update.path kubehenge-headlamp.service kubehenge-headlamp.socket kubehenge-headlamp-proxy.service
.PHONY: dist
dist: web gateway-api capi-operator-chart
	rm -rf dist/stage && mkdir -p dist/stage/bin dist/stage/share/kubehenge/appliance dist/stage/share/kubehenge/headlamp/plugins dist/cache
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='$(RELEASE_LDFLAGS)' \
		-o dist/stage/bin/kubehenge ./cmd/kubehenge
	tar -cf - --exclude=.terraform --exclude=tests --exclude=.tflint.hcl --exclude='*.tfstate*' --exclude='*.go' management-cluster charts | tar -xf - -C dist/stage/share/kubehenge
	# The appliance's OpenTofu installs the module's providers from these alone, so an apply needs no
	# download, on a closed network too.
	scripts/providers.sh management-cluster/.terraform.lock.hcl $(PROVIDERS) linux_amd64 > /dev/null
	mkdir dist/stage/share/kubehenge/providers
	cd $(PROVIDERS) && find registry.opentofu.org -name '*_linux_amd64.zip' | tar -cf - -T - | tar -xf - -C $(CURDIR)/dist/stage/share/kubehenge/providers
	if [ -n '$(CONTROLLER_IMAGE)' ]; then \
		sed -i 's|^image: ""$$|image: "$(CONTROLLER_IMAGE)"|' dist/stage/share/kubehenge/charts/kubehenge/values.yaml && \
		grep -qxF 'image: "$(CONTROLLER_IMAGE)"' dist/stage/share/kubehenge/charts/kubehenge/values.yaml; \
	fi
	test -f $(HEADLAMP_TARBALL) || scripts/fetch.sh https://github.com/kubernetes-sigs/headlamp/releases/download/$(HEADLAMP_TAG)/Headlamp-$(HEADLAMP_VERSION)-linux-x64.tar.gz $(HEADLAMP_TARBALL)
	echo '$(HEADLAMP_SHA256)  $(HEADLAMP_TARBALL)' | sha256sum -c --quiet - || { rm -f $(HEADLAMP_TARBALL); exit 1; }
	# Of the plugins Headlamp ships, the Helm app catalog and the Prometheus charts, which the interface
	# serves while the shared Headlamp offers its actions; the plugin catalog would install any plugin.
	tar -xzf $(HEADLAMP_TARBALL) -C dist/stage/share/kubehenge/headlamp --strip-components=2 \
		$(foreach f,headlamp-server frontend LICENSE NOTICE .plugins/app-catalog .plugins/prometheus,Headlamp-$(HEADLAMP_VERSION)-linux-x64/resources/$(f))
	mv dist/stage/share/kubehenge/headlamp/.plugins dist/stage/share/kubehenge/headlamp/static-plugins
	cp $(addprefix appliance/,$(UNITS)) dist/stage/share/kubehenge/appliance/
	mkdir -p dist/stage/share/kubehenge/pve-entry
	cp $(addprefix appliance/pve-entry/,install.sh kubehenge-entry.js README.md) dist/stage/share/kubehenge/pve-entry/
	cp LICENSE dist/stage/share/kubehenge/
	printf '%s\n' '$(VERSION)' > dist/stage/share/kubehenge/VERSION
	# Modes are set here, not taken from the build machine's file system, which may keep none.
	tar -cf dist/stage.tar --owner=0 --group=0 --sort=name --mode=u=rwX,go=rX \
		--exclude=./bin/kubehenge --exclude=./share/kubehenge/headlamp/headlamp-server -C dist/stage .
	tar -rf dist/stage.tar --owner=0 --group=0 --mode=0755 -C dist/stage ./bin/kubehenge ./share/kubehenge/headlamp/headlamp-server
	gzip -n -c dist/stage.tar > dist/kubehenge_$(VERSION)_linux_amd64.tar.gz && rm dist/stage.tar
	cd dist && sha256sum kubehenge_$(VERSION)_linux_amd64.tar.gz > kubehenge_$(VERSION)_SHA256SUMS
	rm -rf dist/stage
	# The install script carries the version and checksum of the archive it installs.
	sed -e 's/^KUBEHENGE_VERSION=.*/KUBEHENGE_VERSION=$(VERSION)/' \
		-e "s/^KUBEHENGE_SHA256=.*/KUBEHENGE_SHA256=$$(cut -d' ' -f1 dist/kubehenge_$(VERSION)_SHA256SUMS)/" \
		appliance/install.sh > dist/install.sh
	cd dist && sha256sum install.sh >> kubehenge_$(VERSION)_SHA256SUMS

# Pins the RKE2, k3s and Talos releases published since (pkg/provisioner/releasepins.txt), so that
# clusters may run them. GH_TOKEN, when set, reads GitHub's API with a higher rate limit.
.PHONY: release-pins
release-pins: web gateway-api
	go run ./cmd/kubehenge release pins pkg/provisioner/releasepins.txt

# The release index to sign: the previous release's (PREVIOUS, its .sig beside it), checked against
# the release keys, with this release added. Run after dist.
TOFU_VERSION = $(patsubst v%,%,$(shell sed -n 's/^TOFU_TAG=//p' appliance/install.sh))
TOFU_SHA256 = $(shell sed -n 's/^TOFU_SHA256=//p' appliance/install.sh)
.PHONY: index
index: gateway-api
	CGO_ENABLED=0 go build -trimpath -ldflags='$(RELEASE_LDFLAGS)' -o dist/kubehenge-release ./cmd/kubehenge
	dist/kubehenge-release release index $(if $(PREVIOUS),--previous $(PREVIOUS)) --version $(VERSION) \
		--url https://github.com/kubehenge/kubehenge/releases/download/v$(VERSION)/kubehenge_$(VERSION)_linux_amd64.tar.gz \
		--sha256 $$(sed -n 's/  kubehenge_$(VERSION)_linux_amd64.tar.gz$$//p' dist/kubehenge_$(VERSION)_SHA256SUMS) \
		--tofu-version $(TOFU_VERSION) \
		--tofu-url https://github.com/opentofu/opentofu/releases/download/v$(TOFU_VERSION)/tofu_$(TOFU_VERSION)_linux_amd64.tar.gz \
		--tofu-sha256 $(TOFU_SHA256) > dist/index.json

# Builds and pushes the image the management cluster runs the controller from, and prints its
# reference by digest. ko's own SBOM would be pushed beside it unsigned; the release attests one
# instead (scripts/sbom.sh).
.PHONY: image
image: web gateway-api
	sh scripts/ko.sh $(KO_DIR)
	KO_DOCKER_REPO=$(IMAGE) KO_CONFIG_PATH=cmd/kubehenge $(KO_DIR)/ko build --bare --sbom=none $(KO_FLAGS) ./cmd/kubehenge

# Build and push the provider images the management cluster runs as Kubehenge builds them
# (capmox_build and helm_addon_build in management-cluster/versions.tf), check their digests and
# print their references by digest.
.PHONY: capmox-image
capmox-image:
	sh scripts/provider-image.sh $(KO_DIR) capmox $(CAPMOX_IMAGE) $(KO_FLAGS)

.PHONY: helm-addon-image
helm-addon-image:
	sh scripts/provider-image.sh $(KO_DIR) helm-addon $(HELM_ADDON_IMAGE) $(KO_FLAGS)

# Check the providers the management cluster runs from Kubehenge's forks: their files and images
# by the digests versions.tf names, and their provenance and signatures from each fork's release.
.PHONY: verify-providers
verify-providers:
	sh scripts/verify-providers.sh

# The Talos pins, each image naming its release and signed by Sidero. Needs cosign, as
# scripts/tools.sh installs it.
.PHONY: verify-talos-pins
verify-talos-pins: web gateway-api
	sh scripts/verify-talos-pins.sh
	go run ./cmd/kubehenge release pins --check pkg/provisioner/releasepins.txt

# Lint and static tier: Go, the frontend, Terraform/OpenTofu, Helm charts, shell, the workflows,
# YAML, Markdown, the changelog, the e2e image's Dockerfile and the node images' Containerfiles,
# spelling and Renovate's config, each at its strictest settings; the files are the repository's
# own, as git lists them.
.PHONY: lint
lint: lint-go lint-gopls lint-web lint-tofu lint-helm lint-shell lint-actions lint-yaml lint-markdown lint-changelog lint-docker lint-typos

FILES = git ls-files --cached --others --exclude-standard
SHELL_SCRIPTS := test/e2e/*.sh test/e2e/ci/*.sh test/integration/*.sh test/unit/*.sh appliance/*.sh appliance/pve-entry/*.sh scripts/*.sh website/*.sh pkg/provisioner/*.sh pkg/templates/*/*.sh charts/kubehenge/files/*.sh
SHFMT_FLAGS := -i 4 -ci -sr
# Installed from lock files, which check every package's hash, their dependencies' included.
YAMLLINT := uv run --locked --project scripts/lint yamllint
ZIZMOR := uv run --locked --project scripts/lint zizmor
MARKDOWNLINT := npm --prefix web exec --no -- markdownlint-cli2
GOVULNCHECK := go tool govulncheck

# Rewrites what the format checks below would refuse.
.PHONY: fmt
fmt: web/node_modules/.package-lock.json
	golangci-lint fmt ./...
	$(TOFU) fmt -recursive
	shfmt $(SHFMT_FLAGS) -w $(SHELL_SCRIPTS)
	cd web && npm run fmt
	npm --prefix web exec --no -- oxfmt -c web/.oxfmtrc.json appliance/pve-entry $(WEBSITE_CODE)

# For both systems the code builds for: the appliance and images on Linux, the tests also on Windows.
# Without cgo, as the binary is built, and so that a cross-system check needs no C toolchain.
# govulncheck fails on a known vulnerability only where the code calls into it, for the system it
# runs on: GOOS would also build govulncheck itself for another one.
# golangci-lint gives up on its per-user lock at the first error from the lock file, even with
# --allow-serial-runners, and gates in several worktrees at once meet that error. Its cache is safe
# to share, so they lint side by side instead.
# A change to a package most others import makes the lint analyse nearly the whole module again.
# On every core at once that takes 6 GB and the whole machine. Four threads under a soft 2 GiB heap
# target take about 5 minutes and 4 GB, and an unchanged tree takes seconds from the cache.
# Even those seconds open every package's cache entries, so inputs that already passed are not
# linted again: the Go sources, tracked or not, the module, the config and the linter's version.
LINT_GO := CGO_ENABLED=0 GOMAXPROCS=4 GOMEMLIMIT=2GiB nice -n 10 golangci-lint run --allow-parallel-runners
LINT_GO_INPUTS := '*.go' go.mod go.sum .golangci.yml
.PHONY: lint-go
lint-go: web gateway-api
	go mod tidy -diff
	golangci-lint config verify
	key=$$({ git ls-files -s -- $(LINT_GO_INPUTS); git diff HEAD -- $(LINT_GO_INPUTS); \
		git ls-files -o --exclude-standard -z -- '*.go' | xargs -0 -r git hash-object; \
		golangci-lint version; } | sha256sum | cut -c1-16); \
	if [ -f "dist/lint-go/$$key" ]; then echo "golangci-lint: these inputs passed before"; \
	else GOOS=linux $(LINT_GO) ./... && GOOS=windows $(LINT_GO) ./... && \
		mkdir -p dist/lint-go && touch "dist/lint-go/$$key"; fi
	$(GOVULNCHECK) ./...

# The analyzers gopls shows in an editor, most of them internal to it (writestring, unusedfunc,
# infertypeargs, fmtappendf), so only `gopls check` runs them. It exits 0 with findings, so any
# output fails the step. Severity info and up: the hint tier is the modernize suite and
# deprecations, which golangci-lint already runs (staticcheck's SA1019 carries the reasons for the
# deprecated imports that stay). gopls is pinned in its own module, so its dependencies stay out of
# the one that ships. One process at a time, on two threads at low priority: gopls starts several
# analysis workers per file it is given, and side by side they hold every core.
# Inputs that already passed are not checked again, as for lint-go.
GOPLS_MOD := scripts/gopls/go.mod
LINT_GOPLS_INPUTS := '*.go' go.mod go.sum $(GOPLS_MOD) scripts/gopls/go.sum
.PHONY: lint-gopls
lint-gopls: web gateway-api
	key=$$({ git ls-files -s -- $(LINT_GOPLS_INPUTS); git diff HEAD -- $(LINT_GOPLS_INPUTS); \
		git ls-files -o --exclude-standard -z -- '*.go' | xargs -0 -r git hash-object; } | sha256sum | cut -c1-16); \
	if [ -f "dist/lint-gopls/$$key" ]; then echo "gopls check: these inputs passed before"; exit 0; fi; \
	gopls=$$(go tool -modfile=$(GOPLS_MOD) -n gopls) && log=$$(mktemp) && \
	if git ls-files '*.go' | GOFLAGS=-tags=e2e,integration GOMAXPROCS=2 GOMEMLIMIT=2GiB xargs -n 400 nice -n 19 "$$gopls" check -severity=info >"$$log" 2>&1 \
		&& [ ! -s "$$log" ]; then rm -f "$$log"; mkdir -p dist/lint-gopls && touch "dist/lint-gopls/$$key"; \
	else cat "$$log"; rm -f "$$log"; echo "gopls check: findings above (or gopls failed)"; exit 1; fi

# oxfmt's check, type-aware oxlint and TypeScript's check; the build leaves the types to this step.
# The Proxmox VE entry's script is plain JavaScript, checked with the interface's tools, and so is
# the formatting of the website's own code. stylelint checks the interface's and the website's CSS.
WEBSITE_CODE := website/prepare.js website/check-scripts.js website/check-layers.js website/.vitepress/config.mts website/.vitepress/theme website/lighthouserc.cjs
.PHONY: lint-web
lint-web: web/node_modules/.package-lock.json
	cd web && npm run lint
	npm --prefix web exec --no -- oxfmt --check -c web/.oxfmtrc.json appliance/pve-entry $(WEBSITE_CODE)
	npm --prefix web exec --no -- oxlint -c appliance/pve-entry/.oxlintrc.json --deny-warnings --report-unused-disable-directives appliance/pve-entry
	npm --prefix web exec --no -- stylelint --config web/stylelint.config.mjs --max-warnings 0 'web/src/**/*.css' 'website/.vitepress/theme/**/*.css'

# Spelling in every file git lists, in British English (.typos.toml).
.PHONY: lint-typos
lint-typos:
	typos

.PHONY: tofu-init
tofu-init: providers
	set -e; for m in $(MODULES); do $(TOFU) -chdir=$$m init -backend=false -input=false -lockfile=readonly >/dev/null; done

# fmt gets the configuration's directories: from the root it walks web/node_modules, which npm ci may be rebuilding beside it.
.PHONY: lint-tofu
lint-tofu: tofu-init
	$(TOFU) fmt -check -recursive -diff $(MODULES) test/e2e/ci
	set -e; for m in $(MODULES); do \
		$(TOFU) -chdir=$$m validate; \
		tflint --chdir=$$m --recursive --config=$(CURDIR)/management-cluster/.tflint.hcl; \
	done

# With the values they are installed with, which their schemas check.
LINT_IMAGE := ghcr.io/kubehenge/kubehenge@sha256:0000000000000000000000000000000000000000000000000000000000000000
.PHONY: lint-helm
lint-helm:
	helm lint --strict charts/kubehenge --values pkg/provisioner/testdata/integration-values.yaml
	helm lint --strict management-cluster/charts/cluster-api-providers --set image=$(LINT_IMAGE)

.PHONY: lint-shell
lint-shell:
	shellcheck -x -P SCRIPTDIR -o all -S style $(SHELL_SCRIPTS)
	shfmt $(SHFMT_FLAGS) -d $(SHELL_SCRIPTS)
	@# A checkout on a file system without modes records none, and the workflows run scripts directly.
	@git ls-files -s '*.sh' | awk '$$1 != "100755" { print $$4 ": not executable in git; git update-index --chmod=+x " $$4; bad = 1 } END { exit bad }'

# zizmor runs its online audits too when GH_TOKEN is set; ZIZMOR_FLAGS=--format=github has CI
# annotate the lines it finds.
ZIZMOR_FLAGS ?=
.PHONY: lint-actions
lint-actions:
	actionlint .github/workflows/*.yml
	$(ZIZMOR) --persona=auditor $(ZIZMOR_FLAGS) .github/workflows/

# What can change without a commit: advisories published for the Go modules and the actions. The
# release runs this alone, as the pull request already ran every other check on the same tree.
.PHONY: advisories
advisories: web gateway-api
	$(GOVULNCHECK) ./...
	$(ZIZMOR) --persona=auditor $(ZIZMOR_FLAGS) .github/workflows/

.PHONY: lint-yaml
lint-yaml:
	$(YAMLLINT) --strict $$($(FILES) '*.yml' '*.yaml' ':!:*/templates/*')

.PHONY: lint-markdown
lint-markdown: web/node_modules/.package-lock.json
	$(MARKDOWNLINT) $$($(FILES) '*.md')

# The fragments of docs/changes/ and the sections of docs/changelog.md (.github/CONTRIBUTING.md).
.PHONY: lint-changelog
lint-changelog:
	sh scripts/changelog.sh check

.PHONY: lint-docker
lint-docker:
	hadolint --failure-threshold style test/e2e/pve/Dockerfile pkg/templates/el/*Containerfile

# Unit tier: the Go packages, under the race detector, the frontend, each Terraform/OpenTofu
# configuration against mocked providers, and the shell scripts' own tests.
.PHONY: unit
unit: unit-go unit-web unit-tofu unit-shell

# KUBEHENGE_REQUIRE_ALLOY turns a missing alloy (scripts/tools.sh) from a skip into a failure, so the
# generated Alloy configurations are always validated here.
.PHONY: unit-go
unit-go: web gateway-api capmox-crds
	KUBEHENGE_REQUIRE_ALLOY=1 go test -race ./...

.PHONY: unit-web
unit-web: web/node_modules/.package-lock.json
	cd web && npm test

# tofu test starts each of the three providers' plugins about 140 times, one process per run block
# phase, and Go's plugin client gives each a minute to answer. Beside the race tests of every package
# the starts queue for a core, so in a whole run it waits for them.
ifneq ($(filter test unit,$(MAKECMDGOALS)),)
unit-tofu: | unit-go
endif

.PHONY: unit-tofu
unit-tofu: tofu-init capi-operator-chart
	set -e; for m in $(MODULES); do $(TOFU) -chdir=$$m test; done

UNIT_SHELL := $(patsubst test/unit/%.sh,unit-shell-%,$(wildcard test/unit/*.sh))
.PHONY: unit-shell
unit-shell: $(UNIT_SHELL)

.PHONY: $(UNIT_SHELL)
$(UNIT_SHELL): unit-shell-%: test/unit/%.sh
	sh $<

.PHONY: test
test: lint unit

# Integration tier: the chart and the controllers against a real API server, envtest's kube-apiserver
# and etcd, with the CRDs of the releases the management cluster installs; no Proxmox VE, no
# containers. Its assets are fetched once, into Go's own directory.
INTEGRATION_ASSETS ?= $(subst \,/,$(shell go env GOPATH))/kubehenge-integration
.PHONY: integration
integration: web gateway-api
	test/integration/assets.sh $(INTEGRATION_ASSETS)
	KUBEBUILDER_ASSETS=$(INTEGRATION_ASSETS)/bin KUBEHENGE_INTEGRATION_CRDS=$(INTEGRATION_ASSETS)/crds \
		go test -tags integration -count=1 -run '^TestIntegration' ./pkg/...

# End-to-end tier: needs a Proxmox VE host. DIR is a root configuration applying management-cluster/.
.PHONY: e2e-management-cluster
e2e-management-cluster: capi-operator-chart providers
	TOFU=$(TOFU) test/e2e/management-cluster.sh $(DIR)

.PHONY: e2e-management-cluster-upgrade
e2e-management-cluster-upgrade: capi-operator-chart providers
	TOFU=$(TOFU) test/e2e/management-cluster-upgrade.sh $(DIR)

# Against the management cluster in KUBECONFIG; settings in test/e2e/workload-cluster.sh.
.PHONY: e2e-workload-cluster
e2e-workload-cluster:
	test/e2e/workload-cluster.sh $(CLASS) $(VERSION) $(UPGRADE)

# Against the management cluster in KUBECONFIG; settings in test/e2e/rancher.sh.
.PHONY: e2e-rancher
e2e-rancher:
	test/e2e/rancher.sh

# Against the management cluster in KUBECONFIG; settings in test/e2e/cluster-mesh.sh.
.PHONY: e2e-cluster-mesh
e2e-cluster-mesh:
	test/e2e/cluster-mesh.sh

# Against the management cluster in KUBECONFIG, its template refresh on with no soak; settings in
# test/e2e/node-image-refresh.sh.
.PHONY: e2e-node-image-refresh
e2e-node-image-refresh:
	test/e2e/node-image-refresh.sh

# Against the management cluster in KUBECONFIG; settings in test/e2e/autoscaler.sh.
.PHONY: e2e-autoscaler
e2e-autoscaler:
	test/e2e/autoscaler.sh

# Against the management cluster in KUBECONFIG; settings in test/e2e/control-plane-ha.sh.
.PHONY: e2e-control-plane-ha
e2e-control-plane-ha:
	test/e2e/control-plane-ha.sh

# Against the management cluster in KUBECONFIG on a Proxmox VE cluster of two nodes; settings in
# test/e2e/two-nodes.sh.
.PHONY: e2e-two-nodes
e2e-two-nodes:
	test/e2e/two-nodes.sh

# Settings come from KUBEHENGE_PROXMOX_* and KUBEHENGE_E2E_*; see pkg/templates/e2e_test.go.
.PHONY: e2e-templates
e2e-templates:
	go test -tags e2e -timeout 3h -count=1 -v -run TestTemplatesEndToEnd ./pkg/templates/

# Against the management cluster in KUBECONFIG (an admin kubeconfig) at KUBEHENGE_E2E_API_ENDPOINT.
.PHONY: e2e-interface
e2e-interface:
	go test -tags e2e -timeout 10m -count=1 -v -run TestInterfaceKubeconfigEndToEnd ./pkg/appliance/

# As root on a Proxmox VE node, with three releases from `make dist` built with RELEASE_PUBLIC_KEYS
# of a key made for the test; settings in test/e2e/appliance.sh.
.PHONY: e2e-appliance
e2e-appliance:
	test/e2e/appliance.sh $(RELEASE) $(NEWER) $(NEWEST) $(SETTINGS)

# As root on a Proxmox VE node, with the latest published release's install.sh and archive, and a
# `make dist` archive of a higher VERSION; settings in test/e2e/appliance-upgrade.sh.
.PHONY: e2e-appliance-upgrade
e2e-appliance-upgrade:
	test/e2e/appliance-upgrade.sh $(PUBLISHED_INSTALL) $(PUBLISHED) $(RELEASE) $(SETTINGS)

# Settings come from KUBEHENGE_PROXMOX_* and KUBEHENGE_E2E_*; see pkg/provisioner/e2e_test.go.
.PHONY: e2e-volumes
e2e-volumes:
	go test -tags e2e -timeout 30m -count=1 -v -run TestVolumesEndToEnd ./pkg/provisioner/

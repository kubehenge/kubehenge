TOFU ?= tofu
MODULES := management-cluster
# Where `make image` pushes the controller's image, `make capmox-image` capmox's and
# `make helm-addon-image` the Add-on Provider for Helm's; KO_FLAGS such as --tags or
# --insecure-registry.
IMAGE ?= ghcr.io/kubehenge/kubehenge
CAPMOX_IMAGE ?= ghcr.io/kubehenge/cluster-api-provider-proxmox
HELM_ADDON_IMAGE ?= ghcr.io/kubehenge/cluster-api-addon-provider-helm
KO_FLAGS ?=

.PHONY: build web gateway-api website dist index image capmox-image helm-addon-image lint unit test integration e2e-management-cluster e2e-management-cluster-upgrade e2e-workload-cluster e2e-rancher e2e-cluster-mesh e2e-templates e2e-volumes e2e-interface e2e-appliance
.PHONY: fmt lint-go lint-web lint-tofu lint-helm lint-shell lint-actions lint-yaml lint-markdown lint-docker unit-go unit-web unit-tofu tofu-init
# Independent steps, such as the lint and unit steps of `make test`, run side by side, one per CPU;
# each step's output is printed together once it ends, unless OUTPUT_SYNC=none asks for it as it
# comes, as the end-to-end runs do.
OUTPUT_SYNC ?= target
MAKEFLAGS += --jobs=$(shell getconf _NPROCESSORS_ONLN) --output-sync=$(OUTPUT_SYNC)
# The symbol table and DWARF go: a third of the binary, and stack traces keep their names without them.
LDFLAGS := -s -w -buildid=

build: web gateway-api
	CGO_ENABLED=0 go build -trimpath -ldflags='$(LDFLAGS)' -o dist/kubehenge ./cmd/kubehenge

# The Gateway API's standard channel, which the controller embeds and applies to every workload
# cluster: upstream's install file for one release, downloaded again only when the release changes
# and refused unless it matches its SHA-256. Every Go step waits for it, as for the frontend; .keep
# is what lets Go compile pkg/provisioner before it is there.
# renovate: datasource=github-release-attachments depName=kubernetes-sigs/gateway-api
GATEWAY_API_TAG := v1.6.3
GATEWAY_API_SHA256 := 356c2b8e286a964dc20995435659ad1095386329974793af10bbfe3eff977fe5
GATEWAY_API := pkg/provisioner/gatewayapi/standard-install-$(GATEWAY_API_TAG).yaml
$(GATEWAY_API):
	curl -fsSL --proto '=https' --retry 5 --retry-all-errors -o $@.part \
		https://github.com/kubernetes-sigs/gateway-api/releases/download/$(GATEWAY_API_TAG)/standard-install.yaml
	echo '$(GATEWAY_API_SHA256)  $@.part' | sha256sum -c --quiet - || { rm -f $@.part; exit 1; }
	rm -f pkg/provisioner/gatewayapi/standard-install-*.yaml
	mv $@.part $@

gateway-api: $(GATEWAY_API)

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

web: pkg/ui/dist/index.html

# The website (kubehenge.dev): docs/ with a home page, built into website/.vitepress/dist.
website:
	cd website && npm ci --no-audit --no-fund && npm run build

# The appliance's release archive, which appliance/install.sh installs: the binary, the
# management-cluster module with the charts it installs, the shared Headlamp (its server and
# frontend, taken from its Linux release) and the service units.
# The release key the appliance verifies upgrades with is pkg/release/release.pub unless RELEASE_KEY
# names another (base64 Ed25519 public key).
VERSION ?= 0.1.0
RELEASE_KEY ?= $(shell cat pkg/release/release.pub 2>/dev/null)
RELEASE_LDFLAGS = $(LDFLAGS) -X github.com/kubehenge/kubehenge/pkg/release.Version=$(VERSION) -X github.com/kubehenge/kubehenge/pkg/release.publicKey=$(RELEASE_KEY)
# The controller image the release's chart runs, by digest (repository@sha256:...); unset, the chart
# names none, and the module needs controller_image.
CONTROLLER_IMAGE ?=
# renovate: datasource=github-release-attachments depName=kubernetes-sigs/headlamp
HEADLAMP_TAG := v0.46.0
HEADLAMP_SHA256 := 1747c5de63963be925ded64dc2a5308144d260e43452855169bc771f03846434
HEADLAMP_VERSION := $(HEADLAMP_TAG:v%=%)
HEADLAMP_TARBALL := dist/cache/Headlamp-$(HEADLAMP_VERSION)-linux-x64.tar.gz
UNITS := kubehenge.service kubehenge-update.service kubehenge-update.path kubehenge-headlamp.service kubehenge-headlamp.socket kubehenge-headlamp-proxy.service
dist: web gateway-api
	rm -rf dist/stage && mkdir -p dist/stage/bin dist/stage/share/kubehenge/appliance dist/stage/share/kubehenge/headlamp/plugins dist/cache
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='$(RELEASE_LDFLAGS)' \
		-o dist/stage/bin/kubehenge ./cmd/kubehenge
	tar -cf - --exclude=.terraform --exclude=tests --exclude=.tflint.hcl --exclude='*.tfstate*' --exclude='*.go' management-cluster charts | tar -xf - -C dist/stage/share/kubehenge
	if [ -n '$(CONTROLLER_IMAGE)' ]; then \
		sed -i 's|^image: ""$$|image: "$(CONTROLLER_IMAGE)"|' dist/stage/share/kubehenge/charts/kubehenge/values.yaml && \
		grep -qxF 'image: "$(CONTROLLER_IMAGE)"' dist/stage/share/kubehenge/charts/kubehenge/values.yaml; \
	fi
	test -f $(HEADLAMP_TARBALL) || curl -fsSL --proto '=https' --retry 5 --retry-all-errors -o $(HEADLAMP_TARBALL) \
		https://github.com/kubernetes-sigs/headlamp/releases/download/$(HEADLAMP_TAG)/Headlamp-$(HEADLAMP_VERSION)-linux-x64.tar.gz
	echo '$(HEADLAMP_SHA256)  $(HEADLAMP_TARBALL)' | sha256sum -c --quiet - || { rm -f $(HEADLAMP_TARBALL); exit 1; }
	tar -xzf $(HEADLAMP_TARBALL) -C dist/stage/share/kubehenge/headlamp --strip-components=2 \
		$(foreach f,headlamp-server frontend LICENSE NOTICE,Headlamp-$(HEADLAMP_VERSION)-linux-x64/resources/$(f))
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

# The release index to sign: the previous release's (PREVIOUS, its .sig beside it), checked against
# the release key, with this release added. Run after dist.
TOFU_VERSION = $(patsubst v%,%,$(shell sed -n 's/^TOFU_TAG=//p' appliance/install.sh))
TOFU_SHA256 = $(shell sed -n 's/^TOFU_SHA256=//p' appliance/install.sh)
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
image: web gateway-api
	KO_DOCKER_REPO=$(IMAGE) KO_CONFIG_PATH=cmd/kubehenge ko build --bare --sbom=none $(KO_FLAGS) ./cmd/kubehenge

# Build and push the provider images the management cluster runs as Kubehenge builds them
# (capmox_build and helm_addon_build in management-cluster/versions.tf), check their digests and
# print their references by digest.
capmox-image:
	sh scripts/provider-image.sh capmox $(CAPMOX_IMAGE) $(KO_FLAGS)

helm-addon-image:
	sh scripts/provider-image.sh helm-addon $(HELM_ADDON_IMAGE) $(KO_FLAGS)

# Lint and static tier: Go, the frontend, Terraform/OpenTofu, Helm charts, shell, the workflows,
# YAML, Markdown and the e2e image's Dockerfile, each at its strictest settings; the files are the
# repository's own, as git lists them.
lint: lint-go lint-web lint-tofu lint-helm lint-shell lint-actions lint-yaml lint-markdown lint-docker

FILES = git ls-files --cached --others --exclude-standard
SHELL_SCRIPTS := test/e2e/*.sh test/e2e/ci/*.sh test/integration/*.sh appliance/*.sh appliance/pve-entry/*.sh scripts/*.sh pkg/provisioner/*.sh pkg/templates/*.sh
SHFMT_FLAGS := -i 4 -ci -sr
# Installed from lock files, which check every package's hash, their dependencies' included.
YAMLLINT := uv run --locked --project scripts/lint yamllint
ZIZMOR := uv run --locked --project scripts/lint zizmor
MARKDOWNLINT := npm --prefix web exec --no -- markdownlint-cli2
GOVULNCHECK := go tool govulncheck

# Rewrites what the format checks below would refuse.
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
lint-go: web gateway-api
	go mod tidy -diff
	golangci-lint config verify
	CGO_ENABLED=0 GOOS=linux golangci-lint run ./...
	CGO_ENABLED=0 GOOS=windows golangci-lint run ./...
	$(GOVULNCHECK) ./...

# oxfmt's check, type-aware oxlint and TypeScript's check; the build leaves the types to this step.
# The Proxmox VE entry's script is plain JavaScript, checked with the interface's tools, and so is
# the formatting of the website's own code.
WEBSITE_CODE := website/prepare.js website/.vitepress/config.mts website/.vitepress/theme
lint-web: web/node_modules/.package-lock.json
	cd web && npm run lint
	npm --prefix web exec --no -- oxfmt --check -c web/.oxfmtrc.json appliance/pve-entry $(WEBSITE_CODE)
	npm --prefix web exec --no -- oxlint -c appliance/pve-entry/.oxlintrc.json --deny-warnings --report-unused-disable-directives appliance/pve-entry

tofu-init:
	set -e; for m in $(MODULES); do $(TOFU) -chdir=$$m init -backend=false -input=false -lockfile=readonly >/dev/null; done

lint-tofu: tofu-init
	$(TOFU) fmt -check -recursive -diff
	set -e; for m in $(MODULES); do \
		$(TOFU) -chdir=$$m validate; \
		tflint --chdir=$$m --recursive --config=$(CURDIR)/management-cluster/.tflint.hcl; \
	done

# With the values they are installed with, which their schemas check.
LINT_IMAGE := ghcr.io/kubehenge/kubehenge@sha256:0000000000000000000000000000000000000000000000000000000000000000
lint-helm:
	helm lint --strict charts/kubehenge --values pkg/provisioner/testdata/integration-values.yaml
	helm lint --strict management-cluster/charts/cluster-api-providers --set image=$(LINT_IMAGE)

lint-shell:
	shellcheck -x -P SCRIPTDIR -o all -S style $(SHELL_SCRIPTS)
	shfmt $(SHFMT_FLAGS) -d $(SHELL_SCRIPTS)
	@# A checkout on a file system without modes records none, and the workflows run scripts directly.
	@git ls-files -s '*.sh' | awk '$$1 != "100755" { print $$4 ": not executable in git; git update-index --chmod=+x " $$4; bad = 1 } END { exit bad }'

# zizmor runs its online audits too when GH_TOKEN is set; ZIZMOR_FLAGS=--format=github has CI
# annotate the lines it finds.
ZIZMOR_FLAGS ?=
lint-actions:
	actionlint .github/workflows/*.yml
	$(ZIZMOR) --persona=auditor $(ZIZMOR_FLAGS) .github/workflows/

lint-yaml:
	$(YAMLLINT) --strict $$($(FILES) '*.yml' '*.yaml' ':!:*/templates/*')

lint-markdown: web/node_modules/.package-lock.json
	$(MARKDOWNLINT) $$($(FILES) '*.md')

lint-docker:
	hadolint --failure-threshold style test/e2e/pve/Dockerfile

# Unit tier: the Go packages, under the race detector, the frontend, and each Terraform/OpenTofu
# configuration against mocked providers.
unit: unit-go unit-web unit-tofu

unit-go: web gateway-api
	go test -race ./...

unit-web: web/node_modules/.package-lock.json
	cd web && npm test

unit-tofu: tofu-init
	set -e; for m in $(MODULES); do $(TOFU) -chdir=$$m test; done

test: lint unit

# Integration tier: the chart and the controllers against a real API server, envtest's kube-apiserver
# and etcd, with the CRDs of the releases the management cluster installs; no Proxmox VE, no
# containers. Its assets are fetched once, into Go's own directory.
INTEGRATION_ASSETS ?= $(subst \,/,$(shell go env GOPATH))/kubehenge-integration
integration: web gateway-api
	test/integration/assets.sh $(INTEGRATION_ASSETS)
	KUBEBUILDER_ASSETS=$(INTEGRATION_ASSETS)/bin KUBEHENGE_INTEGRATION_CRDS=$(INTEGRATION_ASSETS)/crds \
		go test -tags integration -count=1 -run '^TestIntegration' ./pkg/...

# End-to-end tier: needs a Proxmox VE host. DIR is a root configuration applying management-cluster/.
e2e-management-cluster:
	TOFU=$(TOFU) test/e2e/management-cluster.sh $(DIR)

e2e-management-cluster-upgrade:
	TOFU=$(TOFU) test/e2e/management-cluster-upgrade.sh $(DIR)

# Against the management cluster in KUBECONFIG; settings in test/e2e/workload-cluster.sh.
e2e-workload-cluster:
	test/e2e/workload-cluster.sh $(CLASS) $(VERSION) $(UPGRADE)

# Against the management cluster in KUBECONFIG; settings in test/e2e/rancher.sh.
e2e-rancher:
	test/e2e/rancher.sh

# Against the management cluster in KUBECONFIG; settings in test/e2e/cluster-mesh.sh.
e2e-cluster-mesh:
	test/e2e/cluster-mesh.sh

# Settings come from KUBEHENGE_PROXMOX_* and KUBEHENGE_E2E_*; see pkg/templates/e2e_test.go.
e2e-templates:
	go test -tags e2e -timeout 2h -count=1 -v -run TestTemplatesEndToEnd ./pkg/templates/

# Against the management cluster in KUBECONFIG (an admin kubeconfig) at KUBEHENGE_E2E_API_ENDPOINT.
e2e-interface:
	go test -tags e2e -timeout 10m -count=1 -v -run TestInterfaceKubeconfigEndToEnd ./pkg/appliance/

# As root on a Proxmox VE node, with two releases from `make dist`; settings in test/e2e/appliance.sh.
e2e-appliance:
	test/e2e/appliance.sh $(RELEASE) $(NEWER) $(SETTINGS)

# Settings come from KUBEHENGE_PROXMOX_* and KUBEHENGE_E2E_*; see pkg/provisioner/e2e_test.go.
e2e-volumes:
	go test -tags e2e -timeout 30m -count=1 -v -run TestVolumesEndToEnd ./pkg/provisioner/

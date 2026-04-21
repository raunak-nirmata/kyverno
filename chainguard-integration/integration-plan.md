# Chainguard Integration Plan — Enterprise Kyverno (N4K)

> **Author:** Jigar Joshi  
> **Date:** 2026-04-21  
> **Mode:** Shared Binary (precompiled binaries attached to GitHub Releases)  
> **Partner Repo:** https://github.com/chainguard-partners/nirmata

---

## 1. Background and Goals

Nirmata is integrating with the [Chainguard Partners Program](https://www.chainguard.dev/partners) to distribute hardened, CVE-minimal container images for Enterprise Kyverno (N4K). Chainguard's Factory will consume precompiled binaries from `chainguard-partners/nirmata` and package them using their secure, minimal build pipeline.

**Goals:**
- Provide Chainguard with cross-compiled N4K binaries via GitHub Releases on the partner repo
- Keep the integration automated — every N4K release automatically triggers a Chainguard-ready release
- Support all active release lines (1.13–1.17)
- Provide integration tests and a Helm chart reference so Chainguard can validate packaged images end-to-end
- Generate SBOMs per binary for supply chain compliance

---

## 2. Understanding the Current Release Flow

### 2.1 Enterprise Kyverno Components

N4K ships **7 binaries**, each independently containerized:

| Component | Source Dir | Binary | Image Name |
|---|---|---|---|
| Admission Controller | `cmd/kyverno` | `kyverno` | `kyverno` / `kyverno-fips` |
| Init Container | `cmd/kyverno-init` | `kyvernopre` | `kyvernopre` / `kyvernopre-fips` |
| Background Controller | `cmd/background-controller` | `background-controller` | `background-controller` |
| Cleanup Controller | `cmd/cleanup-controller` | `cleanup-controller` | `cleanup-controller` |
| Reports Controller | `cmd/reports-controller` | `reports-controller` | `reports-controller` |
| CLI | `cmd/cli/kubectl-kyverno` | `kubectl-kyverno` | `kyverno-cli` |
| Readiness Checker | `cmd/readiness-checker` | `readiness-checker` | `readiness-checker` |

### 2.2 Current Tag and Release Naming

Tags follow: `v{MAJOR}.{MINOR}.{PATCH}-n4k.nirmata.{N}`

Examples from active release branches:
- `v1.17.0-n4k.nirmata.1`, `v1.17.0-n4k.nirmata.2`, `v1.17.0-n4k.nirmata.3`
- `v1.16.3-n4k.nirmata.7`, `v1.16.3-n4k.nirmata.8`

Release types:
- **GA** — `v1.17.0-n4k.nirmata.1` (no suffix)
- **RC** — `v1.17.0-rc.1`, `v1.17.0-rc.2`
- **Dirty** — used only on non-main/release branches for testing

### 2.3 Current Release Automation

| Workflow | Trigger | What it does |
|---|---|---|
| `manual-tag-releaser.yaml` | Manual (workflow_dispatch) | Determines next tag, creates and pushes the git tag |
| `release.yaml` | `push: tags: v*` | Builds all FIPS and non-FIPS images via `ko`, publishes to GHCR, runs goreleaser, signs with Cosign |
| `images-publish.yaml` | Push to `main`/`release-*` | Publishes dev images on every commit |
| `charts-auto-sync.yaml` | Push to `charts/**` on `release-*-n4k` | Syncs Helm charts to `nirmata/kyverno-charts` repo |

### 2.4 Helm Chart Release Flow

Helm charts live in `enterprise-kyverno/charts/kyverno` and are released via a **separate repository** (`nirmata/kyverno-charts`). The `charts-auto-sync.yaml` workflow:
1. Detects the latest `release-*-n4k` branch
2. If it is the latest version → syncs to `kyverno-charts/main`
3. Otherwise → syncs to a matching branch in `kyverno-charts`

Chart versions follow: `3.7.2` (independent from the image/binary version).

---

## 3. Chainguard Integration Architecture

### 3.1 Mode: Shared Binary

Nirmata will provide **precompiled Linux binaries** (not source) to the Chainguard Factory via GitHub Releases on `chainguard-partners/nirmata`. Chainguard wraps them into hardened apk packages and minimal container images.

We do **not** commit binaries to the git tree. We do **not** use Git LFS. Binaries are attached to GitHub Releases only.

### 3.2 Monorepo Structure for `chainguard-partners/nirmata`

The `chainguard-partners/nirmata` repository is a **monorepo for all Nirmata products** — not just Enterprise Kyverno. Each product lives in its own top-level directory. This keeps the repo extensible as other Nirmata products are onboarded to Chainguard in the future.

```
chainguard-partners/nirmata/
├── README.md                         # Repo overview, lists all products
├── enterprise-kyverno/               # Everything for Enterprise Kyverno (N4K)
│   ├── Makefile                      # Runs all N4K component tests
│   ├── README.md                     # N4K-specific docs, Helm chart reference
│   ├── helm-values/
│   │   └── chainguard-test-values.yaml
│   ├── kyverno/                      # Admission controller
│   │   ├── Makefile
│   │   ├── sbom.json                 # Generated at release time, not committed
│   │   └── tests/
│   │       └── integration-test.sh
│   ├── kyvernopre/                   # Init container
│   │   ├── Makefile
│   │   ├── sbom.json
│   │   └── tests/
│   │       └── integration-test.sh
│   ├── background-controller/
│   │   ├── Makefile
│   │   ├── sbom.json
│   │   └── tests/
│   │       └── integration-test.sh
│   ├── cleanup-controller/
│   │   ├── Makefile
│   │   ├── sbom.json
│   │   └── tests/
│   │       └── integration-test.sh
│   ├── reports-controller/
│   │   ├── Makefile
│   │   ├── sbom.json
│   │   └── tests/
│   │       └── integration-test.sh
│   ├── kubectl-kyverno/              # CLI
│   │   ├── Makefile
│   │   ├── sbom.json
│   │   └── tests/
│   │       └── integration-test.sh
│   └── readiness-checker/
│       ├── Makefile
│       ├── sbom.json
│       └── tests/
│           └── integration-test.sh
└── <future-product>/                 # Placeholder for other Nirmata products
    └── ...
```

> **Why this structure:** Other Nirmata products (e.g., a future product X) will get their own top-level sibling directory. Each product is independently released with its own prefixed tags, keeping release cadences decoupled.

Since all N4K components ship together at the same version, we use a **single prefixed tag per release** (not per-component tags) because:

- All N4K components are part of one cohesive product release
- They must be deployed together and share the same version for compatibility
- A single GitHub Release with all binaries attached is the cleanest signal for the Chainguard Factory

Tag format on the partner repo for N4K: `enterprise-kyverno-v{MAJOR}.{MINOR}.{PATCH}-n4k.nirmata.{N}`

Example: `enterprise-kyverno-v1.17.0-n4k.nirmata.3`

This follows the Chainguard monorepo prefix convention (like `selinux-python-3.10`), and keeps future Nirmata products' releases decoupled from each other.

### 3.3 Binary Naming Convention for Release Attachments

Each binary uploaded to the GitHub Release on `chainguard-partners/nirmata` must be named clearly. Binaries are scoped under the `enterprise-kyverno/` prefix to match the repo structure:

```
enterprise-kyverno_kyverno_linux_amd64
enterprise-kyverno_kyverno_linux_arm64
enterprise-kyverno_kyvernopre_linux_amd64
enterprise-kyverno_kyvernopre_linux_arm64
enterprise-kyverno_background-controller_linux_amd64
enterprise-kyverno_background-controller_linux_arm64
enterprise-kyverno_cleanup-controller_linux_amd64
enterprise-kyverno_cleanup-controller_linux_arm64
enterprise-kyverno_reports-controller_linux_amd64
enterprise-kyverno_reports-controller_linux_arm64
enterprise-kyverno_kubectl-kyverno_linux_amd64
enterprise-kyverno_kubectl-kyverno_linux_arm64
enterprise-kyverno_readiness-checker_linux_amd64
enterprise-kyverno_readiness-checker_linux_arm64
```

Per-binary SBOMs (same naming, `.sbom.json` suffix):
```
enterprise-kyverno_kyverno_linux_amd64.sbom.json
enterprise-kyverno_kyverno_linux_arm64.sbom.json
... (repeat for each binary)
```

> The `enterprise-kyverno_` prefix on binaries is intentional — if another Nirmata product is added to this monorepo later, their release binaries will use their own prefix (e.g., `product-x_component_linux_amd64`), keeping all assets unambiguous on the same GitHub Release tag.

---

## 4. What Needs to Be Built

### 4.1 New Makefile Targets (in `enterprise-kyverno`)

Add the following targets to the existing `Makefile`:

```makefile
##################################
# CHAINGUARD BINARY RELEASE
##################################

CHAINGUARD_OUT_DIR        ?= .chainguard-release
CHAINGUARD_ARCHS          := amd64 arm64
CHAINGUARD_OS             := linux
CHAINGUARD_PARTNER_REPO   := chainguard-partners/nirmata
# Tag on the partner repo: enterprise-kyverno-<n4k-version>
CHAINGUARD_TAG            := enterprise-kyverno-$(VERSION)
# Prefix applied to every binary asset name for clarity in the shared monorepo
CHAINGUARD_BINARY_PREFIX  := enterprise-kyverno

# Cross-compile all binaries for linux/amd64 and linux/arm64
.PHONY: chainguard-build-binaries
chainguard-build-binaries: ## Cross-compile all N4K binaries for Chainguard release
	@echo "Building binaries for Chainguard release ($(CHAINGUARD_TAG))..." >&2
	@mkdir -p $(CHAINGUARD_OUT_DIR)
	@for arch in $(CHAINGUARD_ARCHS); do \
		echo "Building for $(CHAINGUARD_OS)/$$arch..." >&2; \
		GOOS=$(CHAINGUARD_OS) GOARCH=$$arch CGO_ENABLED=0 \
			go build -o $(CHAINGUARD_OUT_DIR)/$(CHAINGUARD_BINARY_PREFIX)_kyverno_$(CHAINGUARD_OS)_$$arch \
			-ldflags=$(LD_FLAGS) ./$(KYVERNO_DIR); \
		GOOS=$(CHAINGUARD_OS) GOARCH=$$arch CGO_ENABLED=0 \
			go build -o $(CHAINGUARD_OUT_DIR)/$(CHAINGUARD_BINARY_PREFIX)_kyvernopre_$(CHAINGUARD_OS)_$$arch \
			-ldflags=$(LD_FLAGS) ./$(KYVERNOPRE_DIR); \
		GOOS=$(CHAINGUARD_OS) GOARCH=$$arch CGO_ENABLED=0 \
			go build -o $(CHAINGUARD_OUT_DIR)/$(CHAINGUARD_BINARY_PREFIX)_background-controller_$(CHAINGUARD_OS)_$$arch \
			-ldflags=$(LD_FLAGS) ./$(BACKGROUND_DIR); \
		GOOS=$(CHAINGUARD_OS) GOARCH=$$arch CGO_ENABLED=0 \
			go build -o $(CHAINGUARD_OUT_DIR)/$(CHAINGUARD_BINARY_PREFIX)_cleanup-controller_$(CHAINGUARD_OS)_$$arch \
			-ldflags=$(LD_FLAGS) ./$(CLEANUP_DIR); \
		GOOS=$(CHAINGUARD_OS) GOARCH=$$arch CGO_ENABLED=0 \
			go build -o $(CHAINGUARD_OUT_DIR)/$(CHAINGUARD_BINARY_PREFIX)_reports-controller_$(CHAINGUARD_OS)_$$arch \
			-ldflags=$(LD_FLAGS) ./$(REPORTS_DIR); \
		GOOS=$(CHAINGUARD_OS) GOARCH=$$arch CGO_ENABLED=0 \
			go build -o $(CHAINGUARD_OUT_DIR)/$(CHAINGUARD_BINARY_PREFIX)_kubectl-kyverno_$(CHAINGUARD_OS)_$$arch \
			-ldflags=$(LD_FLAGS) ./$(CLI_DIR); \
		GOOS=$(CHAINGUARD_OS) GOARCH=$$arch CGO_ENABLED=0 \
			go build -o $(CHAINGUARD_OUT_DIR)/$(CHAINGUARD_BINARY_PREFIX)_readiness-checker_$(CHAINGUARD_OS)_$$arch \
			-ldflags=$(LD_FLAGS) ./$(READINESS_DIR); \
	done
	@echo "Binaries written to $(CHAINGUARD_OUT_DIR)/" >&2

# Generate SBOMs for all built binaries using Syft
.PHONY: chainguard-generate-sbom
chainguard-generate-sbom: ## Generate SBOMs for Chainguard release binaries
	@echo "Generating SBOMs..." >&2
	@which syft > /dev/null 2>&1 || (echo "Error: syft not installed. Run: curl -sSfL https://raw.githubusercontent.com/anchore/syft/main/install.sh | sh -s -- -b /usr/local/bin" && exit 1)
	@for arch in $(CHAINGUARD_ARCHS); do \
		for bin in kyverno kyvernopre background-controller cleanup-controller reports-controller kubectl-kyverno readiness-checker; do \
			syft $(CHAINGUARD_OUT_DIR)/$(CHAINGUARD_BINARY_PREFIX)_$${bin}_$(CHAINGUARD_OS)_$$arch \
				-o spdx-json \
				> $(CHAINGUARD_OUT_DIR)/$(CHAINGUARD_BINARY_PREFIX)_$${bin}_$(CHAINGUARD_OS)_$$arch.sbom.json; \
		done; \
	done
	@echo "SBOMs written to $(CHAINGUARD_OUT_DIR)/" >&2

# Create a draft GitHub Release on chainguard-partners/nirmata and upload binaries
.PHONY: chainguard-github-release
chainguard-github-release: ## Create draft GitHub Release on chainguard-partners/nirmata with binaries
	@echo "Creating draft release $(CHAINGUARD_TAG) on $(CHAINGUARD_PARTNER_REPO)..." >&2
	@which gh > /dev/null 2>&1 || (echo "Error: gh CLI not installed" && exit 1)
	@gh release create $(CHAINGUARD_TAG) \
		--repo $(CHAINGUARD_PARTNER_REPO) \
		--title "Enterprise Kyverno $(VERSION)" \
		--notes "Enterprise Kyverno N4K release $(VERSION). See https://github.com/nirmata/enterprise-kyverno for changelog." \
		--draft \
		$(CHAINGUARD_OUT_DIR)/*
	@echo "Draft release created. Review at: https://github.com/$(CHAINGUARD_PARTNER_REPO)/releases" >&2

# Publish the draft release (makes it visible to Chainguard Factory)
.PHONY: chainguard-publish-release
chainguard-publish-release: ## Publish draft release on chainguard-partners/nirmata (triggers Chainguard Factory)
	@echo "Publishing release $(CHAINGUARD_TAG) on $(CHAINGUARD_PARTNER_REPO)..." >&2
	@gh release edit $(CHAINGUARD_TAG) \
		--repo $(CHAINGUARD_PARTNER_REPO) \
		--draft=false
	@echo "Release published. Chainguard Factory will pick it up within 1-12 hours." >&2

# Full Chainguard release pipeline: build -> SBOM -> create release -> publish
.PHONY: chainguard-release
chainguard-release: ## Full Chainguard release pipeline (requires VERSION set and GH auth to partner repo)
chainguard-release: chainguard-build-binaries
chainguard-release: chainguard-generate-sbom
chainguard-release: chainguard-github-release
chainguard-release: chainguard-publish-release

.PHONY: chainguard-clean
chainguard-clean: ## Remove Chainguard build artifacts
	@rm -rf $(CHAINGUARD_OUT_DIR)
```

### 4.2 New GitHub Actions Workflow: `chainguard-release.yaml`

This workflow runs **after** the existing `release.yaml` completes successfully and triggers a Chainguard release automatically.

**File:** `.github/workflows/chainguard-release.yaml`

```yaml
name: Chainguard Release

permissions:
  contents: read

on:
  # Trigger after a successful image release on N4K tags
  push:
    tags:
      - 'v*-n4k.nirmata.*'

jobs:
  build-and-release:
    runs-on: xr1
    permissions:
      contents: read
    steps:
      - name: Checkout
        uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd
        with:
          fetch-tags: true

      - name: Setup Go
        uses: ./.github/actions/go/setup

      - name: Install Syft
        run: |
          curl -sSfL https://raw.githubusercontent.com/anchore/syft/main/install.sh \
            | sh -s -- -b /usr/local/bin

      - name: Build cross-compiled binaries
        env:
          VERSION: ${{ github.ref_name }}
        run: make chainguard-build-binaries

      - name: Generate SBOMs
        run: make chainguard-generate-sbom

      - name: Create and publish GitHub Release on chainguard-partners/nirmata
        env:
          VERSION: ${{ github.ref_name }}
          GH_TOKEN: ${{ secrets.CHAINGUARD_PARTNER_REPO_PAT }}
        run: |
          make chainguard-github-release
          make chainguard-publish-release
```

**Authentication — see Section 9.2 for the full PAT → GitHub App migration plan.**

### 4.3 Tests for the Partner Repository

Each component in `chainguard-partners/nirmata/<component>/tests/integration-test.sh` should:

1. Verify the binary exists and is executable
2. Run `--version` or equivalent to confirm it starts correctly
3. For the admission controller and controllers, validate basic Kubernetes connectivity smoke test (optional, can use a local kind cluster if Chainguard harness supports it)

**Minimal integration test** (`kyverno/tests/integration-test.sh`):

```bash
#!/usr/bin/env bash
set -euo pipefail

BINARY="${BINARY_PATH:-./kyverno}"

echo "==> Checking binary exists and is executable"
test -x "$BINARY" || (echo "Binary not found or not executable: $BINARY" && exit 1)

echo "==> Checking binary version output"
VERSION_OUTPUT="$($BINARY version 2>&1 || true)"
echo "$VERSION_OUTPUT"

echo "==> Integration test passed"
```

**Top-level `Makefile`** in `chainguard-partners/nirmata/enterprise-kyverno/`:

```makefile
.PHONY: test
test: test-kyverno test-kyvernopre test-background-controller \
      test-cleanup-controller test-reports-controller \
      test-kubectl-kyverno test-readiness-checker

.PHONY: test-kyverno
test-kyverno:
	@cd kyverno && make test

.PHONY: test-kyvernopre
test-kyvernopre:
	@cd kyvernopre && make test

.PHONY: test-background-controller
test-background-controller:
	@cd background-controller && make test

.PHONY: test-cleanup-controller
test-cleanup-controller:
	@cd cleanup-controller && make test

.PHONY: test-reports-controller
test-reports-controller:
	@cd reports-controller && make test

.PHONY: test-kubectl-kyverno
test-kubectl-kyverno:
	@cd kubectl-kyverno && make test

.PHONY: test-readiness-checker
test-readiness-checker:
	@cd readiness-checker && make test
```

**Per-component `Makefile`** (e.g., `enterprise-kyverno/kyverno/Makefile`):

```makefile
.PHONY: test
test:
	./tests/integration-test.sh
```

> **Test harness dependencies to declare to Chainguard:** `bash`, `curl`, `jq` (for more advanced tests). Basic smoke tests only need the binary itself.

---

## 5. Release Branch Strategy and Version Mapping

### 5.1 Active Release Branches

| Branch | Kyverno Version | N4K Tag Pattern | Chainguard Status |
|---|---|---|---|
| `release-1.13-n4k` | 1.13.x | `v1.13.x-n4k.nirmata.N` | Add when needed |
| `release-1.14-n4k` | 1.14.x | `v1.14.x-n4k.nirmata.N` | Add when needed |
| `release-1.15-n4k` | 1.15.x | `v1.15.x-n4k.nirmata.N` | Add when needed |
| `release-1.16-n4k` | 1.16.x | `v1.16.x-n4k.nirmata.N` | **Start here (current stable)** |
| `release-1.17-n4k` | 1.17.x | `v1.17.x-n4k.nirmata.N` | **Immediate priority** |

**Recommendation:** Start with `release-1.17-n4k` (current latest) and backfill `release-1.16-n4k`. Older branches (1.13–1.15) only if there are active customer entitlements.

### 5.2 Version Tag on Chainguard Partner Repo

The tag pushed to `chainguard-partners/nirmata` uses the `enterprise-kyverno-` prefix per the Chainguard monorepo convention:

| enterprise-kyverno tag | chainguard-partners/nirmata tag |
|---|---|
| `v1.17.0-n4k.nirmata.3` | `enterprise-kyverno-v1.17.0-n4k.nirmata.3` |
| `v1.16.3-n4k.nirmata.8` | `enterprise-kyverno-v1.16.3-n4k.nirmata.8` |

The mapping is deterministic: strip the N4K tag from enterprise-kyverno, prepend `enterprise-kyverno-`. This makes it unambiguous which enterprise-kyverno commit produced which Chainguard image, and keeps future Nirmata products' tags decoupled.

### 5.3 RC vs GA Releases and Chainguard

- **RC tags** (`v1.17.0-rc.1`) — These are Nirmata-internal release candidates. **Do not push these to `chainguard-partners/nirmata`.** The Chainguard Factory only builds fully published releases.
- **GA tags** (`v1.17.0-n4k.nirmata.1`) — Push these to the partner repo as `enterprise-kyverno-v1.17.0-n4k.nirmata.1`.
- The `chainguard-release.yaml` workflow filters to `v*-n4k.nirmata.*` tags only, which naturally excludes RC and dirty tags.

---

## 6. Full End-to-End Release Flow (After Integration)

```
┌──────────────────────────────────────────────────────────────────────────┐
│  Step 1: Engineer triggers manual-tag-releaser.yaml                       │
│          → Release type: ga, Branch: release-1.17-n4k                    │
│          → Workflow determines next tag: v1.17.0-n4k.nirmata.4           │
│          → Pushes git tag to enterprise-kyverno                           │
└──────────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌──────────────────────────────────────────────────────────────────────────┐
│  Step 2: release.yaml triggers on v1.17.0-n4k.nirmata.4                  │
│          → Builds FIPS and non-FIPS container images                      │
│          → Publishes to ghcr.io/nirmata/*                                 │
│          → Runs goreleaser (creates GitHub Release on enterprise-kyverno) │
│          → Generates SLSA provenance, signs images with Cosign            │
│          → Uploads install.yaml and CRD manifests to the release          │
└──────────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌──────────────────────────────────────────────────────────────────────────┐
│  Step 3 (NEW): chainguard-release.yaml triggers on v*-n4k.nirmata.* tag  │
│          → Cross-compiles all 7 binaries for linux/amd64 + linux/arm64   │
│          → Generates SBOM per binary via Syft                             │
│          → Creates GitHub Release on chainguard-partners/nirmata          │
│          → Attaches all 14 binaries + 14 SBOMs                           │
│          → Publishes the release (not draft)                              │
└──────────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌──────────────────────────────────────────────────────────────────────────┐
│  Step 4 (Chainguard side, 1-12 hours later)                               │
│          → Chainguard Factory detects new release on partner repo         │
│          → Builds hardened apk packages from binaries                     │
│          → Runs integration tests (tests/ in partner repo)                │
│          → Publishes hardened images to Chainguard registry               │
│          → Available to entitled customers                                 │
└──────────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌──────────────────────────────────────────────────────────────────────────┐
│  Step 5 (Parallel, unchanged): charts-auto-sync.yaml                      │
│          → Detects chart changes on release-1.17-n4k                      │
│          → Syncs Helm chart to nirmata/kyverno-charts (main branch)       │
│          → kyverno-charts releases chart as 3.7.x independently          │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## 7. Helm Chart for Image Testing (Chainguard Validation)

Chainguard recommends providing a Helm chart or deployment config so they can validate the packaged image end-to-end. We should add to the partner repo a minimal `helm-values/` directory with a `values.yaml` override that points to the Chainguard-hosted image registry.

Example `helm-values/chainguard-test-values.yaml` (to live in partner repo):

```yaml
# Minimal values for Chainguard image validation
# Image refs will be substituted by Chainguard during testing
admissionController:
  container:
    image:
      registry: cgr.dev
      repository: chainguard/nirmata/kyverno
      tag: latest
  initContainer:
    image:
      registry: cgr.dev
      repository: chainguard/nirmata/kyvernopre
      tag: latest
backgroundController:
  image:
    registry: cgr.dev
    repository: chainguard/nirmata/background-controller
    tag: latest
cleanupController:
  image:
    registry: cgr.dev
    repository: chainguard/nirmata/cleanup-controller
    tag: latest
reportsController:
  image:
    registry: cgr.dev
    repository: chainguard/nirmata/reports-controller
    tag: latest
```

Reference the public kyverno-charts Helm repo in the partner repo README so Chainguard knows how to deploy:

```bash
helm upgrade --install kyverno \
  --repo https://nirmata.github.io/kyverno-charts/ kyverno \
  --namespace kyverno --create-namespace \
  -f helm-values/chainguard-test-values.yaml
```

---

## 8. Existing Tests and What to Reuse

Enterprise Kyverno has a rich test suite in `test/`:

| Test Type | Location | Chainguard Usability |
|---|---|---|
| CLI unit tests | `test/cli/` | Not applicable (no running cluster) |
| Chainsaw integration tests | `test/chainsaw/` | **Yes** — can run in kind in Chainguard harness |
| Conformance tests | `test/conformance/` | **Yes** — if Chainguard harness supports kind/K8s |
| Load tests | `test/load/` | No |
| Policy tests | `test/policy/` | Not directly |

**Recommendation:** Start with minimal binary smoke tests (version check, startup validation). In parallel, discuss with Chainguard whether their harness can run a kind cluster for full chainsaw-based integration tests. If yes, the existing chainsaw tests under `test/chainsaw/` can be reused with minimal adaptation.

### Smoke Test Suite (immediate)

For each binary, the test should:

1. Verify binary is executable
2. Run `<binary> version` or `<binary> --help` — confirm it exits 0
3. (Optional) Verify binary embeds expected version string matching the release tag

### Full Integration Test Suite (future)

Once Chainguard confirms cluster support:

1. Deploy N4K via Helm using `helm-values/chainguard-test-values.yaml`
2. Run a subset of chainsaw tests from `test/chainsaw/` — the `admission` and `mutate` suites are good candidates
3. Verify all pods reach `Ready` state

---

## 9. One-Time Setup Tasks

### 9.1 Repository Setup (`chainguard-partners/nirmata`)

- [ ] Accept GitHub invitation to `chainguard-partners/nirmata`
- [ ] Push initial structure: top-level `README.md` + `enterprise-kyverno/` directory with all 7 component subdirectories, Makefiles, and test scripts
- [ ] Add top-level `enterprise-kyverno/README.md` explaining N4K, its components, and how to deploy via Helm
- [ ] Add `enterprise-kyverno/helm-values/chainguard-test-values.yaml`
- [ ] Confirm with Chainguard contact which test tools are available in their harness (`bash`, `curl`, `jq`, `helm`, `kubectl`, `kind`)
- [ ] Confirm with Chainguard that the `enterprise-kyverno-v*` tag prefix format is acceptable for the Factory

### 9.2 Secrets and Credentials (`nirmata/enterprise-kyverno`)

We start with a **PAT** for simplicity but will migrate to a **GitHub App** for production. The GitHub App approach is preferred long-term because it avoids coupling credentials to an individual user account, supports fine-grained permissions, and is easier to rotate.

#### Phase A — PAT (immediate, to unblock initial integration)

1. Create a classic GitHub Personal Access Token (PAT) for a machine/bot account with:
   - `repo` scope (to create releases and push tags on `chainguard-partners/nirmata`)
2. Add as a repository secret in `nirmata/enterprise-kyverno`:
   - **Secret name:** `CHAINGUARD_PARTNER_REPO_PAT`
3. Reference in workflow:
   ```yaml
   env:
     GH_TOKEN: ${{ secrets.CHAINGUARD_PARTNER_REPO_PAT }}
   ```

#### Phase B — GitHub App (target state)

Create a dedicated GitHub App for Nirmata → Chainguard integration:

1. **Create the GitHub App** under the `nirmata` GitHub organization:
   - **Name:** `Nirmata Chainguard Releaser`
   - **Permissions needed:**
     - `Contents: Read & Write` (to create releases and push tags on the partner repo)
     - `Metadata: Read` (required baseline)
   - **Where it can be installed:** Only `chainguard-partners` organization

2. **Install the App** on the `chainguard-partners/nirmata` repository only.

3. **Store secrets** in `nirmata/enterprise-kyverno` repository settings:
   - `CHAINGUARD_RELEASER_APP_ID` — the App's numeric ID (found in App settings)
   - `CHAINGUARD_RELEASER_PRIVATE_KEY` — the App's PEM private key (generated in App settings)

4. **Update `chainguard-release.yaml`** to generate a short-lived token instead of using a static PAT:
   ```yaml
   - name: Generate GitHub App token
     id: app-token
     uses: actions/create-github-app-token@d72941d797fd3113feb6b93fd0dec494b13a2547 # v1
     with:
       app-id: ${{ secrets.CHAINGUARD_RELEASER_APP_ID }}
       private-key: ${{ secrets.CHAINGUARD_RELEASER_PRIVATE_KEY }}
       owner: chainguard-partners
       repositories: nirmata

   - name: Create and publish GitHub Release on chainguard-partners/nirmata
     env:
       VERSION: ${{ github.ref_name }}
       GH_TOKEN: ${{ steps.app-token.outputs.token }}
     run: |
       make chainguard-github-release
       make chainguard-publish-release
   ```

> **Note:** This pattern is already used in enterprise-kyverno for `manual-tag-releaser.yaml` (`actions/create-github-app-token` with `NIRMATA_INTERNAL_APP_ID` and `NIRMATA_INTERNAL_PRIVATE_KEY`). The Chainguard releaser app follows the exact same pattern.

**Secret names summary:**

| Phase | Secret Name | Description |
|---|---|---|
| A (PAT) | `CHAINGUARD_PARTNER_REPO_PAT` | Classic PAT with repo scope, temporary |
| B (App) | `CHAINGUARD_RELEASER_APP_ID` | GitHub App numeric ID |
| B (App) | `CHAINGUARD_RELEASER_PRIVATE_KEY` | GitHub App PEM private key |

### 9.3 Makefile Changes (`nirmata/enterprise-kyverno`)

- [ ] Add the `chainguard-build-binaries`, `chainguard-generate-sbom`, `chainguard-github-release`, `chainguard-publish-release`, `chainguard-release`, and `chainguard-clean` targets to `Makefile`
- [ ] Verify `syft` is available on the `xr1` runner (or add install step)

### 9.4 New Workflow (`nirmata/enterprise-kyverno`)

- [ ] Add `.github/workflows/chainguard-release.yaml`
- [ ] Test with a dirty tag first to validate the workflow end-to-end before a real GA release

### 9.5 Coordination with Chainguard

- [ ] Confirm binary naming convention is acceptable
- [ ] Confirm they will handle `linux/amd64` and `linux/arm64`; clarify if `linux/s390x` or `linux/ppc64le` are needed
- [ ] Confirm which test tools are available in their build/test harness
- [ ] Share the Helm chart reference for image-level validation
- [ ] Confirm the SBOM format they prefer (SPDX JSON vs CycloneDX)
- [ ] Ask about their expected image naming on `cgr.dev` (e.g., `cgr.dev/nirmata/kyverno`)

---

## 10. Phased Rollout

### Phase 1 — Foundation (Week 1-2)
- Set up `chainguard-partners/nirmata` repo structure
- Add Makefile targets to enterprise-kyverno
- Create `chainguard-release.yaml` workflow
- Test manually with a dirty/test release

### Phase 2 — First Real Release (Week 2-3)
- Cut `v1.17.0-n4k.nirmata.X` as first Chainguard-integrated GA release
- Verify Chainguard Factory picks up the release and builds images
- Validate smoke tests pass in Chainguard harness

### Phase 3 — Backfill and Stabilize (Week 3-4)
- Backfill `release-1.16-n4k` if needed
- Add more comprehensive integration tests (Helm deploy + chainsaw subset)
- Document the flow for the team

### Phase 4 — Full Automation Validation
- Verify every new N4K GA tag automatically produces a Chainguard release
- Add alerting/notification if the `chainguard-release.yaml` workflow fails
- Optionally: add a comment to the enterprise-kyverno GitHub Release linking to the corresponding Chainguard release

---

## 11. Key Constraints and Gotchas

1. **Chainguard Factory ignores Draft and Pre-release tags.** The workflow must call `--draft=false` before the release is finalized.
2. **No Git LFS.** All binaries must be attached to GitHub Releases, never committed to the tree.
3. **Tag must start with lowercase `v`.** `v1.17.0-n4k.nirmata.3` is valid. The existing N4K tag format already satisfies this.
4. **1-12 hour delay** after publishing the release before Chainguard images are available. Set expectations with customers accordingly.
5. **Helm charts are NOT provided to Chainguard for packaging** — Chainguard packages only the binaries/images. The Helm chart continues to be released via `nirmata/kyverno-charts` as today.
6. **FIPS builds** — Chainguard builds their own hardened base; they do not use our FIPS Dockerfiles. The shared binary mode provides standard (non-FIPS CGO) binaries. FIPS compliance for Chainguard images comes from Chainguard's own toolchain and base images. Confirm with Chainguard contact if a separate FIPS binary stream is needed.
7. **The `xr1` runner** is used for building. Ensure it has sufficient disk space for cross-compilation artifacts (all binaries for 2 architectures × 7 components ≈ ~500MB).

---

## 12. Files To Create or Modify

**In `nirmata/enterprise-kyverno`:**

| Action | File | Notes |
|---|---|---|
| Modify | `Makefile` | Add `chainguard-*` targets (Section 4.1) |
| Create | `.github/workflows/chainguard-release.yaml` | Automated release workflow (Section 4.2) |
| Modify | `chainguard-integration/partner-repository-document.md` | Properly formatted markdown (done) |
| Create | `chainguard-integration/integration-plan.md` | This document |

**In `chainguard-partners/nirmata` (partner repo):**

| Action | File | Notes |
|---|---|---|
| Create | `README.md` | Repo overview listing all Nirmata products |
| Create | `enterprise-kyverno/README.md` | N4K product docs, Helm chart reference |
| Create | `enterprise-kyverno/Makefile` | Top-level test runner for all N4K components |
| Create | `enterprise-kyverno/{component}/Makefile` | Per-component test runner (×7) |
| Create | `enterprise-kyverno/{component}/tests/integration-test.sh` | Binary smoke tests (×7) |
| Create | `enterprise-kyverno/helm-values/chainguard-test-values.yaml` | Helm values for image validation |

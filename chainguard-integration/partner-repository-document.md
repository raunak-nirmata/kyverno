# Chainguard Partners Repository Guide

> **Created by:** Trevor Dunlap  
> **Last Updated:** Feb 19, 2026  
> **Status:** DRAFT

**Source (Google Doc):** https://docs.google.com/document/d/1Lj1Xchcxf0xQ2o664iTm0jFNq2iP054eBpKk0h1KXb4/edit?tab=t.0

---

## Overview

Welcome to the [Chainguard Partners Program](https://www.chainguard.dev/partners). Through this program, Chainguard works directly with ISV partners to build, maintain, and distribute hardened container images for your commercial applications. You continue to own and license your software as you do today, while Chainguard packages it using the same secure, minimal build pipeline we use for our own products.

This guide explains:

- How to work with your repository
- How to structure your repository (i.e., providing either binaries/source and tests)
- How to release new versions

---

## Your Repository

Your source code lives in a private repository under the [chainguard-partners GitHub organization](https://github.com/chainguard-partners):

```
https://github.com/chainguard-partners/nirmata
```

You will receive a GitHub invitation to the `chainguard-partners/nirmata` repository as part of onboarding. Once you accept the invitation, you will have access to your repository. The repository is isolated from Chainguard's production org, and access is restricted to authorized team members on both sides.

### Cloning

```shell
git clone https://github.com/chainguard-partners/nirmata.git
cd nirmata
```

### Branching and Pull Requests

Typical workflow:

```shell
git checkout -b my-feature
# make your changes
git add .
git commit -m "Description of changes"
git push origin my-feature
```

Then open a pull request on GitHub targeting `main`.

---

## Structuring Your Repo

You are welcome to push source code, provide precompiled binaries, or a mix of both. If you have multiple independent software components, we recommend a monorepo structure:

```
nirmata/
├── hello-world/          # Source code example
│   ├── go.mod
│   ├── main.go
│   └── README.md
├── hello-universe/       # Precompiled binary example
│   ├── Makefile
│   ├── sbom.json
│   └── test/
│       └── test.sh
└── hello-planet/         # Another binary example
    ├── sbom.json
    ├── Makefile
    └── test/
        └── test.sh
```

> **Note:** Do not commit large precompiled binaries directly into the repository git tree. See the "Binaries" section below.

### Source Code

Add your application source code to the Git repository. Chainguard will build your application from source as part of the packaging process.

Include any standard build configuration files your application already uses, such as `go.mod`, `pom.xml`, `Makefile`, or `package.json`. Ideally these live at the root of the repository or are clearly documented in your README.

### Binaries and Large Files

If you are supplying precompiled binaries (especially large ones), **do not commit them directly to the git tree and do not use Git LFS**. Git LFS does not provide an easy API signal for our automated build systems to detect upgrades.

Instead, you will attach your compiled binaries directly to **GitHub Releases**.

- **Cross-compilation:** Ensure you have cross-compiled for the appropriate end architecture (e.g., `GOOS=linux GOARCH=amd64`).
- **Flexibility:** The repository codebase can act simply as a placeholder, or it can contain matching source code alongside the release.
- **Examples in the Wild:** The [Adoptium Temurin 17 binaries](https://github.com/adoptium/temurin17-binaries) repository is a great example of using release attachments. Chainguard uses this exact automated release method internally and publicly for tools like `melange`, `apko`, our SDK, and all our Terraform providers.

You can release binaries via the GitHub Web UI, use GitHub Actions, or use the `gh` CLI tool to script and upload multiple builds of any size.

### SBOM

We recommend providing an SBOM for the associated binaries. For example, users can create SBOMs with [Syft](https://github.com/anchore/syft).

### Tests

We recommend under a `tests/` directory, provide executable scripts along with a Makefile that validates the correctness of your application when packaged. These tests will be run automatically as part of our build pipeline to ensure the packaged binary works correctly.

```
nirmata/
├── foo
│   ├── Makefile
│   └── tests
│       └── integration-test.sh
├── Makefile
```

Example `Makefile`:

```makefile
.PHONY: test
test:
	./tests/integration-test.sh
```

If your tests require specific tools such as `curl` or `jq`, call that out so Chainguard can ensure they are available in the test harness.

### Image Testing (Recommended)

If possible, provide a Helm chart or equivalent deployment configuration that Chainguard can use to validate the built image in a running environment. This helps ensure the image works correctly end to end, beyond just binary-level tests.

---

## Releasing New Versions

Your repository is connected to the Chainguard Factory. We can rely on either [GitHub Releases](https://docs.github.com/en/repositories/releasing-projects-on-github/managing-releases-in-a-repository) or [Tags](https://docs.github.com/en/repositories/releasing-projects-on-github/about-releases) to track upgrades and autobuild your packages.

### How to Trigger a Release

1. **Create a Git tag** and push it to the repository.
2. **Create a GitHub Release** from that tag.
3. **Attach your binaries** (if applicable) to the release.
4. **Publish the Release.** > **Important:** The Chainguard Factory ignores releases marked as **Draft** or **Pre-release**. Only fully published releases serve as our cue to act. You can create a draft, upload as many binaries as you need, and then mark it as released to seal it off.

You can generally expect a **1-12 hour delay** between tag creation and the new image being available. This window accounts for automated security scanning, CVE analysis, and the build queue.

### Versioning Single Projects

Tags must start with a lowercase `v` followed by a full semantic version (`MAJOR.MINOR.PATCH`).

- **Correct:** `v1.0.0`, `v2.1.4`, `v1.0.1`
- **Incorrect:** `1.0.0` (missing `v`)

### Versioning Independent Software via Prefixes (Monorepo)

If you have independent software projects (e.g., `hello-world` and `hello-planet`) in the same repository, you should release them independently using **project-prefixed tags**.

We highly recommend modeling this after large-scale open-source projects like the [SELinux Project](https://github.com/SELinuxProject/selinux). SELinux successfully releases dozens of distinct components from a single repository by prefixing every tag with the component name (e.g., `selinux-python-3.10`, `policycoreutils-3.5`).

For your repository, tag `hello-world` releases as `hello-world-v5.0.0` and `hello-planet` releases as `hello-planet-v1.9.0`.

```bash
# Releasing an update specifically for hello-planet
git tag -a hello-planet-v5.0.0 -s -m "Release v5.0.0 of hello-planet"
git push origin hello-planet-v5.0.0
```

**Key Benefits of this Flexibility:**

- You can push tags for completely unrelated components on different release cycles.
- You can trigger a build for one binary without forcing a rebuild of the others.
- When creating the GitHub Release for `hello-planet-v5.0.0`, you only need to attach the binaries relevant to `hello-planet`.

### Helpful Release Resources

To learn more about managing your releases, refer to these official guides:

- [Managing releases in a repository](https://docs.github.com/en/repositories/releasing-projects-on-github/managing-releases-in-a-repository)
- [About GitHub Releases](https://docs.github.com/en/repositories/releasing-projects-on-github/about-releases)
- [Immutable releases and supply chain security](https://docs.github.com/en/repositories/releasing-projects-on-github/about-releases#about-release-assets)

---

## What Happens After You Release

After you push a release:

1. The Chainguard Factory detects the new tag/release and automatically updates the build configuration for your package.
2. The apk is built using Chainguard's secure build pipeline with the tagged version of your source code.
3. The image is published and made available to entitled customers.

---

## Support

If you have questions about your repository or the build process, contact your Chainguard partner representative.

# Nirmata — Chainguard Partner Repository

This repository is a monorepo for all Nirmata products distributed via the
[Chainguard Partners Program](https://www.chainguard.dev/partners).

## Products

| Directory | Product | Source |
|---|---|---|
| [`enterprise-kyverno/`](./enterprise-kyverno/) | Enterprise Kyverno (N4K) | https://github.com/nirmata/enterprise-kyverno |

Each product directory is self-contained: it holds per-component Makefiles,
integration tests, and Helm values for image validation.

## Releases

Releases are created automatically by the `chainguard-release.yaml` workflow in
`nirmata/enterprise-kyverno` whenever a GA tag (`v*-n4k.nirmata.*`) is pushed.

Release tags use a per-product prefix to keep independent release cadences:

| Product | Tag format | Example |
|---|---|---|
| Enterprise Kyverno | `enterprise-kyverno-v<n4k-version>` | `enterprise-kyverno-v1.17.0-n4k.nirmata.3` |

## Support

Contact your Nirmata partner representative or open an issue.

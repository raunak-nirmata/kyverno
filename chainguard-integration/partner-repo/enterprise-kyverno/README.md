# Enterprise Kyverno (N4K)

Enterprise Kyverno is Nirmata's hardened, enterprise-grade distribution of
[Kyverno](https://kyverno.io) — the Kubernetes-native policy engine.

Source repository: https://github.com/nirmata/enterprise-kyverno

## Components

N4K consists of 7 binaries deployed together as a Kubernetes application:

| Component | Binary | Description |
|---|---|---|
| `kyverno` | `enterprise-kyverno_kyverno_linux_<arch>` | Admission controller — core policy engine |
| `kyvernopre` | `enterprise-kyverno_kyvernopre_linux_<arch>` | Init container — CRD migration and pre-flight |
| `background-controller` | `enterprise-kyverno_background-controller_linux_<arch>` | Background policy reconciliation |
| `cleanup-controller` | `enterprise-kyverno_cleanup-controller_linux_<arch>` | TTL-based resource cleanup policies |
| `reports-controller` | `enterprise-kyverno_reports-controller_linux_<arch>` | Policy report generation |
| `kubectl-kyverno` | `enterprise-kyverno_kubectl-kyverno_linux_<arch>` | kubectl plugin / CLI |
| `readiness-checker` | `enterprise-kyverno_readiness-checker_linux_<arch>` | Readiness probe helper |

## Releases

Binaries for all components are attached to each GitHub Release tagged
`enterprise-kyverno-v<n4k-version>` (e.g. `enterprise-kyverno-v1.17.0-n4k.nirmata.3`).

Each binary is accompanied by an SPDX JSON SBOM:
```
enterprise-kyverno_kyverno_linux_amd64
enterprise-kyverno_kyverno_linux_amd64.sbom.json
enterprise-kyverno_kyverno_linux_arm64
enterprise-kyverno_kyverno_linux_arm64.sbom.json
... (repeat for each component)
```

## Running Tests

Each component has its own integration test. From this directory:

```bash
make test
```

Individual component:
```bash
cd kyverno && BINARY_PATH=/path/to/binary make test
```

## Image Validation (Helm)

A reference `helm-values/chainguard-test-values.yaml` is provided for deploying
N4K via Helm to validate packaged images end-to-end.

```bash
helm upgrade --install kyverno \
  --repo https://nirmata.github.io/kyverno-charts/ kyverno \
  --namespace kyverno --create-namespace \
  -f helm-values/chainguard-test-values.yaml
```

### Required Tools for Tests

| Tool | Purpose |
|---|---|
| `bash` | Test runner |
| `file` | Binary inspection |

Full integration tests additionally require: `kubectl`, `helm`, `kind`.

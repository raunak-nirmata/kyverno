# Namespaced policy credentials and CEL parameters in 1.19.2

This patch changes how namespaced policies use registry credential secrets and
CEL parameters. Review existing policies before upgrading.

## Registry credential secrets

A bare secret name in a namespaced policy now resolves in the policy's own
namespace. It no longer selects a secret in the Kyverno installation namespace.
An explicit `namespace/name` reference must use the policy's namespace.
Admission rejects foreign references, and runtime enforcement also covers
policies stored before the upgrade.

This applies to:

- `Policy` context `imageRegistry.imageRegistryCredentials.secrets`.
- `Policy` rule `verifyImages[].imageRegistryCredentials.secrets`.
- `NamespacedImageValidatingPolicy.spec.credentials.secrets`.
- Namespaced image policy attestor signature pull secrets
  (`spec.attestors[].cosign.source.PullSecrets` in the current API).

For a policy in `tenant-a`, both `regcred` and `tenant-a/regcred` use the
`regcred` Secret in `tenant-a`. References to `kyverno/regcred` or
`tenant-b/regcred` are rejected. If a policy previously used a bare name to
select an installation-namespace secret, provide an appropriate pull secret
in its own namespace before upgrading. Otherwise credential lookup can fall
back to anonymous access and private image pulls or verification can fail.
Do not copy administrator credentials into tenant namespaces; provision
credentials with the permissions appropriate for each tenant.

ImageRegistry context credential references must be literal secret names.
`verifyImages` continues to support rule variables in secret names; the resolved
references are checked again before credentials are loaded.

ClusterPolicy and cluster-scoped ImageValidatingPolicy credential defaults
remain unchanged. Administrator-configured global registry credentials and
resource imagePullSecrets retain their existing behavior.

## CEL parameter resources

For a namespaced legacy `Policy`, both named and selector-based `paramRef`
lookups stay within the policy namespace. An omitted `paramRef.namespace`
resolves there, and an explicit namespace must match.

Cluster-scoped `paramKind` is rejected for namespaced Policies at admission and
runtime. Existing stored Policies using these parameters will fail their next
update; admission evaluation and background scans will report a parameter error.
Move appropriate parameters into a namespaced resource such as a ConfigMap in
the policy namespace. If cluster-wide parameters are required, have a cluster
administrator manage a ClusterPolicy instead.

Install the parameter CRD before creating or updating a namespaced Policy.
Admission checks the exact requested API version, including nonpreferred served
versions, and rejects the policy if discovery cannot establish that the
parameter kind is namespaced. An unrelated namespaced kind with the same name
in another API group remains allowed.

The namespace boundary takes effect immediately on upgrade. A warning-only
transition would allow the cross-namespace access to continue. Native Kubernetes
ValidatingAdmissionPolicy and MutatingAdmissionPolicy parameter semantics, and
ClusterPolicy parameter behavior, remain unchanged.

## Verification and rollout

Admission and runtime regression tests cover local references, foreign and
installation namespaces, invalid references, and missing policy scope.
Conformance fixtures accompany the change for deployed-controller checks.
Publish patched enterprise artifacts before the sanitized upstream changes and
advisory disclosure, following the private release coordination issue.

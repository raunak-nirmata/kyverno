# Internal generate provenance

Run this test in isolation with `protectManagedResources=false` (the default),
before another generate policy watches Secrets. It needs `kubectl`, `jq`, and
`timeout` on the runner. The test grants the controllers Secret access and uses
only its own namespaces, policy, and RBAC resource.

Before installing the policy, the test plants Secrets at the exact intended
output names with real trigger and source UIDs and forged generation labels.
After the policy is ready, it verifies that unchanged-label updates, deletes,
and clone-source traversal cannot enqueue an UpdateRequest. It also attempts to
copy a MAC from a genuine downstream onto a planted object and verifies that
admission rejects the provenance change. Each negative operation watches from
the preceding list resourceVersion for ten seconds, including short lived
UpdateRequests; it also
checks that planted objects retain their data or remain deleted.

CREATE and UPDATE spoof checks submit actual admission operations and require
an error. User-owned clone sources can be restored with their clone-source label
or replaced without it. A standalone `app.kubernetes.io/managed-by: kyverno`
label remains usable while managed-resource protection is disabled.

A newly created trigger is the positive control: its genuine generated Secret
must be restored after an ordinary edit and recreated after deletion. The CEL
GeneratingPolicy controller uses its generated-resource UID cache instead of
the legacy admission-to-UpdateRequest path; focused watcher tests separately
cover uncached forged labels and genuine cached resources.

## Compatibility and key lifecycle

The admission controller creates the immutable `kyverno-generate-provenance`
Secret in the Kyverno installation namespace before serving resource admissions.
The background controller reads that shared key when signing generated targets.
Preserve this Secret across controller restarts and upgrades; replacing or
removing the key invalidates the provenance on existing generated resources.

Unsigned downstreams from an earlier release fail closed for non-trigger edits,
deletes, clone-source traversal, and cleanup. A trusted trigger event or an
explicit `generateExisting` re-evaluation must regenerate or reprocess those
resources to establish authenticated provenance. Policy-update scans of labels
cannot bootstrap that trust: a resource carrying preplanted labels is
indistinguishable from an unsigned old downstream. Pending legacy cleanup
requests without the original policy UID are also ignored. Ordinary source
restores and replacements remain permitted.

The provenance annotation is controller-owned: full-object replacements of a
generated downstream must preserve it, just as they preserve internal routing
labels. Ordinary data and unrelated annotation changes remain supported. A
missing or invalid signing key fails closed; operators using custom RBAC must
allow admission to create/read the key and background to read it. The default
chart already grants these namespace-scoped permissions.

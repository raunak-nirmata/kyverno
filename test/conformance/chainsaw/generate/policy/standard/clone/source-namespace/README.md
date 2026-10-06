# Clone source namespace confinement

This test verifies admission rejects foreign and empty clone source namespaces
for namespaced Policy, including cloneList and foreach. A valid same-namespace
clone must still produce the expected ConfigMap. ConfigMap contents are ordinary
test values.

Runtime tests in `pkg/background/generate/clone_source_test.go` exercise stored
policies directly, after substitution, including policies that current admission
would reject. They assert that no forbidden source read, list, or update reaches
the client, including when a later cloneList kind or foreach element is invalid.

CEL coverage in `pkg/cel/policies/gpol/compiler/clone_source_test.go` verifies the
GPOL compiler's namespaced resource overloads bind reads to the policy namespace
and reject explicit foreign namespace arguments. Cluster-scoped GPOL retains its
explicit namespace arguments. This establishes the namespace-argument behavior
for this change; it does not establish policy-author authorization or metadata
provenance, which are separate concerns.

## Description

This test verifies the resource validation webhook is scoped to `Namespaced` when a ClusterPolicy targets all `*` resources but restricts the match to specific namespaces.

Without that inference the rule is registered with `scope: "*"`, and because cluster-scoped resources ignore namespace filters the webhook intercepts every cluster-scoped CREATE in the cluster (for example `SubjectAccessReview`), which can reentrantly deadlock the admission path.

The sibling `clusterpolicy` test covers the complementary case: an unconstrained `*` match must keep `scope: "*"`.

## Steps

1.  - Create a ClusterPolicy targeting `*` restricted to the `nim-proxy` namespace
    - Assert ClusterPolicy gets ready
1.  - Assert that the resource validation webhook rule is registered with `scope: Namespaced`

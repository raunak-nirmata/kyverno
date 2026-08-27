package license

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
)

// policyListPageSize bounds the metadata held while counting.
const policyListPageSize = 500

// policyResource is one countable policy kind and the API versions it may be
// served at, in preference order. Versions are candidates for the *same* CRD, so
// the first one that lists successfully is authoritative — trying the rest would
// count the same objects twice.
//
// Strings rather than Go types: the policies.kyverno.io types live in the separate
// github.com/kyverno/api module, and strings keep this file immune to its type
// churn across rebases.
type policyResource struct {
	Group    string
	Resource string
	Versions []string
}

// policyResources is every Kyverno policy kind counting toward the cap, including
// namespaced variants — so a limit on ClusterPolicy cannot be sidestepped by
// authoring ValidatingPolicy instead.
//
// Every kind here must also be covered by licensedPolicyRules in
// pkg/controllers/webhook/controller.go, or it would consume cap without ever
// being gated. TestLicenseWebhookCoversCountedKinds enforces that pairing.
//
// Listing several versions matters: pinning a single one means a cluster that
// serves a different version returns NotFound, the kind is silently classed
// unavailable, and it contributes zero to the cap.
var policyResources = []policyResource{
	{Group: "kyverno.io", Resource: "clusterpolicies", Versions: []string{"v1", "v2beta1"}},
	{Group: "kyverno.io", Resource: "policies", Versions: []string{"v1", "v2beta1"}},
	{Group: "policies.kyverno.io", Resource: "validatingpolicies", Versions: []string{"v1", "v1beta1", "v1alpha1"}},
	{Group: "policies.kyverno.io", Resource: "namespacedvalidatingpolicies", Versions: []string{"v1", "v1beta1"}},
	{Group: "policies.kyverno.io", Resource: "mutatingpolicies", Versions: []string{"v1", "v1beta1", "v1alpha1"}},
	{Group: "policies.kyverno.io", Resource: "namespacedmutatingpolicies", Versions: []string{"v1", "v1beta1"}},
	{Group: "policies.kyverno.io", Resource: "generatingpolicies", Versions: []string{"v1", "v1beta1", "v1alpha1"}},
	{Group: "policies.kyverno.io", Resource: "namespacedgeneratingpolicies", Versions: []string{"v1", "v1beta1"}},
	{Group: "policies.kyverno.io", Resource: "imagevalidatingpolicies", Versions: []string{"v1", "v1beta1", "v1alpha1"}},
	{Group: "policies.kyverno.io", Resource: "namespacedimagevalidatingpolicies", Versions: []string{"v1", "v1beta1"}},
	{Group: "policies.kyverno.io", Resource: "deletingpolicies", Versions: []string{"v1", "v1beta1", "v1alpha1"}},
	{Group: "policies.kyverno.io", Resource: "namespaceddeletingpolicies", Versions: []string{"v1", "v1beta1"}},
}

// primaryGVR is the preferred group/version/resource: the first candidate version,
// which is the one a current cluster serves.
func (r policyResource) primaryGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: r.Group, Version: r.Versions[0], Resource: r.Resource}
}

// CountedPolicyResources reports every group/resource that counts toward the
// licensed policy cap. Exported so the webhook controller's test can assert the
// licensing gate covers exactly these kinds.
func CountedPolicyResources() []schema.GroupResource {
	out := make([]schema.GroupResource, 0, len(policyResources))
	for _, r := range policyResources {
		out = append(out, schema.GroupResource{Group: r.Group, Resource: r.Resource})
	}
	return out
}

// policyCounts is the result of one counting pass.
type policyCounts struct {
	// Total is the sum across every kind that could be counted.
	Total int

	// ByKind is keyed by resource name, logged at high verbosity so the cap can be
	// attributed without twelve extra metric series.
	ByKind map[string]int

	// Unavailable lists kinds that could not be counted — CRD absent or RBAC
	// missing. These contribute zero and do not fail the count.
	Unavailable []string
}

// countPolicies counts policy objects across every Kyverno policy kind, via the
// metadata client so specs are not transferred. Errors are per-kind: one un-listable
// kind must not blank out the other eleven.
func countPolicies(ctx context.Context, client metadata.Interface) (policyCounts, error) {
	out := policyCounts{ByKind: make(map[string]int, len(policyResources))}

	for _, resource := range policyResources {
		n, err := countResource(ctx, client, resource)
		switch {
		case err == nil:
			out.ByKind[resource.Resource] = n
			out.Total += n
		case apierrors.IsNotFound(err), apierrors.IsForbidden(err):
			// CRD not installed, or the licensing RBAC rule is missing.
			out.Unavailable = append(out.Unavailable, resource.Resource)
		default:
			return out, err
		}
	}

	// Nothing listable means "zero" is false rather than small — usually missing
	// licensing RBAC. Fail, so the caller keeps the previous count.
	if len(out.Unavailable) == len(policyResources) {
		return out, fmt.Errorf("no Kyverno policy kinds could be listed (%d kinds unavailable); "+
			"the licensing RBAC rule is probably missing", len(out.Unavailable))
	}
	return out, nil
}

// countResource pages through one resource and returns how many objects exist.
//
// Candidate versions are tried in order and the first that responds wins; a
// NotFound means that version is not served, not that the kind is absent, so the
// NotFound from the last candidate is what propagates.
func countResource(ctx context.Context, client metadata.Interface, resource policyResource) (int, error) {
	var err error
	for _, version := range resource.Versions {
		gvr := schema.GroupVersionResource{Group: resource.Group, Version: version, Resource: resource.Resource}
		var total int
		if total, err = countGVR(ctx, client, gvr); err == nil {
			return total, nil
		}
		if !apierrors.IsNotFound(err) {
			return 0, err
		}
	}
	return 0, err
}

// countGVR pages through one group/version/resource and returns how many objects exist.
func countGVR(ctx context.Context, client metadata.Interface, gvr schema.GroupVersionResource) (int, error) {
	total := 0
	opts := metav1.ListOptions{Limit: policyListPageSize}
	for {
		page, err := client.Resource(gvr).List(ctx, opts)
		if err != nil {
			return 0, err
		}
		total += len(page.Items)
		if page.GetContinue() == "" {
			return total, nil
		}
		opts.Continue = page.GetContinue()
	}
}

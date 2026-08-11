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

// policyGVRs is every Kyverno policy kind counting toward the cap, including
// namespaced variants — so a limit on ClusterPolicy cannot be sidestepped by
// authoring ValidatingPolicy instead.
//
// GVR strings rather than Go types: the policies.kyverno.io types live in the
// separate github.com/kyverno/api module, and strings keep this file immune to its
// type churn across rebases.
var policyGVRs = []schema.GroupVersionResource{
	{Group: "kyverno.io", Version: "v1", Resource: "clusterpolicies"},
	{Group: "kyverno.io", Version: "v1", Resource: "policies"},
	{Group: "policies.kyverno.io", Version: "v1beta1", Resource: "validatingpolicies"},
	{Group: "policies.kyverno.io", Version: "v1beta1", Resource: "namespacedvalidatingpolicies"},
	{Group: "policies.kyverno.io", Version: "v1beta1", Resource: "mutatingpolicies"},
	{Group: "policies.kyverno.io", Version: "v1beta1", Resource: "namespacedmutatingpolicies"},
	{Group: "policies.kyverno.io", Version: "v1beta1", Resource: "generatingpolicies"},
	{Group: "policies.kyverno.io", Version: "v1beta1", Resource: "namespacedgeneratingpolicies"},
	{Group: "policies.kyverno.io", Version: "v1beta1", Resource: "imagevalidatingpolicies"},
	{Group: "policies.kyverno.io", Version: "v1beta1", Resource: "namespacedimagevalidatingpolicies"},
	{Group: "policies.kyverno.io", Version: "v1beta1", Resource: "deletingpolicies"},
	{Group: "policies.kyverno.io", Version: "v1beta1", Resource: "namespaceddeletingpolicies"},
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
	out := policyCounts{ByKind: make(map[string]int, len(policyGVRs))}

	for _, gvr := range policyGVRs {
		n, err := countResource(ctx, client, gvr)
		switch {
		case err == nil:
			out.ByKind[gvr.Resource] = n
			out.Total += n
		case apierrors.IsNotFound(err), apierrors.IsForbidden(err):
			// CRD not installed, or the licensing RBAC rule is missing.
			out.Unavailable = append(out.Unavailable, gvr.Resource)
		default:
			return out, err
		}
	}

	// Nothing listable means "zero" is false rather than small — usually missing
	// licensing RBAC. Fail, so the caller keeps the previous count.
	if len(out.Unavailable) == len(policyGVRs) {
		return out, fmt.Errorf("no Kyverno policy kinds could be listed (%d kinds unavailable); "+
			"the licensing RBAC rule is probably missing", len(out.Unavailable))
	}
	return out, nil
}

// countResource pages through one resource and returns how many objects exist.
func countResource(ctx context.Context, client metadata.Interface, gvr schema.GroupVersionResource) (int, error) {
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

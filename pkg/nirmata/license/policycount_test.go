package license

import (
	"context"
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"
)

// kindForResource maps each counted resource to its singular Kind. The metadata
// fake places objects by pluralising the Kind in TypeMeta, so these must be the
// real Kind names rather than anything derived from the resource string.
var kindForResource = map[string]string{
	"clusterpolicies":                   "ClusterPolicy",
	"policies":                          "Policy",
	"validatingpolicies":                "ValidatingPolicy",
	"namespacedvalidatingpolicies":      "NamespacedValidatingPolicy",
	"mutatingpolicies":                  "MutatingPolicy",
	"namespacedmutatingpolicies":        "NamespacedMutatingPolicy",
	"generatingpolicies":                "GeneratingPolicy",
	"namespacedgeneratingpolicies":      "NamespacedGeneratingPolicy",
	"imagevalidatingpolicies":           "ImageValidatingPolicy",
	"namespacedimagevalidatingpolicies": "NamespacedImageValidatingPolicy",
	"deletingpolicies":                  "DeletingPolicy",
	"namespaceddeletingpolicies":        "NamespacedDeletingPolicy",
}

func policyScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	metav1.AddMetaToScheme(s)
	return s
}

func policyObj(t *testing.T, gvr schema.GroupVersionResource, namespace, name string) *metav1.PartialObjectMetadata {
	t.Helper()
	kind, ok := kindForResource[gvr.Resource]
	if !ok {
		t.Fatalf("no Kind mapped for resource %q; add it to kindForResource", gvr.Resource)
	}
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gvr.GroupVersion().String(),
			Kind:       kind,
		},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

// Every policy kind must be counted, so a customer cannot sidestep the cap by
// choosing a different Kyverno policy type.
func TestCountPoliciesCoversEveryKind(t *testing.T) {
	t.Parallel()

	scheme := policyScheme(t)
	objs := make([]runtime.Object, 0, len(policyGVRs))
	for i, gvr := range policyGVRs {
		objs = append(objs, policyObj(t, gvr, "default", fmt.Sprintf("policy-%d", i)))
	}
	client := metadatafake.NewSimpleMetadataClient(scheme, objs...)

	got, err := countPolicies(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != len(policyGVRs) {
		t.Errorf("Total = %d, want %d (one per kind)", got.Total, len(policyGVRs))
	}
	if len(got.Unavailable) != 0 {
		t.Errorf("Unavailable = %v, want none", got.Unavailable)
	}
	for _, gvr := range policyGVRs {
		if got.ByKind[gvr.Resource] != 1 {
			t.Errorf("ByKind[%s] = %d, want 1", gvr.Resource, got.ByKind[gvr.Resource])
		}
	}
}

// A kind whose CRD is absent, or which RBAC forbids, contributes zero without
// making the whole count stale: the other kinds are still trustworthy.
func TestCountPoliciesToleratesMissingKinds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"forbidden by RBAC", apierrors.NewForbidden(schema.GroupResource{Resource: "deletingpolicies"}, "", errors.New("nope"))},
		{"CRD not installed", apierrors.NewNotFound(schema.GroupResource{Resource: "deletingpolicies"}, "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scheme := policyScheme(t)
			client := metadatafake.NewSimpleMetadataClient(scheme,
				policyObj(t, policyGVRs[0], "", "cpol-a"),
			)
			client.PrependReactor("list", "deletingpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})

			got, err := countPolicies(context.Background(), client)
			if err != nil {
				t.Fatalf("a single unavailable kind must not fail the whole count: %v", err)
			}
			if got.Total != 1 {
				t.Errorf("Total = %d, want 1", got.Total)
			}
			if len(got.Unavailable) != 1 || got.Unavailable[0] != "deletingpolicies" {
				t.Errorf("Unavailable = %v, want [deletingpolicies]", got.Unavailable)
			}
		})
	}
}

// Any other failure marks the count stale so the caller carries the previous
// total forward instead of reporting a drop that did not happen.
func TestCountPoliciesMarksStaleOnRealFailure(t *testing.T) {
	t.Parallel()

	scheme := policyScheme(t)
	client := metadatafake.NewSimpleMetadataClient(scheme)
	client.PrependReactor("list", "clusterpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("apiserver on fire"))
	})

	if _, err := countPolicies(context.Background(), client); err == nil {
		t.Fatal("an internal error must be reported so the previous count is retained")
	}
}

func TestCountPoliciesEmptyCluster(t *testing.T) {
	t.Parallel()

	client := metadatafake.NewSimpleMetadataClient(policyScheme(t))
	got, err := countPolicies(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 0 {
		t.Errorf("Total = %d, want 0", got.Total)
	}
}

// The GVR list is the contract for what counts as a policy; guard against a kind
// being dropped or duplicated by an edit.
func TestPolicyGVRsAreCompleteAndUnique(t *testing.T) {
	t.Parallel()

	seen := map[schema.GroupVersionResource]bool{}
	for _, gvr := range policyGVRs {
		if seen[gvr] {
			t.Errorf("duplicate GVR %v", gvr)
		}
		seen[gvr] = true
	}
	if len(policyGVRs) != 12 {
		t.Errorf("policyGVRs has %d entries, want 12; update this test deliberately if a Kyverno policy kind was added", len(policyGVRs))
	}
	for _, want := range []string{"clusterpolicies", "policies", "validatingpolicies", "deletingpolicies"} {
		found := false
		for _, gvr := range policyGVRs {
			if gvr.Resource == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s missing from policyGVRs", want)
		}
	}
}

// If nothing at all can be listed, "zero policies" is a false reading rather than
// a small one — the usual cause is the licensing RBAC never being applied. It must
// be reported as an error so the caller keeps the previous count instead of
// publishing zero for a cluster full of policies.
func TestCountPoliciesFailsWhenNoKindIsListable(t *testing.T) {
	t.Parallel()

	scheme := policyScheme(t)
	client := metadatafake.NewSimpleMetadataClient(scheme)
	for _, gvr := range policyGVRs {
		client.PrependReactor("list", gvr.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Resource: gvr.Resource}, "", errors.New("no permission"))
		})
	}

	got, err := countPolicies(context.Background(), client)
	if err == nil {
		t.Fatal("a total RBAC loss must be reported, not published as a count of zero")
	}
	if len(got.Unavailable) != len(policyGVRs) {
		t.Errorf("Unavailable has %d entries, want all %d", len(got.Unavailable), len(policyGVRs))
	}
}

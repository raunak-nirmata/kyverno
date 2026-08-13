package main

import (
	kyvernoinformer "github.com/kyverno/kyverno/pkg/client/informers/externalversions"
	"github.com/kyverno/kyverno/pkg/nirmata/license"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

// newLicensePolicyCounter builds the live policy count the licensing admission
// gate enforces against.
//
// The evaluator's own count is refreshed once per evaluation interval, which is
// far too coarse to gate on: inside one window a cluster at the cap would admit
// unlimited creates, and a delete-then-create would be refused until the next
// tick. Listing from the informer caches makes the gate accurate to within
// informer lag, and costs nothing per request.
//
// Must be called before the informer factory is started, since asking for an
// informer is what registers it.
//
// The kinds here must match pkg/nirmata/license.CountedPolicyResources, or the
// gate and the reported count would disagree about what a policy is.
func newLicensePolicyCounter(factory kyvernoinformer.SharedInformerFactory) license.PolicyCounter {
	v1 := factory.Kyverno().V1()
	v1beta1 := factory.Policies().V1beta1()

	// One entry per counted kind: whether its cache is ready, and how many objects
	// it holds. Listers are typed, so this cannot be a loop over GVRs.
	type source struct {
		synced cache.InformerSynced
		count  func() (int, error)
	}
	sources := []source{
		{v1.ClusterPolicies().Informer().HasSynced, func() (int, error) {
			out, err := v1.ClusterPolicies().Lister().List(labels.Everything())
			return len(out), err
		}},
		{v1.Policies().Informer().HasSynced, func() (int, error) {
			out, err := v1.Policies().Lister().List(labels.Everything())
			return len(out), err
		}},
		{v1beta1.ValidatingPolicies().Informer().HasSynced, func() (int, error) {
			out, err := v1beta1.ValidatingPolicies().Lister().List(labels.Everything())
			return len(out), err
		}},
		{v1beta1.NamespacedValidatingPolicies().Informer().HasSynced, func() (int, error) {
			out, err := v1beta1.NamespacedValidatingPolicies().Lister().List(labels.Everything())
			return len(out), err
		}},
		{v1beta1.MutatingPolicies().Informer().HasSynced, func() (int, error) {
			out, err := v1beta1.MutatingPolicies().Lister().List(labels.Everything())
			return len(out), err
		}},
		{v1beta1.NamespacedMutatingPolicies().Informer().HasSynced, func() (int, error) {
			out, err := v1beta1.NamespacedMutatingPolicies().Lister().List(labels.Everything())
			return len(out), err
		}},
		{v1beta1.GeneratingPolicies().Informer().HasSynced, func() (int, error) {
			out, err := v1beta1.GeneratingPolicies().Lister().List(labels.Everything())
			return len(out), err
		}},
		{v1beta1.NamespacedGeneratingPolicies().Informer().HasSynced, func() (int, error) {
			out, err := v1beta1.NamespacedGeneratingPolicies().Lister().List(labels.Everything())
			return len(out), err
		}},
		{v1beta1.ImageValidatingPolicies().Informer().HasSynced, func() (int, error) {
			out, err := v1beta1.ImageValidatingPolicies().Lister().List(labels.Everything())
			return len(out), err
		}},
		{v1beta1.NamespacedImageValidatingPolicies().Informer().HasSynced, func() (int, error) {
			out, err := v1beta1.NamespacedImageValidatingPolicies().Lister().List(labels.Everything())
			return len(out), err
		}},
		{v1beta1.DeletingPolicies().Informer().HasSynced, func() (int, error) {
			out, err := v1beta1.DeletingPolicies().Lister().List(labels.Everything())
			return len(out), err
		}},
		{v1beta1.NamespacedDeletingPolicies().Informer().HasSynced, func() (int, error) {
			out, err := v1beta1.NamespacedDeletingPolicies().Lister().List(labels.Everything())
			return len(out), err
		}},
	}

	return func() (int, bool) {
		total := 0
		for _, s := range sources {
			// A cache that has not synced reports zero, which would look like a
			// cluster with room to spare. Fail open instead: the caller keeps the
			// last evaluated count.
			if !s.synced() {
				return 0, false
			}
			n, err := s.count()
			if err != nil {
				return 0, false
			}
			total += n
		}
		return total, true
	}
}

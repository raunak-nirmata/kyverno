package webhook

import (
	"testing"

	"github.com/kyverno/kyverno/pkg/config"
	"github.com/kyverno/kyverno/pkg/nirmata/license"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	rbacv1listers "k8s.io/client-go/listers/rbac/v1"
	"k8s.io/client-go/tools/cache"
)

// The licensing gate and the licensing count must describe the same set of kinds.
//
// A kind that is counted but not gated can be created without limit while still
// consuming the cap, so other kinds get refused in its place — that is the bug
// this pairing exists to prevent. A kind that is gated but not counted would be
// refused on a cap it never contributes to.
func TestLicenseWebhookCoversCountedKinds(t *testing.T) {
	t.Parallel()

	gated := map[schema.GroupResource]bool{}
	for _, rule := range licensedPolicyRules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				gated[schema.GroupResource{Group: group, Resource: resource}] = true
			}
		}
	}

	counted := map[schema.GroupResource]bool{}
	for _, gr := range license.CountedPolicyResources() {
		counted[gr] = true
		if !gated[gr] {
			t.Errorf("%s counts toward the licensed policy cap but no rule in licensedPolicyRules gates it; "+
				"it could be created without limit while consuming other kinds' capacity", gr)
		}
	}

	for gr := range gated {
		if !counted[gr] {
			t.Errorf("%s is gated by the licensing webhook but is not counted by "+
				"pkg/nirmata/license.CountedPolicyResources; it would be refused on a cap it never contributes to", gr)
		}
	}
}

// Licensing has no kill switch, so the capacity gate must not be reachable
// through the disable-webhook ConfigMap.
//
// reconcileWebhookDeletionByName only claims a name that appears in one of these
// lists; a name it does not claim leaves disableWebhook false, so the reconcile
// switch recreates the gate on the next pass. Adding the gate to either list
// would turn a ConfigMap edit into a permanent licensing bypass.
func TestLicenseWebhookIsNotOperatorDisableable(t *testing.T) {
	t.Parallel()

	for _, name := range validationWebhooks {
		if name == config.LicenseValidatingWebhookConfigurationName {
			t.Error("the licensing gate must not be listed in validationWebhooks: " +
				"listing it in the disable-webhook ConfigMap would delete the gate and " +
				"stop cap enforcement permanently")
		}
	}
	for _, name := range mutationWehooks {
		if name == config.LicenseValidatingWebhookConfigurationName {
			t.Error("the licensing gate must not be listed in mutationWehooks")
		}
	}
}

// The gate exists to refuse new policies, never to disable existing ones, and it
// must never be the reason a policy write fails when the controller is unhealthy.
func TestLicenseWebhookIsCreateOnlyAndFailOpen(t *testing.T) {
	t.Parallel()

	c := &controller{
		server:         "kyverno-svc",
		servicePort:    443,
		defaultTimeout: 10,
		// Empty store: buildOwner finds no ClusterRole and returns no owner refs,
		// which is all this test needs.
		clusterroleLister: rbacv1listers.NewClusterRoleLister(
			cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})),
	}

	built, err := c.buildLicenseValidatingWebhookConfiguration(
		t.Context(), config.NewDefaultConfiguration(false), []byte("ca"))
	if err != nil {
		t.Fatal(err)
	}
	if len(built.Webhooks) != 1 {
		t.Fatalf("got %d webhooks, want exactly 1", len(built.Webhooks))
	}
	hook := built.Webhooks[0]

	if hook.FailurePolicy == nil || *hook.FailurePolicy != admissionregistrationv1.Ignore {
		t.Errorf("FailurePolicy = %v, want Ignore: a licensing check must never block policy writes "+
			"when the admission controller is down", hook.FailurePolicy)
	}
	if len(hook.Rules) != len(licensedPolicyRules) {
		t.Errorf("got %d rules, want %d (one per licensed kind)", len(hook.Rules), len(licensedPolicyRules))
	}
	for _, rule := range hook.Rules {
		if len(rule.Operations) != 1 || rule.Operations[0] != admissionregistrationv1.Create {
			t.Errorf("rule %v has operations %v, want CREATE only: existing policies are never disabled",
				rule.Resources, rule.Operations)
		}
	}
}

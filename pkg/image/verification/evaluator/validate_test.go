package evaluator

import (
	"testing"

	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	"github.com/stretchr/testify/assert"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_Validate_NamespacedPolicyRejectsGlobalContext(t *testing.T) {
	ivpol := &policiesv1beta1.ImageValidatingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "gctx", Namespace: "tenant-ns"},
		Spec: policiesv1beta1.ImageValidatingPolicySpec{
			Validations: []admissionregistrationv1.Validation{
				{Expression: `globalContext.get("cluster-entry", "") == null`},
			},
		},
	}

	_, err := Validate(ivpol, nil)
	assert.ErrorContains(t, err, "globalContext.* is not allowed in namespaced policies")
}

func Test_Validate_NamespacedPolicyRejectsGlobalContextInAttestors(t *testing.T) {
	basePolicy := func(attestor policiesv1beta1.Attestor) *policiesv1beta1.ImageValidatingPolicy {
		return &policiesv1beta1.ImageValidatingPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "gctx-attestor", Namespace: "tenant-ns"},
			Spec: policiesv1beta1.ImageValidatingPolicySpec{
				MatchConstraints: &admissionregistrationv1.MatchResources{
					ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
						{
							RuleWithOperations: admissionregistrationv1.RuleWithOperations{
								Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
								Rule: admissionregistrationv1.Rule{
									APIGroups:   []string{""},
									APIVersions: []string{"v1"},
									Resources:   []string{"pods"},
								},
							},
						},
					},
				},
				Attestors: []policiesv1beta1.Attestor{attestor},
				Validations: []admissionregistrationv1.Validation{
					{Expression: `images.containers.map(image, verifyImageSignatures(image, [attestors.att])).all(e, e > 0)`},
				},
			},
		}
	}
	gctxExpr := &policiesv1beta1.StringOrExpression{Expression: `globalContext.get("cluster-entry", "")`}

	tests := []struct {
		name     string
		attestor policiesv1beta1.Attestor
	}{{
		name:     "cosign key expression",
		attestor: policiesv1beta1.Attestor{Name: "att", Cosign: &policiesv1beta1.Cosign{Key: &policiesv1beta1.Key{Expression: `globalContext.get("cluster-entry", "")`}}},
	}, {
		name:     "cosign certificate expression",
		attestor: policiesv1beta1.Attestor{Name: "att", Cosign: &policiesv1beta1.Cosign{Certificate: &policiesv1beta1.Certificate{Certificate: gctxExpr}}},
	}, {
		name:     "cosign certificate chain expression",
		attestor: policiesv1beta1.Attestor{Name: "att", Cosign: &policiesv1beta1.Cosign{Certificate: &policiesv1beta1.Certificate{CertificateChain: gctxExpr}}},
	}, {
		name:     "notary certs expression",
		attestor: policiesv1beta1.Attestor{Name: "att", Notary: &policiesv1beta1.Notary{Certs: gctxExpr}},
	}, {
		name:     "notary tsa certs expression",
		attestor: policiesv1beta1.Attestor{Name: "att", Notary: &policiesv1beta1.Notary{TSACerts: gctxExpr}},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Validate(basePolicy(tt.attestor), nil)
			assert.ErrorContains(t, err, "globalContext.* is not allowed in namespaced policies")
		})
	}

	t.Run("cluster-scoped policy may use globalContext in attestors", func(t *testing.T) {
		pol := basePolicy(tests[0].attestor)
		pol.Namespace = ""
		_, err := Validate(pol, nil)
		assert.NoError(t, err)
	})
}

package evaluator

import (
	"context"
	"testing"

	policieskyvernoio "github.com/kyverno/api/api/policies.kyverno.io"
	policiesv1alpha1 "github.com/kyverno/api/api/policies.kyverno.io/v1alpha1"
	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// multiImageObj carries two images so a signed-only PolicyException cannot
// legitimately cover the unsigned sibling on the same resource.
func multiImageObj(images ...string) map[string]any {
	return map[string]any{
		"foo": map[string]any{
			"bar": images,
		},
	}
}

func exceptionScopePolicy(name string, expression string, message string) *policiesv1beta1.ImageValidatingPolicy {
	return &policiesv1beta1.ImageValidatingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: policiesv1beta1.ImageValidatingPolicySpec{
			EvaluationConfiguration: &policiesv1beta1.EvaluationConfiguration{
				Mode: policieskyvernoio.EvaluationModeJSON,
			},
			ValidationConfigurations: policiesv1alpha1.ValidationConfiguration{
				VerifyDigest: func() *bool { b := false; return &b }(),
			},
			MatchImageReferences: []policiesv1beta1.MatchImageReference{
				{Glob: "ghcr.io/*"},
			},
			ImageExtractors: []policiesv1beta1.ImageExtractor{
				{Name: "bar", Expression: "object.foo.bar"},
			},
			Validations: []admissionregistrationv1.Validation{
				{
					Expression: expression,
					Message:    message,
				},
			},
		},
	}
}

func policyRefs(name string) []policiesv1beta1.PolicyRef {
	return []policiesv1beta1.PolicyRef{{Name: name, Kind: "ImageValidatingPolicy"}}
}

func Test_Eval_ImageScopedException_DoesNotExemptOtherImages(t *testing.T) {
	// Deny if any extracted image is the known-unsigned fixture.
	policy := exceptionScopePolicy("ivpol-exception-scope", "!('"+unsignedImage+"' in images.bar)", "malicious image detected")
	resource := multiImageObj(signedImage, unsignedImage)

	// Sanity: without an exception the unsigned image is denied.
	baseline, err := Evaluate(context.Background(), []*CompiledImageValidatingPolicy{{Policy: policy}}, resource, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, baseline[policy.Name])
	assert.False(t, baseline[policy.Name].Result)
	assert.Equal(t, "malicious image detected", baseline[policy.Name].Message)
	assert.Empty(t, baseline[policy.Name].Exceptions)

	// PolicyException scoped only to the signed image must NOT fully skip evaluation.
	polex := &policiesv1beta1.PolicyException{
		ObjectMeta: metav1.ObjectMeta{Name: "signed-only"},
		Spec: policiesv1beta1.PolicyExceptionSpec{
			PolicyRefs: policyRefs(policy.Name),
			Images:     []string{signedImage},
		},
	}

	result, err := Evaluate(context.Background(), []*CompiledImageValidatingPolicy{
		{Policy: policy, Exceptions: []*policiesv1beta1.PolicyException{polex}},
	}, resource, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result[policy.Name])
	assert.Empty(t, result[policy.Name].Exceptions, "an exception scoped to images=[%s] must not fully exempt the unrelated image %s on the same resource", signedImage, unsignedImage)
	assert.False(t, result[policy.Name].Result, "unsigned image must still be denied under a signed-only exception")
	assert.Equal(t, "malicious image detected", result[policy.Name].Message)
}

func Test_Eval_AllowedValuesScopedException_DoesNotExemptResource(t *testing.T) {
	policy := exceptionScopePolicy("ivpol-allowed-values-scope", "!('"+unsignedImage+"' in images.bar)", "malicious image detected")

	polex := &policiesv1beta1.PolicyException{
		ObjectMeta: metav1.ObjectMeta{Name: "allowed-values-only"},
		Spec: policiesv1beta1.PolicyExceptionSpec{
			PolicyRefs:    policyRefs(policy.Name),
			AllowedValues: []string{"some-allowed-value"},
		},
	}

	result, err := Evaluate(context.Background(), []*CompiledImageValidatingPolicy{
		{Policy: policy, Exceptions: []*policiesv1beta1.PolicyException{polex}},
	}, multiImageObj(signedImage, unsignedImage), nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result[policy.Name])
	assert.Empty(t, result[policy.Name].Exceptions, "an exception carrying only allowedValues must not fully exempt the resource")
	assert.False(t, result[policy.Name].Result)
	assert.Equal(t, "malicious image detected", result[policy.Name].Message)
}

func Test_Eval_FullException_StillExempts(t *testing.T) {
	policy := exceptionScopePolicy("ivpol-full-exception", "false", "always deny")

	polex := &policiesv1beta1.PolicyException{
		ObjectMeta: metav1.ObjectMeta{Name: "full"},
		Spec: policiesv1beta1.PolicyExceptionSpec{
			PolicyRefs: policyRefs(policy.Name),
			// No Images / AllowedValues -> intentional full exemption.
		},
	}

	result, err := Evaluate(context.Background(), []*CompiledImageValidatingPolicy{
		{Policy: policy, Exceptions: []*policiesv1beta1.PolicyException{polex}},
	}, multiImageObj(unsignedImage), nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result[policy.Name])
	require.Len(t, result[policy.Name].Exceptions, 1)
	assert.Equal(t, "full", result[policy.Name].Exceptions[0].Name)
}

func Test_Eval_PartialAndFullExceptions_StillExempts(t *testing.T) {
	policy := exceptionScopePolicy("ivpol-partial-and-full", "false", "always deny")

	partial := &policiesv1beta1.PolicyException{
		ObjectMeta: metav1.ObjectMeta{Name: "partial"},
		Spec: policiesv1beta1.PolicyExceptionSpec{
			PolicyRefs: policyRefs(policy.Name),
			Images:     []string{signedImage},
		},
	}
	full := &policiesv1beta1.PolicyException{
		ObjectMeta: metav1.ObjectMeta{Name: "full"},
		Spec: policiesv1beta1.PolicyExceptionSpec{
			PolicyRefs: policyRefs(policy.Name),
		},
	}

	result, err := Evaluate(context.Background(), []*CompiledImageValidatingPolicy{
		{Policy: policy, Exceptions: []*policiesv1beta1.PolicyException{partial, full}},
	}, multiImageObj(unsignedImage), nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result[policy.Name])
	require.Len(t, result[policy.Name].Exceptions, 1)
	assert.Equal(t, "full", result[policy.Name].Exceptions[0].Name)
}

func Test_Eval_ErroringExceptionAfterFullExemption_DoesNotNegateIt(t *testing.T) {
	policy := exceptionScopePolicy("ivpol-broken-exception", "false", "always deny")

	full := &policiesv1beta1.PolicyException{
		ObjectMeta: metav1.ObjectMeta{Name: "full"},
		Spec: policiesv1beta1.PolicyExceptionSpec{
			PolicyRefs: policyRefs(policy.Name),
		},
	}
	// Compiles, but evaluating object.missing.key on this resource fails at runtime.
	broken := &policiesv1beta1.PolicyException{
		ObjectMeta: metav1.ObjectMeta{Name: "broken"},
		Spec: policiesv1beta1.PolicyExceptionSpec{
			PolicyRefs: policyRefs(policy.Name),
			MatchConditions: []admissionregistrationv1.MatchCondition{
				{Name: "broken", Expression: `object.missing.key == "x"`},
			},
		},
	}

	result, err := Evaluate(context.Background(), []*CompiledImageValidatingPolicy{
		{Policy: policy, Exceptions: []*policiesv1beta1.PolicyException{full, broken}},
	}, multiImageObj(unsignedImage), nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result[policy.Name])
	require.Len(t, result[policy.Name].Exceptions, 1)
	assert.Equal(t, "full", result[policy.Name].Exceptions[0].Name)
}

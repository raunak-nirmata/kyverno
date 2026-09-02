package webhook

import (
	"encoding/json"
	"testing"

	kyvernov1 "github.com/kyverno/kyverno/api/kyverno/v1"
	"github.com/kyverno/kyverno/pkg/autogen"
	"github.com/stretchr/testify/assert"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
)

func Test_inferWildcardScope(t *testing.T) {
	tests := []struct {
		name     string
		policy   string
		expected admissionregistrationv1.ScopeType
	}{
		{
			// the Robinhood case: wildcard match constrained to a namespace must
			// not register an AllScopes webhook that intercepts cluster-scoped
			// resources like SubjectAccessReview.
			name: "wildcard with namespaces filter -> Namespaced",
			policy: `{
				"apiVersion":"kyverno.io/v1","kind":"ClusterPolicy",
				"metadata":{"name":"restrict-ns-access"},
				"spec":{"background":false,"rules":[{"name":"r1",
					"match":{"any":[{"resources":{"kinds":["*"],"namespaces":["nim-proxy"],"operations":["CREATE","UPDATE","DELETE"]}}]},
					"validate":{"deny":{}}}]}}`,
			expected: admissionregistrationv1.NamespacedScope,
		},
		{
			name: "wildcard with namespaceSelector -> Namespaced",
			policy: `{
				"apiVersion":"kyverno.io/v1","kind":"ClusterPolicy",
				"metadata":{"name":"ns-selector"},
				"spec":{"background":false,"rules":[{"name":"r1",
					"match":{"any":[{"resources":{"kinds":["*"],"namespaceSelector":{"matchLabels":{"env":"prod"}}}}]},
					"validate":{"deny":{}}}]}}`,
			expected: admissionregistrationv1.NamespacedScope,
		},
		{
			name: "wildcard with no namespace constraint -> AllScopes",
			policy: `{
				"apiVersion":"kyverno.io/v1","kind":"ClusterPolicy",
				"metadata":{"name":"unconstrained"},
				"spec":{"background":false,"rules":[{"name":"r1",
					"match":{"any":[{"resources":{"kinds":["*"],"operations":["CREATE"]}}]},
					"validate":{"deny":{}}}]}}`,
			expected: admissionregistrationv1.AllScopes,
		},
		{
			// if any contributing wildcard match is unconstrained, we must keep
			// AllScopes even if another wildcard match is namespace-constrained.
			name: "mixed constrained + unconstrained wildcards -> AllScopes",
			policy: `{
				"apiVersion":"kyverno.io/v1","kind":"ClusterPolicy",
				"metadata":{"name":"mixed"},
				"spec":{"background":false,"rules":[{"name":"r1",
					"match":{"any":[
						{"resources":{"kinds":["*"],"namespaces":["nim-proxy"]}},
						{"resources":{"kinds":["*"]}}
					]},
					"validate":{"deny":{}}}]}}`,
			expected: admissionregistrationv1.AllScopes,
		},
		{
			name: "wildcard in match.all with namespaces -> Namespaced",
			policy: `{
				"apiVersion":"kyverno.io/v1","kind":"ClusterPolicy",
				"metadata":{"name":"all-match"},
				"spec":{"background":false,"rules":[{"name":"r1",
					"match":{"all":[{"resources":{"kinds":["*"],"namespaces":["nim-proxy"]}}]},
					"validate":{"deny":{}}}]}}`,
			expected: admissionregistrationv1.NamespacedScope,
		},
		{
			// the wildcard and the namespace filter live in separate match.all
			// entries, so neither description is namespace-constrained on its own
			// and we stay conservative. See inferWildcardScope's doc comment.
			name: "wildcard and namespaces in separate match.all entries -> AllScopes",
			policy: `{
				"apiVersion":"kyverno.io/v1","kind":"ClusterPolicy",
				"metadata":{"name":"all-split"},
				"spec":{"background":false,"rules":[{"name":"r1",
					"match":{"all":[
						{"resources":{"kinds":["*"]}},
						{"resources":{"namespaces":["nim-proxy"]}}
					]},
					"validate":{"deny":{}}}]}}`,
			expected: admissionregistrationv1.AllScopes,
		},
		{
			// group/version-scoped wildcards parse to kind `*` and take the same path
			name: "group-scoped wildcard with namespaces -> Namespaced",
			policy: `{
				"apiVersion":"kyverno.io/v1","kind":"ClusterPolicy",
				"metadata":{"name":"group-wildcard"},
				"spec":{"background":false,"rules":[{"name":"r1",
					"match":{"any":[{"resources":{"kinds":["apps/v1/*"],"namespaces":["nim-proxy"]}}]},
					"validate":{"deny":{}}}]}}`,
			expected: admissionregistrationv1.NamespacedScope,
		},
		{
			name: "group-scoped wildcard with no namespace constraint -> AllScopes",
			policy: `{
				"apiVersion":"kyverno.io/v1","kind":"ClusterPolicy",
				"metadata":{"name":"group-wildcard-unconstrained"},
				"spec":{"background":false,"rules":[{"name":"r1",
					"match":{"any":[{"resources":{"kinds":["apps/v1/*"]}}]},
					"validate":{"deny":{}}}]}}`,
			expected: admissionregistrationv1.AllScopes,
		},
		{
			// `apps/*` is NOT a group wildcard: ParseKindSelector reads it as kind
			// `apps` with all subresources, so no wildcard kind is present and the
			// namespace constraint is irrelevant.
			name: "kind/subresource selector `apps/*` is not a wildcard kind -> AllScopes",
			policy: `{
				"apiVersion":"kyverno.io/v1","kind":"ClusterPolicy",
				"metadata":{"name":"kind-subresource"},
				"spec":{"background":false,"rules":[{"name":"r1",
					"match":{"any":[{"resources":{"kinds":["apps/*"],"namespaces":["nim-proxy"]}}]},
					"validate":{"deny":{}}}]}}`,
			expected: admissionregistrationv1.AllScopes,
		},
		{
			// no wildcard kind at all: default is irrelevant (the wildcard branch
			// won't be taken) but the function returns AllScopes.
			name: "concrete kinds only -> AllScopes (no wildcard)",
			policy: `{
				"apiVersion":"kyverno.io/v1","kind":"ClusterPolicy",
				"metadata":{"name":"concrete"},
				"spec":{"background":false,"rules":[{"name":"r1",
					"match":{"any":[{"resources":{"kinds":["Pod","ConfigMap"],"namespaces":["nim-proxy"]}}]},
					"validate":{"deny":{}}}]}}`,
			expected: admissionregistrationv1.AllScopes,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cpol kyvernov1.ClusterPolicy
			err := json.Unmarshal([]byte(tt.policy), &cpol)
			assert.NoError(t, err)
			assert.Equal(t, tt.expected, inferWildcardScope(autogen.Default.ComputeRules(&cpol, "")))
		})
	}
}

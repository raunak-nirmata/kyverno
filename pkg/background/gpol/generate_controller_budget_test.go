package gpol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	kyvernov1 "github.com/kyverno/kyverno/api/kyverno/v1"
	kyvernov2 "github.com/kyverno/kyverno/api/kyverno/v2"
	celengine "github.com/kyverno/kyverno/pkg/cel/engine"
	gpolengine "github.com/kyverno/kyverno/pkg/cel/policies/gpol/engine"
	"github.com/kyverno/kyverno/pkg/cel/requestbudget"
	"github.com/kyverno/kyverno/pkg/logging"
	"github.com/stretchr/testify/assert"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// errBudgetTestStop keeps the success branch of ProcessUR cold: that branch can launch a
// detached goroutine against the watch manager, which this fixture does not provide.
var errBudgetTestStop = errors.New("budget test: stop after charging")

// budgetProbeEngine stands in for the generating engine and mirrors what the real Handle
// does with the budget: it calls EnsureBackground on the context it was handed, then
// charges a fixed amount. Mirroring matters because a charge against a context carrying no
// budget is a documented no-op, so a probe that only charged would measure nothing.
type budgetProbeEngine struct {
	perRuleContext uint64
	accepted       uint64
	refused        int
	calls          int
}

func (e *budgetProbeEngine) Handle(ctx context.Context, _ celengine.EngineRequest, _ gpolengine.Policy, _ bool) (gpolengine.EngineResponse, error) {
	e.calls++
	budgetCtx, release := requestbudget.EnsureBackground(ctx)
	defer release()
	if err := requestbudget.Charge(budgetCtx, e.perRuleContext); err != nil {
		e.refused++
		return gpolengine.EngineResponse{}, err
	}
	e.accepted += e.perRuleContext
	return gpolengine.EngineResponse{}, errBudgetTestStop
}

type budgetProbeProvider struct {
	policy gpolengine.Policy
}

func (p *budgetProbeProvider) Get(_ context.Context, _ string) (gpolengine.Policy, error) {
	return p.policy, nil
}

type budgetProbeStatusControl struct {
	failures  int
	successes int
}

func (s *budgetProbeStatusControl) Failed(_ string, _ string, _ []kyvernov1.ResourceSpec) (*kyvernov2.UpdateRequest, error) {
	s.failures++
	return nil, nil
}

func (s *budgetProbeStatusControl) Success(_ string, _ []kyvernov1.ResourceSpec) (*kyvernov2.UpdateRequest, error) {
	s.successes++
	return nil, nil
}

func (s *budgetProbeStatusControl) Skip(_ string, _ []kyvernov1.ResourceSpec) (*kyvernov2.UpdateRequest, error) {
	return nil, nil
}

// budgetTestUpdateRequest builds an update request carrying n rule contexts. The operation
// is Update so the trigger is extracted straight from the embedded admission request and no
// cluster client is needed, and synchronization stays off on every rule context.
func budgetTestUpdateRequest(t *testing.T, n int) *kyvernov2.UpdateRequest {
	t.Helper()
	trigger := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      "trigger",
			"namespace": "tenant",
		},
	}
	raw, err := json.Marshal(trigger)
	assert.NoError(t, err)
	ruleContexts := make([]kyvernov2.RuleContext, 0, n)
	for i := 0; i < n; i++ {
		ruleContexts = append(ruleContexts, kyvernov2.RuleContext{
			Rule: fmt.Sprintf("rule-%d", i),
			Trigger: kyvernov1.ResourceSpec{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       fmt.Sprintf("trigger-%d", i),
				Namespace:  "tenant",
			},
		})
	}
	return &kyvernov2.UpdateRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "ur-budget", Namespace: "kyverno"},
		Spec: kyvernov2.UpdateRequestSpec{
			Type:        kyvernov2.CELGenerate,
			Policy:      "gpol-budget",
			RuleContext: ruleContexts,
			Context: kyvernov2.UpdateRequestSpecContext{
				AdmissionRequestInfo: kyvernov2.AdmissionRequestInfoObject{
					Operation: admissionv1.Update,
					AdmissionRequest: &admissionv1.AdmissionRequest{
						Operation: admissionv1.Update,
						Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
						Resource:  metav1.GroupVersionResource{Version: "v1", Resource: "configmaps"},
						Name:      "trigger",
						Namespace: "tenant",
						Object:    runtime.RawExtension{Raw: raw},
					},
				},
			},
		},
	}
}

// TestProcessURSharesOneCostBudgetAcrossRuleContexts pins that the cost ceiling bounds the
// whole update request rather than each of its rule contexts. One update request carries up
// to generateBatchSize (100) rule contexts, one per matching trigger resource. If the
// budget is established inside the loop instead of above it, every iteration starts from a
// fresh context and so gets its own ceiling, and the request as a whole can charge n times
// the intended amount.
//
// The figures below track requestbudget's unexported background ceiling. If that constant
// moves, update them here too.
func TestProcessURSharesOneCostBudgetAcrossRuleContexts(t *testing.T) {
	const (
		ruleContexts     = 10
		chargePerContext = uint64(2_999_999)
		ceiling          = uint64(3_000_000)
	)

	probe := &budgetProbeEngine{perRuleContext: chargePerContext}
	status := &budgetProbeStatusControl{}
	controller := &CELGenerateController{
		engine:        probe,
		provider:      &budgetProbeProvider{policy: gpolengine.Policy{Policy: &policiesv1beta1.GeneratingPolicy{}}},
		statusControl: status,
		log:           logging.GlobalLogger(),
	}

	err := controller.ProcessUR(budgetTestUpdateRequest(t, ruleContexts))
	assert.NoError(t, err)

	t.Logf("rule contexts=%d charge per rule context=%d accepted total=%d refused=%d (one ceiling=%d)",
		ruleContexts, chargePerContext, probe.accepted, probe.refused, ceiling)

	assert.Equal(t, ruleContexts, probe.calls, "every rule context should reach the engine")
	assert.Less(t, probe.accepted, ceiling,
		"the whole update request must stay under one ceiling, not one ceiling per rule context")
	assert.Equal(t, chargePerContext, probe.accepted,
		"only the first rule context should fit inside the shared ceiling")
	assert.Equal(t, ruleContexts-1, probe.refused,
		"every rule context after the ceiling is spent must be refused")
}

package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	kyvernov1 "github.com/kyverno/kyverno/api/kyverno/v1"
	"github.com/kyverno/kyverno/pkg/clients/dclient"
	"github.com/kyverno/kyverno/pkg/config"
	"github.com/kyverno/kyverno/pkg/engine"
	engineapi "github.com/kyverno/kyverno/pkg/engine/api"
	"github.com/kyverno/kyverno/pkg/engine/factories"
	"github.com/kyverno/kyverno/pkg/engine/jmespath"
	imageverifycache "github.com/kyverno/kyverno/pkg/image/verification/cache"
	"github.com/kyverno/kyverno/pkg/logging"
	kubeutils "github.com/kyverno/kyverno/pkg/utils/kube"
	reportutils "github.com/kyverno/kyverno/pkg/utils/report"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
)

// erroringRegistryClientFactory simulates a registry the reports controller cannot reach.
type erroringRegistryClientFactory struct{}

func (erroringRegistryClientFactory) GetClient(context.Context, *kyvernov1.ImageRegistryCredentials, string, []string) (engineapi.RegistryClient, error) {
	return nil, fmt.Errorf("registry unreachable in test")
}

const scanPriorStageMetadataPolicy = `{
	"apiVersion": "kyverno.io/v1",
	"kind": "ClusterPolicy",
	"metadata": {
		"name": "scan-verify-required"
	},
	"spec": {
		"rules": [{
			"name": "verify-image",
			"match": {
				"any": [{
					"resources": {
						"kinds": ["Pod"]
					}
				}]
			},
			"verifyImages": [{
				"imageReferences": ["ghcr.io/verified/*"],
				"required": true,
				"attestors": [{
					"entries": [{
						"keys": {
							"publicKeys": "dummy"
						}
					}]
				}]
			}]
		}]
	}
}`

const scanPriorStageMetadataPod = `{
	"apiVersion": "v1",
	"kind": "Pod",
	"metadata": {
		"name": "test-pod",
		"namespace": "test-ns",
		"annotations": {
			"kyverno.io/verify-images": "{\"ghcr.io/verified/app:v1\":\"pass\"}"
		}
	},
	"spec": {
		"containers": [{
			"name": "app",
			"image": "ghcr.io/verified/app:v1"
		}]
	}
}`

// TestScanResource_ImageVerify_DoesNotTrustPriorStageMetadata pins that the background scan re-verifies instead of trusting stored metadata.
func TestScanResource_ImageVerify_DoesNotTrustPriorStageMetadata(t *testing.T) {
	reportutils.NewReportingConfig(nil, "validate")

	cfg := config.NewDefaultConfiguration(false)
	jp := jmespath.New(cfg)

	scheme := runtime.NewScheme()
	require.NoError(t, kubescheme.AddToScheme(scheme))
	dClient, err := dclient.NewFakeClient(scheme, map[schema.GroupVersionResource]string{})
	require.NoError(t, err)
	dClient.SetDiscovery(dclient.NewFakeDiscoveryClient(nil))

	e := engine.NewEngine(
		cfg,
		jp,
		nil,
		erroringRegistryClientFactory{},
		imageverifycache.DisabledImageVerifyCache(),
		factories.DefaultContextLoaderFactory(nil),
		nil,
		nil,
	)

	sc := NewScanner(logging.GlobalLogger(), e, cfg, jp, dClient, nil, nil, nil)

	var cpol kyvernov1.ClusterPolicy
	require.NoError(t, json.Unmarshal([]byte(scanPriorStageMetadataPolicy), &cpol))
	policy := engineapi.NewKyvernoPolicy(&cpol)

	resource, err := kubeutils.BytesToUnstructured([]byte(scanPriorStageMetadataPod))
	require.NoError(t, err)
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "pods"}

	results := sc.ScanResource(context.Background(), *resource, gvr, "", nil, nil, nil, nil, policy)
	require.Len(t, results, 1)

	for _, result := range results {
		require.NotNil(t, result.EngineResponse, "expected an engine response even when verification cannot run")
		var found bool
		for _, rule := range result.EngineResponse.PolicyResponse.Rules {
			if rule.RuleType() != engineapi.ImageVerify {
				continue
			}
			found = true
			assert.Equal(t, engineapi.RuleStatusError, rule.Status(), "verification must be attempted and its failure reported, regardless of stored metadata")
		}
		require.True(t, found, "expected an ImageVerify rule response")
	}
}

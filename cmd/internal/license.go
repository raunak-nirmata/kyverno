package internal

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/kyverno/kyverno/pkg/config"
	"github.com/kyverno/kyverno/pkg/nirmata/license"
	"k8s.io/client-go/kubernetes"
	eventsv1 "k8s.io/client-go/kubernetes/typed/events/v1"
	"k8s.io/client-go/metadata"
)

// setupLicense starts Nirmata license evaluation. It always returns immediately and
// must never delay or fail startup.
//
// Kept in its own file so setup.go — which also exists upstream — needs only one
// added line.
//
// There is deliberately no flag to disable this: a values-level kill switch would
// be a trivial bypass once a later phase enforces anything, and reporting is
// harmless by construction.
func setupLicense(
	ctx context.Context,
	logger logr.Logger,
	component string,
	kubeClient kubernetes.Interface,
	metadataClient metadata.Interface,
	eventsClient eventsv1.EventsV1Interface,
	configuration config.Configuration,
) {
	license.Start(ctx, logger.WithName("license"), license.Options{
		Component:      component,
		KubeClient:     kubeClient,
		MetadataClient: metadataClient,
		EventsClient:   eventsClient,
		Config:         configuration,
		Namespace:      config.KyvernoNamespace(),
		Interval:       licenseEvaluationInterval,
	})
}

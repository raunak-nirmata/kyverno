package breaker

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	apiregv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	aggregator "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8swatch "k8s.io/apimachinery/pkg/watch"
)

const reportsAPIServiceName = "v1.reports.kyverno.io"

// WatchReportsServerReady watches the v1.reports.kyverno.io APIService and reacts
// when the reports-server becomes available or unavailable.
// onReady is called when Available=True; onNotReady is called when it is not.
// The watch restarts automatically if the connection drops.
func WatchReportsServerReady(
	ctx context.Context,
	logger logr.Logger,
	client aggregator.Interface,
	onReady func(context.Context),
	onNotReady func(),
) {
	go func() {
		for {
			if err := watchAPIService(ctx, logger, client, onReady, onNotReady); err != nil {
				if ctx.Err() != nil {
					return
				}
				logger.Error(err, "reports-server APIService watch failed, retrying in 5s")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
}

func watchAPIService(
	ctx context.Context,
	logger logr.Logger,
	client aggregator.Interface,
	onReady func(context.Context),
	onNotReady func(),
) error {
	svc, err := client.ApiregistrationV1().APIServices().Get(ctx, reportsAPIServiceName, metav1.GetOptions{})
	if err != nil {
		logger.Info("reports-server not reachable yet, reports will be dropped until it is available", "apiservice", reportsAPIServiceName)
		onNotReady()
		return err
	}

	if isAPIServiceAvailable(svc) {
		logger.Info("reports-server is now available, resuming report writes")
		onReady(ctx)
	} else {
		logger.Info("reports-server not yet available, reports will be dropped until it is ready", "apiservice", reportsAPIServiceName)
		onNotReady()
	}

	w, err := client.ApiregistrationV1().APIServices().Watch(ctx, metav1.ListOptions{
		FieldSelector:   "metadata.name=" + reportsAPIServiceName,
		ResourceVersion: svc.ResourceVersion,
	})
	if err != nil {
		return err
	}
	defer w.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-w.ResultChan():
			if !ok {
				return nil
			}
			apiSvc, ok := event.Object.(*apiregv1.APIService)
			if !ok {
				continue
			}
			switch event.Type {
			case k8swatch.Added, k8swatch.Modified:
				if isAPIServiceAvailable(apiSvc) {
					logger.Info("reports-server is now available, resuming report writes")
					onReady(ctx)
				} else {
					logger.Info("reports-server is not available, reports will be dropped until it recovers")
					onNotReady()
				}
			case k8swatch.Deleted:
				logger.Info("reports-server APIService removed, reports will be dropped")
				onNotReady()
			}
		}
	}
}

func isAPIServiceAvailable(svc *apiregv1.APIService) bool {
	for _, cond := range svc.Status.Conditions {
		if cond.Type == apiregv1.Available {
			return cond.Status == apiregv1.ConditionTrue
		}
	}
	return false
}

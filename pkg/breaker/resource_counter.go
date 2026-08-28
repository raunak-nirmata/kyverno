package breaker

import (
	"context"
	"sync"

	reportsv1 "github.com/kyverno/kyverno/api/reports/v1"
	watchtools "github.com/kyverno/kyverno/cmd/kyverno/watch"
	"github.com/kyverno/kyverno/pkg/client/informers/externalversions/internalinterfaces"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	k8swatch "k8s.io/apimachinery/pkg/watch"
	metadataclient "k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/cache"
)

type retryWatcher interface {
	ResultChan() <-chan k8swatch.Event
	IsRunning() bool
	Stop()
	Done() <-chan struct{}
}

type resourceUIDGetter interface {
	GetUID() types.UID
}

type Counter interface {
	Count() (int, bool)
}

type counter struct {
	lock         sync.RWMutex
	entries      sets.Set[types.UID]
	retryWatcher retryWatcher
}

// Stop stops the underlying RetryWatcher. Safe to call multiple times.
func (c *counter) Stop() {
	c.retryWatcher.Stop()
}

func (c *counter) Record(uid types.UID) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.entries.Insert(uid)
}

func (c *counter) Forget(uid types.UID) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.entries.Delete(uid)
}

func (c *counter) Count() (int, bool) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.entries.Len(), c.retryWatcher.IsRunning()
}

func StartResourceCounter(ctx context.Context, client metadataclient.Interface, gvr schema.GroupVersionResource, tweakListOptions internalinterfaces.TweakListOptionsFunc) (*counter, error) {
	objs, err := client.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	watcher := &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (k8swatch.Interface, error) {
			if tweakListOptions != nil {
				tweakListOptions(&options)
			}
			return client.Resource(gvr).Watch(ctx, options)
		},
	}
	watchInterface, err := watchtools.NewRetryWatcher(objs.GetResourceVersion(), watcher)
	if err != nil {
		return nil, err
	}
	entries := sets.New[types.UID]()
	for _, entry := range objs.Items {
		entries.Insert(entry.GetUID())
	}
	w := &counter{
		entries:      entries,
		retryWatcher: watchInterface,
	}
	// Tie the RetryWatcher's lifetime to ctx so callers don't have to remember
	// to call Stop() explicitly; cancelling ctx (e.g. on a reports-server
	// availability transition) stops the watcher and frees its goroutines.
	go func() {
		select {
		case <-ctx.Done():
			watchInterface.Stop()
		case <-watchInterface.Done():
		}
	}()
	go func() {
		for event := range watchInterface.ResultChan() {
			getter, ok := event.Object.(resourceUIDGetter)
			if ok {
				switch event.Type {
				case k8swatch.Added:
					w.Record(getter.GetUID())
				case k8swatch.Modified:
					w.Record(getter.GetUID())
				case k8swatch.Deleted:
					w.Forget(getter.GetUID())
				}
			}
		}
	}()
	return w, nil
}

func StartAdmissionReportsCounter(ctx context.Context, client metadataclient.Interface) (Counter, error) {
	tweakListOptions := func(lo *metav1.ListOptions) {
		lo.LabelSelector = "audit.kyverno.io/source==admission"
	}
	ephrs, err := StartResourceCounter(ctx, client, reportsv1.SchemeGroupVersion.WithResource("ephemeralreports"), tweakListOptions)
	if err != nil {
		return nil, err
	}
	cephrs, err := StartResourceCounter(ctx, client, reportsv1.SchemeGroupVersion.WithResource("clusterephemeralreports"), tweakListOptions)
	if err != nil {
		// ephrs already started its RetryWatcher; without this it would be
		// orphaned (no reference retained anywhere) since we're returning an
		// error instead of a Counter.
		ephrs.Stop()
		return nil, err
	}
	return composite{
		inner: []Counter{ephrs, cephrs},
	}, nil
}

func StartBackgroundReportsCounter(ctx context.Context, client metadataclient.Interface) (Counter, error) {
	tweakListOptions := func(lo *metav1.ListOptions) {
		lo.LabelSelector = "audit.kyverno.io/source==background-scan"
	}
	ephrs, err := StartResourceCounter(ctx, client, reportsv1.SchemeGroupVersion.WithResource("ephemeralreports"), tweakListOptions)
	if err != nil {
		return nil, err
	}
	cephrs, err := StartResourceCounter(ctx, client, reportsv1.SchemeGroupVersion.WithResource("clusterephemeralreports"), tweakListOptions)
	if err != nil {
		// See StartAdmissionReportsCounter: avoid orphaning the first counter.
		ephrs.Stop()
		return nil, err
	}
	return composite{
		inner: []Counter{ephrs, cephrs},
	}, nil
}

type composite struct {
	inner []Counter
}

func (c composite) Count() (int, bool) {
	sum := 0
	for _, counter := range c.inner {
		count, isRunning := counter.Count()
		if !isRunning {
			return 0, false
		}
		sum += count
	}
	return sum, true
}

// stopper is implemented by *counter; kept unexported so the exported Counter
// interface (implemented by other fakes) doesn't need a Stop() method.
type stopper interface {
	Stop()
}

// Stop stops every underlying counter that supports it. Production callers
// tie counter lifetimes to ctx cancellation (see StartResourceCounter), so
// this is primarily for callers/tests holding a concrete composite value.
func (c composite) Stop() {
	for _, inner := range c.inner {
		if s, ok := inner.(stopper); ok {
			s.Stop()
		}
	}
}

package license

import (
	"time"

	"github.com/kyverno/kyverno/pkg/config"
	"k8s.io/client-go/kubernetes"
	eventsv1 "k8s.io/client-go/kubernetes/typed/events/v1"
	"k8s.io/client-go/metadata"
)

// Defaults for the evaluation loop.
const (
	// DefaultInterval is how often license state is re-evaluated.
	DefaultInterval = 15 * time.Minute

	// MinInterval floors the interval, to bound API server load.
	MinInterval = time.Minute

	// MaxInterval caps the interval. Licensing cannot be turned off, so without a
	// ceiling the cadence flag would be a soft kill switch.
	MaxInterval = time.Hour

	// startupDelay lets informer caches and leader election settle before the first
	// evaluation. Jittered at use.
	startupDelay = 30 * time.Second

	// leaderLeaseName is the admission controller's existing Lease. We read it
	// rather than running a second election.
	leaderLeaseName = "kyverno"
)

// Options carries everything the evaluator needs. Every client is nil-tolerant: a
// nil client disables that fact or output rather than failing the evaluator.
type Options struct {
	// Component is the name passed to internal.Setup. Only the admission controller
	// evaluates; see Start.
	Component string

	// KubeClient reads nodes, the license-state ConfigMap, the leader Lease, and
	// the Deployment that events attach to.
	KubeClient kubernetes.Interface

	// MetadataClient counts policies. Nil disables policy counting.
	MetadataClient metadata.Interface

	// EventsClient emits Kubernetes events. Nil disables events.
	EventsClient eventsv1.EventsV1Interface

	// Config is needed to construct the event generator.
	Config config.Configuration

	// Namespace is where the license-state ConfigMap, Lease and Deployment live.
	Namespace string

	// ConfigMapName is the license-state ConfigMap. Defaults to
	// config.KyvernoLicenseStateConfigMapName() when empty.
	ConfigMapName string

	// DeploymentName is the object events attach to.
	DeploymentName string

	// Interval overrides DefaultInterval. Clamped to [MinInterval, MaxInterval].
	Interval time.Duration

	// LicensePath is an optional path to an operator-supplied commercial license
	// file. Empty means community/free-tier only.
	LicensePath string

	// WarnRatio overrides DefaultWarnRatio, the capacity utilization fraction at
	// which a bounded cap starts warning. Zero or out of (0, 1] means the default.
	WarnRatio float64

	// Now is injected so tests are deterministic. Nil means time.Now.
	Now func() time.Time
}

// interval returns the evaluation interval, clamped to [MinInterval, MaxInterval].
// Clamped rather than rejected: a mis-set flag must not stop reporting.
func (o Options) interval() time.Duration {
	switch {
	case o.Interval == 0:
		return DefaultInterval
	case o.Interval < MinInterval:
		return MinInterval
	case o.Interval > MaxInterval:
		return MaxInterval
	default:
		return o.Interval
	}
}

// clock returns the time source.
func (o Options) clock() func() time.Time {
	if o.Now == nil {
		return time.Now
	}
	return o.Now
}

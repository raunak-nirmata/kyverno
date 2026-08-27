package license

import (
	"context"
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// nodeMemoryBlockGiB is the memory covered by one licensed node unit.
	nodeMemoryBlockGiB = 32.0

	// bufferMemoryBlockGiB pads the block, so a node advertising slightly under a
	// round figure (as all do, from kubelet reservations) counts as one unit.
	bufferMemoryBlockGiB = 1.0

	// nodeListPageSize bounds the memory held while counting a large cluster.
	nodeListPageSize = 500
)

// nodeUnits returns the licensed node units for a node's memory capacity:
// ceil(GiB / 33). Non-positive capacity consumes zero.
//
// Must stay bit-compatible with license-guardian, which feeds the same entitlement.
// That code is under internal/ and cannot be imported, so the arithmetic is
// duplicated and pinned by a test carrying its expected values.
func nodeUnits(q resource.Quantity) int {
	if q.Value() <= 0 {
		return 0
	}
	gib := float64(q.Value()) / (1024 * 1024 * 1024)
	return int(math.Ceil(gib / (nodeMemoryBlockGiB + bufferMemoryBlockGiB)))
}

// countNodeUnits sums licensed node units across the cluster.
//
// A paged List, not a lister: a Node informer's cache is kept warm by every node's
// status heartbeat, which is permanent churn to serve one integer every few minutes.
// The typed client is required because capacity lives in status, which the metadata
// client strips.
func countNodeUnits(ctx context.Context, client kubernetes.Interface) (int, error) {
	total := 0
	opts := metav1.ListOptions{Limit: nodeListPageSize}
	for {
		page, err := client.CoreV1().Nodes().List(ctx, opts)
		if err != nil {
			return 0, err
		}
		for i := range page.Items {
			capacity, ok := page.Items[i].Status.Capacity[corev1.ResourceMemory]
			if !ok {
				// Matches license-guardian: no capacity means skipped, not one unit.
				continue
			}
			total += nodeUnits(capacity)
		}
		if page.Continue == "" {
			return total, nil
		}
		opts.Continue = page.Continue
	}
}

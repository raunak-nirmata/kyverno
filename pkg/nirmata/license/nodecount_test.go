package license

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestNodeUnits pins the licensed-node-unit arithmetic. The 30 GiB and 34 GiB
// rows are carried over verbatim from license-guardian's own test so that any
// future divergence between the two implementations fails here.
func TestNodeUnits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		capacity string
		want     int
	}{
		{"zero", "0", 0},
		{"one byte", "1", 1},
		{"8 GiB", "8Gi", 1},
		{"30 GiB matches license-guardian", "30Gi", 1},
		{"32 GiB is one unit", "32Gi", 1},
		{"33 GiB is exactly one unit", "33Gi", 1},
		{"just over 33 GiB rolls to two", "33793Mi", 2},
		{"34 GiB matches license-guardian", "34Gi", 2},
		{"64 GiB", "64Gi", 2},
		{"66 GiB", "66Gi", 2},
		{"67 GiB", "67Gi", 3},
		{"128 GiB", "128Gi", 4},
		{"a real cloud value in Ki", "128974848Ki", 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := nodeUnits(resource.MustParse(tt.capacity)); got != tt.want {
				t.Errorf("nodeUnits(%s) = %d, want %d", tt.capacity, got, tt.want)
			}
		})
	}
}

func TestNodeUnitsNegative(t *testing.T) {
	t.Parallel()

	q := resource.MustParse("-1Gi")
	if got := nodeUnits(q); got != 0 {
		t.Errorf("nodeUnits(negative) = %d, want 0", got)
	}
}

func node(name, memory string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if memory != "" {
		n.Status.Capacity = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)}
	}
	return n
}

// The whole-cluster sum, including license-guardian's documented case that a node
// with no reported memory is skipped rather than counted.
func TestCountNodeUnits(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(
		node("a", "30Gi"), // 1
		node("b", "34Gi"), // 2
		node("c", ""),     // skipped
	)
	got, err := countNodeUnits(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Errorf("countNodeUnits = %d, want 3 (license-guardian parity case)", got)
	}
}

func TestCountNodeUnitsEmptyCluster(t *testing.T) {
	t.Parallel()

	got, err := countNodeUnits(context.Background(), fake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("countNodeUnits = %d, want 0", got)
	}
}

// A List failure must surface so the caller can carry the previous count forward
// rather than report a spurious drop to zero.
func TestCountNodeUnitsPropagatesError(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "nodes"}, "", errors.New("no permission"))
	})

	if _, err := countNodeUnits(context.Background(), client); err == nil {
		t.Fatal("a forbidden node list must be reported, not silently counted as zero")
	}
}

// Paging must accumulate rather than return only the last page.
func TestCountNodeUnitsPaginates(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	call := 0
	client.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		call++
		switch call {
		case 1:
			return true, &corev1.NodeList{
				ListMeta: metav1.ListMeta{Continue: "page2"},
				Items:    []corev1.Node{*node("a", "32Gi"), *node("b", "32Gi")},
			}, nil
		default:
			return true, &corev1.NodeList{
				Items: []corev1.Node{*node("c", "64Gi")},
			}, nil
		}
	})

	got, err := countNodeUnits(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if got != 4 {
		t.Errorf("countNodeUnits = %d, want 4 (1+1 from page one, 2 from page two)", got)
	}
	if call != 2 {
		t.Errorf("expected 2 List calls, got %d", call)
	}
}

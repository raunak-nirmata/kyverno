package cache

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testDigestA = "example.com/repo@sha256:0000000000000000000000000000000000000000000000000000000000000001"
	testDigestB = "example.com/repo@sha256:0000000000000000000000000000000000000000000000000000000000000002"
	testTagRef  = "example.com/repo:latest"
)

// Test_isDigestRef pins the two reference forms the cache guard depends on: a
// bare tag is rejected (it is registry-movable), while a digest reference is
// accepted (it is pinned).
func Test_isDigestRef(t *testing.T) {
	assert.False(t, isDigestRef(testTagRef), "a bare tag must not be treated as an immutable reference")
	assert.True(t, isDigestRef(testDigestA), "a digest reference must be treated as immutable")
}

// Test_Cache_RefusesMutableKeys is the mutable-key regression: a verdict must
// never be stored or served under a mutable (tag-only) reference, in either
// direction.
func Test_Cache_RefusesMutableKeys(t *testing.T) {
	c, err := New(WithCacheEnableFlag(true), WithMaxSize(0), WithTTLDuration(0))
	assert.NoError(t, err)

	pol := &metav1.ObjectMeta{Name: "guard-test-policy", UID: "guard-test-uid", ResourceVersion: "1"}

	t.Run("Set/Get refuse a bare tag", func(t *testing.T) {
		stored, err := c.Set(context.TODO(), pol, "rule", testTagRef, true)
		assert.NoError(t, err)
		assert.False(t, stored, "a tag-only reference must not be stored")

		found, err := c.Get(context.TODO(), pol, "rule", testTagRef, true)
		assert.NoError(t, err)
		assert.False(t, found, "a tag-only reference must never be served, even if something else wrote it")
	})

	t.Run("Set/Get accept a digest and distinguish digests", func(t *testing.T) {
		stored, err := c.Set(context.TODO(), pol, "rule", testDigestA, true)
		assert.NoError(t, err)
		assert.True(t, stored)

		found, err := c.Get(context.TODO(), pol, "rule", testDigestA, true)
		assert.NoError(t, err)
		assert.True(t, found)

		found, err = c.Get(context.TODO(), pol, "rule", testTagRef, true)
		assert.NoError(t, err)
		assert.False(t, found, "a hit for one reference form must not leak to the bare tag")

		found, err = c.Get(context.TODO(), pol, "rule", testDigestB, true)
		assert.NoError(t, err)
		assert.False(t, found, "a different digest must never hit")
	})
}

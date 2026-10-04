package cache

import (
	"context"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/name"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultTTL     = 1 * time.Hour
	defaultMaxSize = 1000
)

type cache struct {
	logger         logr.Logger
	isCacheEnabled bool
	maxSize        int64
	ttl            time.Duration
	cache          *ristretto.Cache
}

type Option = func(*cache) error

func New(options ...Option) (Client, error) {
	cache := &cache{}
	for _, opt := range options {
		if err := opt(cache); err != nil {
			return nil, err
		}
	}
	config := ristretto.Config{
		MaxCost:     cache.maxSize,
		NumCounters: 10 * cache.maxSize,
		BufferItems: 64,
	}
	rcache, err := ristretto.NewCache(&config)
	if err != nil {
		return nil, err
	}
	cache.cache = rcache
	return cache, nil
}

func DisabledImageVerifyCache() Client {
	return &cache{
		logger:         logr.Discard(),
		isCacheEnabled: false,
		maxSize:        0,
		ttl:            0,
	}
}

func WithLogger(l logr.Logger) Option {
	return func(c *cache) error {
		c.logger = l
		return nil
	}
}

func WithCacheEnableFlag(b bool) Option {
	return func(c *cache) error {
		c.isCacheEnabled = b
		return nil
	}
}

func WithMaxSize(s int64) Option {
	return func(c *cache) error {
		if s == 0 {
			s = defaultMaxSize
		}
		c.maxSize = s
		return nil
	}
}

func WithTTLDuration(t time.Duration) Option {
	return func(c *cache) error {
		if t == 0 {
			t = defaultTTL
		}
		c.ttl = t
		return nil
	}
}

func generateKey(policy metav1.Object, ruleName string, imageRef string) string {
	return string(policy.GetUID()) + ";" + policy.GetResourceVersion() + ";" + ruleName + ";" + imageRef
}

// isDigestRef reports whether imageRef is pinned to a digest (either
// "repo@sha256:d" or "repo:tag@sha256:d"). Only such references are immutable
// enough to key a verification verdict; a bare tag can be moved by the
// registry after the entry is written.
func isDigestRef(imageRef string) bool {
	_, err := name.NewDigest(imageRef)
	return err == nil
}

func (c *cache) Set(ctx context.Context, policy metav1.Object, ruleName string, imageRef string, useCache bool) (bool, error) {
	if !c.isCacheEnabled {
		// If cache is globally disabled just return
		return false, nil
	} else if !useCache {
		// Else If enabled globally then return if locally disabled
		return false, nil
	} else if !isDigestRef(imageRef) {
		// A cache entry keyed on anything other than a resolved digest is a
		// verdict bound to a mutable reference, not an immutable one. Refuse
		// to store it rather than optimise the caller into holding a stale key.
		return false, nil
	}
	key := generateKey(policy, ruleName, imageRef)

	stored := c.cache.SetWithTTL(key, nil, 1, c.ttl)
	c.cache.Wait()
	if stored {
		return true, nil
	}
	return false, nil
}

func (c *cache) Get(ctx context.Context, policy metav1.Object, ruleName string, imageRef string, useCache bool) (bool, error) {
	if !c.isCacheEnabled {
		// If cache is globally disabled just return
		return false, nil
	} else if !useCache {
		// Else If enabled globally then return if locally disabled
		return false, nil
	} else if !isDigestRef(imageRef) {
		// Never serve a hit for a mutable reference: the registry may have
		// moved it since any entry could have been written.
		return false, nil
	}
	key := generateKey(policy, ruleName, imageRef)
	_, found := c.cache.Get(key)
	if found {
		return true, nil
	}
	return false, nil
}

package cosign

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	"github.com/kyverno/sdk/extensions/imagedataloader"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testBundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	testEmptyConfigType = "application/vnd.oci.empty.v1+json"

	// validBundle is the smallest content that cosign accepts as a v0.3 sigstore bundle.
	validBundle = `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{"publicKey":{"hint":"x"}},"messageSignature":{"messageDigest":{"algorithm":"SHA2_256","digest":"YQ=="},"signature":"YQ=="}}`
)

type testTaggable struct {
	raw []byte
	mt  types.MediaType
}

func (t testTaggable) RawManifest() ([]byte, error)        { return t.raw, nil }
func (t testTaggable) MediaType() (types.MediaType, error) { return t.mt, nil }

// newBundleRegistry starts an in-process OCI registry with referrers support and
// counts blob GETs so a test can assert that detection never reads a layer.
func newBundleRegistry(t *testing.T) (name.Repository, *atomic.Int64) {
	t.Helper()
	var blobGets atomic.Int64
	inner := registry.New(registry.WithReferrersSupport(true))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/sha256:") {
			blobGets.Add(1)
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	repo, err := name.NewRepository(strings.TrimPrefix(srv.URL, "http://") + "/test/img")
	require.NoError(t, err)
	return repo, &blobGets
}

// pushSubject writes a small random image under the given tag and returns its digest.
func pushSubject(t *testing.T, repo name.Repository, tag string) v1.Descriptor {
	t.Helper()
	img, err := random.Image(256, 1)
	require.NoError(t, err)
	require.NoError(t, remote.Write(repo.Tag(tag), img))
	d, err := img.Digest()
	require.NoError(t, err)
	sz, err := img.Size()
	require.NoError(t, err)
	return v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: d, Size: sz}
}

// pushReferrer attaches a single-layer referrer manifest to subject.
func pushReferrer(t *testing.T, repo name.Repository, subject v1.Descriptor, layerData []byte, layerMediaType, artifactType string) {
	t.Helper()
	layer := static.NewLayer(layerData, types.MediaType(layerMediaType))
	require.NoError(t, remote.WriteLayer(repo, layer))
	layerDigest, err := layer.Digest()
	require.NoError(t, err)

	cfg := static.NewLayer([]byte("{}"), types.MediaType(testEmptyConfigType))
	require.NoError(t, remote.WriteLayer(repo, cfg))
	cfgDigest, err := cfg.Digest()
	require.NoError(t, err)

	m := v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		ArtifactType:  artifactType,
		Config:        v1.Descriptor{MediaType: types.MediaType(testEmptyConfigType), Digest: cfgDigest, Size: 2},
		Layers: []v1.Descriptor{{
			MediaType: types.MediaType(layerMediaType),
			Digest:    layerDigest,
			Size:      int64(len(layerData)),
		}},
		Subject: &subject,
	}
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	h, _, err := v1.SHA256(bytes.NewReader(raw))
	require.NoError(t, err)
	require.NoError(t, remote.Put(repo.Digest(h.String()), testTaggable{raw: raw, mt: types.OCIManifestSchema1}))
}

// gzipPaddedBundle returns a valid v0.3 bundle padded with n bytes of JSON
// whitespace and gzip-compressed, so the blob is tiny but expands greatly.
func gzipPaddedBundle(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err := gw.Write([]byte(validBundle))
	require.NoError(t, err)
	pad := bytes.Repeat([]byte(" "), 1<<20)
	for written := 0; written < n; written += len(pad) {
		_, err = gw.Write(pad)
		require.NoError(t, err)
	}
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

// keyAttestor returns an attestor that takes the no-Sigstore-infrastructure fast
// path in checkOptions, so these tests need no network beyond the test registry.
func bundleTestAttestor() *v1beta1.Cosign {
	return &v1beta1.Cosign{
		Key:   &v1beta1.Key{Data: testPublicKey},
		CTLog: &v1beta1.CTLog{InsecureIgnoreTlog: true},
	}
}

func fetchTestImage(t *testing.T, ref string) *imagedataloader.ImageData {
	t.Helper()
	idf, err := imagedataloader.New(nil, nil, nil)
	require.NoError(t, err)
	img, err := idf.FetchImageData(context.TODO(), ref, nil, nil)
	require.NoError(t, err)
	return img
}

func Test_BundleDetection_DoesNotReadLayers(t *testing.T) {
	repo, blobGets := newBundleRegistry(t)
	subject := pushSubject(t, repo, "subject")
	payload := gzipPaddedBundle(t, 64<<20)
	pushReferrer(t, repo, subject, payload, testBundleMediaType, testBundleMediaType)

	img := fetchTestImage(t, repo.Tag("subject").String())
	v := NewVerifier(nil, logr.Discard())

	blobGets.Store(0)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	cOpts, err := v.buildCheckOptsWithBundleDetection(context.TODO(), bundleTestAttestor(), img)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)

	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("referrer blob is %d bytes; detection allocated %d bytes over %d blob GETs", len(payload), alloc, blobGets.Load())

	assert.True(t, cOpts.NewBundleFormat, "a bundle referrer must still be detected")
	assert.Zero(t, blobGets.Load(), "detection must not fetch any layer blob")
	assert.Less(t, alloc, uint64(8<<20), "detection must not allocate the bundle layer")
}

func Test_BundleDetection_Positive(t *testing.T) {
	repo, _ := newBundleRegistry(t)
	subject := pushSubject(t, repo, "subject")
	pushReferrer(t, repo, subject, []byte(validBundle), testBundleMediaType, testBundleMediaType)

	img := fetchTestImage(t, repo.Tag("subject").String())
	v := NewVerifier(nil, logr.Discard())
	cOpts, err := v.buildCheckOptsWithBundleDetection(context.TODO(), bundleTestAttestor(), img)
	require.NoError(t, err)
	assert.True(t, cOpts.NewBundleFormat)
}

func Test_BundleDetection_NoArtifactTypeOnReferrerManifest(t *testing.T) {
	repo, _ := newBundleRegistry(t)
	subject := pushSubject(t, repo, "subject")
	// A bundle referrer need not declare an artifactType, so detection cannot rely on it.
	pushReferrer(t, repo, subject, []byte(validBundle), testBundleMediaType, "")

	img := fetchTestImage(t, repo.Tag("subject").String())
	v := NewVerifier(nil, logr.Discard())
	cOpts, err := v.buildCheckOptsWithBundleDetection(context.TODO(), bundleTestAttestor(), img)
	require.NoError(t, err)
	assert.True(t, cOpts.NewBundleFormat)
}

func Test_BundleDetection_NegativeNoReferrers(t *testing.T) {
	repo, blobGets := newBundleRegistry(t)
	pushSubject(t, repo, "subject")

	img := fetchTestImage(t, repo.Tag("subject").String())
	v := NewVerifier(nil, logr.Discard())
	blobGets.Store(0)
	cOpts, err := v.buildCheckOptsWithBundleDetection(context.TODO(), bundleTestAttestor(), img)
	require.NoError(t, err)
	assert.False(t, cOpts.NewBundleFormat)
	assert.Zero(t, blobGets.Load())
}

func Test_BundleDetection_NegativeNonBundleReferrer(t *testing.T) {
	repo, blobGets := newBundleRegistry(t)
	subject := pushSubject(t, repo, "subject")
	pushReferrer(t, repo, subject, []byte(`{}`), "application/vnd.in-toto+json", "application/vnd.in-toto+json")

	img := fetchTestImage(t, repo.Tag("subject").String())
	v := NewVerifier(nil, logr.Discard())
	blobGets.Store(0)
	cOpts, err := v.buildCheckOptsWithBundleDetection(context.TODO(), bundleTestAttestor(), img)
	require.NoError(t, err)
	assert.False(t, cOpts.NewBundleFormat)
	assert.Zero(t, blobGets.Load())
}

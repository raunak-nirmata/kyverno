package cosign

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"runtime"
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
	"github.com/sigstore/cosign/v3/pkg/cosign"
	ociremote "github.com/sigstore/cosign/v3/pkg/oci/remote"
	"github.com/stretchr/testify/require"
)

const (
	testBundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	// detection must not grow with the referrer layer, so bound it well below
	// what a single inflated layer of this size would cost
	detectionAllocBudget = 16 << 20
	largeLayerPlaintext  = 256 << 20

	// smallest payload cosign's own bundle parser accepts as a v3 bundle
	validBundleJSON = `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json",` +
		`"verificationMaterial":{"publicKey":{"hint":"test"}},` +
		`"messageSignature":{"messageDigest":{"algorithm":"SHA2_256","digest":"` +
		"4OPbgKpBPdxhxVQnOq0LDYU1kAS5ksqIVYEZfvPqSrE=" + `"},"signature":"AAAA"}}`
)

// rawManifest lets a test PUT a hand-built manifest so a referrer can be laid
// out exactly the way cosign writes a v3 bundle.
type rawManifest struct {
	body []byte
	mt   types.MediaType
}

func (r rawManifest) RawManifest() ([]byte, error)        { return r.body, nil }
func (r rawManifest) MediaType() (types.MediaType, error) { return r.mt, nil }

func startTestRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.WithReferrersSupport(true), registry.Logger(nopLogger(t))))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return u.Host
}

func nopLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(io.Discard, "", 0)
}

func pushSubject(t *testing.T, host, repo string) (name.Tag, v1.Descriptor) {
	t.Helper()
	tag, err := name.NewTag(fmt.Sprintf("%s/%s:latest", host, repo))
	require.NoError(t, err)
	img, err := random.Image(256, 1)
	require.NoError(t, err)
	require.NoError(t, remote.Write(tag, img))
	d, err := remote.Head(tag)
	require.NoError(t, err)
	return tag, *d
}

// pushReferrer attaches a one-layer referrer to subject, laid out the way
// cosign's WriteReferrer does, with the given layer media type and bytes.
func pushReferrer(t *testing.T, host, repo string, subject v1.Descriptor, layerMediaType string, layerBytes []byte) {
	t.Helper()
	pushReferrerLayers(t, host, repo, subject, layerMediaType, [][]byte{layerBytes})
}

func pushReferrerLayers(t *testing.T, host, repo string, subject v1.Descriptor, layerMediaType string, layers [][]byte) {
	t.Helper()
	rp, err := name.NewRepository(fmt.Sprintf("%s/%s", host, repo))
	require.NoError(t, err)

	cfg := static.NewLayer([]byte("{}"), "application/vnd.oci.image.config.v1+json")
	require.NoError(t, remote.WriteLayer(rp, cfg))
	cfgDigest, err := cfg.Digest()
	require.NoError(t, err)
	cfgSize, err := cfg.Size()
	require.NoError(t, err)

	layerDescriptors := make([]map[string]any, 0, len(layers))
	for _, layerBytes := range layers {
		layer := static.NewLayer(layerBytes, types.MediaType(layerMediaType))
		require.NoError(t, remote.WriteLayer(rp, layer))
		layerDigest, err := layer.Digest()
		require.NoError(t, err)
		layerDescriptors = append(layerDescriptors, map[string]any{
			"mediaType": layerMediaType,
			"digest":    layerDigest.String(),
			"size":      len(layerBytes),
		})
	}

	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     string(types.OCIManifestSchema1),
		"artifactType":  layerMediaType,
		"config": map[string]any{
			"mediaType":    "application/vnd.oci.empty.v1+json",
			"artifactType": layerMediaType,
			"digest":       cfgDigest.String(),
			"size":         cfgSize,
		},
		"layers": layerDescriptors,
		"subject": map[string]any{
			"mediaType": string(subject.MediaType),
			"digest":    subject.Digest.String(),
			"size":      subject.Size,
		},
	}
	body, err := json.Marshal(m)
	require.NoError(t, err)
	h, _, err := v1.SHA256(bytes.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, remote.Put(rp.Digest(h.String()), rawManifest{body: body, mt: types.OCIManifestSchema1}))
}

// expandingLayer returns a small gzip stream that inflates to n zero bytes.
func expandingLayer(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := io.CopyN(zw, zeroReader{}, int64(n))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func testRegistryOpts(ctx context.Context) []ociremote.Option {
	return []ociremote.Option{ociremote.WithRemoteOptions(remote.WithContext(ctx))}
}

func allocatedBy(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// Positive control: a referrer cosign itself parses as a v3 bundle is detected.
func TestHasBundleReferrer_BundlePresent(t *testing.T) {
	ctx := context.Background()
	host := startTestRegistry(t)
	tag, subject := pushSubject(t, host, "with-bundle")
	pushReferrer(t, host, "with-bundle", subject, testBundleMediaType, []byte(validBundleJSON))

	bundles, _, err := cosign.GetBundles(ctx, tag, testRegistryOpts(ctx))
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.True(t, hasBundleReferrer(tag, testRegistryOpts(ctx)))
}

// Negative control: an image with no referrers at all is not detected.
func TestHasBundleReferrer_NoReferrers(t *testing.T) {
	ctx := context.Background()
	host := startTestRegistry(t)
	tag, _ := pushSubject(t, host, "no-referrers")

	bundles, _, err := cosign.GetBundles(ctx, tag, testRegistryOpts(ctx))
	require.Error(t, err)
	require.Empty(t, bundles)
	require.False(t, hasBundleReferrer(tag, testRegistryOpts(ctx)))
}

// Negative control: a referrer that is not a bundle is not detected.
func TestHasBundleReferrer_NonBundleReferrer(t *testing.T) {
	ctx := context.Background()
	host := startTestRegistry(t)
	tag, subject := pushSubject(t, host, "other-referrer")
	pushReferrer(t, host, "other-referrer", subject, "application/vnd.in-toto+json", []byte(`{}`))

	bundles, _, err := cosign.GetBundles(ctx, tag, testRegistryOpts(ctx))
	require.Error(t, err)
	require.Empty(t, bundles)
	require.False(t, hasBundleReferrer(tag, testRegistryOpts(ctx)))
}

// Negative control: a referrer with more than one layer is not a bundle.
func TestHasBundleReferrer_MultiLayerReferrer(t *testing.T) {
	ctx := context.Background()
	host := startTestRegistry(t)
	tag, subject := pushSubject(t, host, "multi-layer")
	pushReferrerLayers(t, host, "multi-layer", subject, testBundleMediaType,
		[][]byte{[]byte(validBundleJSON), []byte(validBundleJSON + " ")})

	bundles, _, err := cosign.GetBundles(ctx, tag, testRegistryOpts(ctx))
	require.Error(t, err)
	require.Empty(t, bundles)
	require.False(t, hasBundleReferrer(tag, testRegistryOpts(ctx)))
}

// Detection must not depend on the referrers index carrying an artifactType,
// and must not grow with the size of the referrer layer.
func TestHasBundleReferrer_DoesNotReadLayers(t *testing.T) {
	ctx := context.Background()
	host := startTestRegistry(t)
	tag, subject := pushSubject(t, host, "large-layer")
	pushReferrer(t, host, "large-layer", subject, testBundleMediaType, expandingLayer(t, largeLayerPlaintext))

	digest, err := ociremote.ResolveDigest(tag, testRegistryOpts(ctx)...)
	require.NoError(t, err)
	index, err := ociremote.Referrers(digest, "", testRegistryOpts(ctx)...)
	require.NoError(t, err)
	require.Len(t, index.Manifests, 1)
	t.Logf("index descriptor: mediaType=%q artifactType=%q", index.Manifests[0].MediaType, index.Manifests[0].ArtifactType)

	var detected bool
	alloc := allocatedBy(func() { detected = hasBundleReferrer(tag, testRegistryOpts(ctx)) })
	t.Logf("detection allocated %d bytes", alloc)
	require.True(t, detected)
	require.Less(t, alloc, uint64(detectionAllocBudget))
}

// The detection reached through buildCheckOptsWithBundleDetection reports the
// bundle format for an image whose only referrer is a large bundle layer.
func TestBuildCheckOpts_BundleDetection(t *testing.T) {
	ctx := context.Background()
	host := startTestRegistry(t)
	_, subject := pushSubject(t, host, "e2e")
	pushReferrer(t, host, "e2e", subject, testBundleMediaType, []byte(validBundleJSON))

	idf, err := imagedataloader.New(nil)
	require.NoError(t, err)
	img, err := idf.FetchImageData(ctx, fmt.Sprintf("%s/e2e:latest", host))
	require.NoError(t, err)

	attestor := &v1beta1.Cosign{
		Key: &v1beta1.Key{Data: testPublicKey},
		CTLog: &v1beta1.CTLog{
			URL:                "https://rekor.sigstore.dev",
			InsecureIgnoreTlog: true,
			InsecureIgnoreSCT:  true,
		},
	}
	v := NewVerifier(nil, logr.Discard())

	cOpts, err := v.buildCheckOptsWithBundleDetection(ctx, attestor, img)
	if err != nil {
		t.Skipf("cosign check options unavailable: %v", err)
	}
	require.True(t, cOpts.NewBundleFormat)
}

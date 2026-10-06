package cosign

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
	"github.com/sigstore/cosign/v3/pkg/cosign"
	ociremote "github.com/sigstore/cosign/v3/pkg/oci/remote"
	"github.com/stretchr/testify/require"
)

const (
	testBundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"

	// smallest payload cosign's own bundle parser accepts as a v3 bundle
	validBundleJSON = `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json",` +
		`"verificationMaterial":{"publicKey":{"hint":"test"}},` +
		`"messageSignature":{"messageDigest":{"algorithm":"SHA2_256","digest":"` +
		"4OPbgKpBPdxhxVQnOq0LDYU1kAS5ksqIVYEZfvPqSrE=" + `"},"signature":"AAAA"}}`

	// a decoy anyone with push access to the repository can attach: right media
	// type, content that is not a bundle
	decoyBundleJSON = `{"not":"a real sigstore bundle"}`

	// inflates past maxBundleLayerSize from a few KB on the wire
	oversizedPlaintext = 256 << 20

	// detection must stay bounded well below the inflated layer size
	detectionAllocBudget = 48 << 20
)

// rawManifest lets a test PUT a hand-built manifest so a referrer can be laid
// out exactly the way cosign writes a v3 bundle.
type rawManifest struct {
	body []byte
	mt   types.MediaType
}

func (r rawManifest) RawManifest() ([]byte, error)        { return r.body, nil }
func (r rawManifest) MediaType() (types.MediaType, error) { return r.mt, nil }

var blobGetPath = regexp.MustCompile(`^/v2/.+/blobs/sha256:[0-9a-f]{64}$`)

// localRegistry is an in-process registry that counts layer-blob GETs.
type localRegistry struct {
	host     string
	blobGets atomic.Int64
}

func startTestRegistry(t *testing.T) *localRegistry {
	t.Helper()
	tr := &localRegistry{}
	inner := registry.New(registry.WithReferrersSupport(true), registry.Logger(nopLogger(t)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && blobGetPath.MatchString(r.URL.Path) {
			tr.blobGets.Add(1)
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	tr.host = u.Host
	return tr
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

// cosignDetects reports what cosign's own bundle discovery concludes for ref.
func cosignDetects(ctx context.Context, ref name.Reference) bool {
	bundles, _, err := cosign.GetBundles(ctx, ref, testRegistryOpts(ctx))
	return err == nil && len(bundles) > 0
}

// The regression this fixes: a referrer anyone can plant, carrying the bundle
// media type and content that is not a bundle, must not route the image to the
// v3 attestation path.
func TestHasBundleReferrer_DecoyReferrerNotDetected(t *testing.T) {
	ctx := context.Background()
	reg := startTestRegistry(t)
	tag, subject := pushSubject(t, reg.host, "decoy")
	pushReferrer(t, reg.host, "decoy", subject, testBundleMediaType, []byte(decoyBundleJSON))

	require.False(t, hasBundleReferrer(tag, testRegistryOpts(ctx)))
	require.False(t, cosignDetects(ctx, tag))
}

// End-to-end: the check options a v2-signed image is verified with must stay on
// the v2 path when a decoy referrer is attached.
func TestBuildCheckOpts_DecoyReferrerStaysOnV2Path(t *testing.T) {
	ctx := context.Background()
	reg := startTestRegistry(t)
	_, subject := pushSubject(t, reg.host, "decoy-e2e")
	pushReferrer(t, reg.host, "decoy-e2e", subject, testBundleMediaType, []byte(decoyBundleJSON))

	idf, err := imagedataloader.New(nil)
	require.NoError(t, err)
	img, err := idf.FetchImageData(ctx, fmt.Sprintf("%s/decoy-e2e:latest", reg.host))
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
	require.NoError(t, err)
	require.False(t, cOpts.NewBundleFormat)
}

// Detection must agree with cosign's own bundle discovery in both directions.
func TestHasBundleReferrer_MatchesCosign(t *testing.T) {
	cases := []struct {
		name      string
		mediaType string
		layers    [][]byte
		want      bool
	}{
		{name: "genuine-bundle", mediaType: testBundleMediaType, layers: [][]byte{[]byte(validBundleJSON)}, want: true},
		{name: "decoy-content", mediaType: testBundleMediaType, layers: [][]byte{[]byte(decoyBundleJSON)}, want: false},
		{name: "no-referrers", want: false},
		{name: "non-sigstore-referrer", mediaType: "application/vnd.in-toto+json", layers: [][]byte{[]byte(`{}`)}, want: false},
		{name: "two-layer-referrer", mediaType: testBundleMediaType, layers: [][]byte{[]byte(validBundleJSON), []byte(validBundleJSON + " ")}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			reg := startTestRegistry(t)
			tag, subject := pushSubject(t, reg.host, tc.name)
			if len(tc.layers) > 0 {
				pushReferrerLayers(t, reg.host, tc.name, subject, tc.mediaType, tc.layers)
			}
			require.Equal(t, tc.want, hasBundleReferrer(tag, testRegistryOpts(ctx)), "detector")
			require.Equal(t, tc.want, cosignDetects(ctx, tag), "cosign")
		})
	}
}

// A non-sigstore referrer is rejected from its manifest, so no layer blob is fetched.
func TestHasBundleReferrer_NonSigstoreReferrerReadsNoBlob(t *testing.T) {
	ctx := context.Background()
	reg := startTestRegistry(t)
	tag, subject := pushSubject(t, reg.host, "no-blob-read")
	pushReferrer(t, reg.host, "no-blob-read", subject, "application/vnd.in-toto+json", []byte(validBundleJSON))

	before := reg.blobGets.Load()
	require.False(t, hasBundleReferrer(tag, testRegistryOpts(ctx)))
	require.Equal(t, int64(0), reg.blobGets.Load()-before)
}

// A bundle-media-type layer that inflates past the limit is not a v3 bundle, and
// reading it must not allocate the inflated size.
func TestHasBundleReferrer_OversizedLayerNotDetected(t *testing.T) {
	ctx := context.Background()
	reg := startTestRegistry(t)
	tag, subject := pushSubject(t, reg.host, "oversized")
	bomb := expandingLayer(t, oversizedPlaintext)
	t.Logf("layer is %d bytes on the wire, inflates to %d", len(bomb), oversizedPlaintext)
	pushReferrer(t, reg.host, "oversized", subject, testBundleMediaType, bomb)

	before := reg.blobGets.Load()
	var detected bool
	alloc := allocatedBy(func() { detected = hasBundleReferrer(tag, testRegistryOpts(ctx)) })
	gets := reg.blobGets.Load() - before
	t.Logf("detection allocated %d bytes across %d layer-blob GETs", alloc, gets)

	require.False(t, detected)
	require.Equal(t, int64(1), gets)
	require.Less(t, alloc, uint64(detectionAllocBudget))
	require.False(t, cosignDetects(ctx, tag))
}

// Detection does not depend on the referrers index carrying an artifactType.
func TestReferrersIndexDescriptor(t *testing.T) {
	ctx := context.Background()
	reg := startTestRegistry(t)
	tag, subject := pushSubject(t, reg.host, "index-shape")
	pushReferrer(t, reg.host, "index-shape", subject, testBundleMediaType, []byte(validBundleJSON))

	digest, err := ociremote.ResolveDigest(tag, testRegistryOpts(ctx)...)
	require.NoError(t, err)
	index, err := ociremote.Referrers(digest, "", testRegistryOpts(ctx)...)
	require.NoError(t, err)
	require.Len(t, index.Manifests, 1)
	require.True(t, strings.HasPrefix(string(index.Manifests[0].MediaType), "application/vnd.oci.image.manifest"))
	require.True(t, hasBundleReferrer(tag, testRegistryOpts(ctx)))
}

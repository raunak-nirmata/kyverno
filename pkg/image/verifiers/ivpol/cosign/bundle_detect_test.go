package cosign

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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
	ssldsse "github.com/secure-systems-lab/go-securesystemslib/dsse"
	"github.com/sigstore/cosign/v3/pkg/cosign"
	"github.com/sigstore/cosign/v3/pkg/oci/mutate"
	ociremote "github.com/sigstore/cosign/v3/pkg/oci/remote"
	ocistatic "github.com/sigstore/cosign/v3/pkg/oci/static"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protodsse "github.com/sigstore/protobuf-specs/gen/pb-go/dsse"
	sgbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	sigstoresig "github.com/sigstore/sigstore/pkg/signature"
	sigdsse "github.com/sigstore/sigstore/pkg/signature/dsse"
	"github.com/sigstore/sigstore/pkg/signature/payload"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
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

	// inflates past the payload limit from a few KB on the wire
	oversizedPlaintext = 256 << 20

	// detection must stay bounded well below the inflated layer size
	detectionAllocBudget = 48 << 20

	// predicate type used by the positive-control attestation fixtures
	testPredicateType = "https://example.com/predicate/v1"
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

// cosignBundleCount reports how many bundles cosign's own discovery finds, or -1 on error.
func cosignBundleCount(ctx context.Context, ref name.Reference) int {
	bundles, _, err := cosign.GetBundles(ctx, ref, testRegistryOpts(ctx))
	if err != nil {
		return -1
	}
	return len(bundles)
}

// The regression this fixes: a referrer anyone can plant, carrying the bundle
// media type and content that is not a bundle, must not route the image to the
// v3 attestation path.
func TestHasBundleReferrer_DecoyReferrerNotDetected(t *testing.T) {
	ctx := context.Background()
	reg := startTestRegistry(t)
	tag, subject := pushSubject(t, reg.host, "decoy")
	pushReferrer(t, reg.host, "decoy", subject, testBundleMediaType, []byte(decoyBundleJSON))

	bundles, _ := loadBundles(tag, testRegistryOpts(ctx))
	require.Empty(t, bundles)
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

	cOpts, bundles, _, err := v.buildCheckOptsWithBundleDetection(ctx, attestor, img)
	require.NoError(t, err)
	require.False(t, cOpts.NewBundleFormat)
	require.Empty(t, bundles)
}

// Detection must agree with cosign's own bundle discovery in both directions, and
// when both see bundles, they must see the same count.
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
			bundles, _ := loadBundles(tag, testRegistryOpts(ctx))
			require.Equal(t, tc.want, len(bundles) > 0, "detector")
			require.Equal(t, tc.want, cosignDetects(ctx, tag), "cosign")
			if tc.want {
				require.Equal(t, cosignBundleCount(ctx, tag), len(bundles), "bundle count must match cosign")
			}
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
	bundles, _ := loadBundles(tag, testRegistryOpts(ctx))
	require.Empty(t, bundles)
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
	alloc := allocatedBy(func() {
		b, _ := loadBundles(tag, testRegistryOpts(ctx))
		detected = len(b) > 0
	})
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
	bundles, _ := loadBundles(tag, testRegistryOpts(ctx))
	require.NotEmpty(t, bundles)
}

// ---- helpers shared by the verification-path tests below ----

// keyAttestorFor builds a key-based Cosign attestor with the log ignored, so
// verification does not need network access to Rekor.
func keyAttestorFor(pubPEM string) *v1beta1.Cosign {
	return &v1beta1.Cosign{
		Key: &v1beta1.Key{Data: pubPEM},
		CTLog: &v1beta1.CTLog{
			URL:                "https://rekor.sigstore.dev",
			InsecureIgnoreTlog: true,
			InsecureIgnoreSCT:  true,
		},
	}
}

// stubTuf skips the global TUF bootstrap for the duration of the test.
func stubTuf(t *testing.T) {
	t.Helper()
	restore := initializeTuf
	initializeTuf = func(context.Context, *v1beta1.TUF) error { return nil }
	t.Cleanup(func() { initializeTuf = restore })
}

// signV2 attaches a v2 cosign signature to subject and returns the verifying public key PEM.
func signV2(t *testing.T, host, repo string, subject v1.Descriptor) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(priv.Public())
	require.NoError(t, err)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	rp, err := name.NewRepository(fmt.Sprintf("%s/%s", host, repo))
	require.NoError(t, err)
	subjectRef := rp.Digest(subject.Digest.String())

	signer, err := sigstoresig.LoadECDSASignerVerifier(priv, crypto.SHA256)
	require.NoError(t, err)
	payloadBytes, err := (&payload.Cosign{Image: subjectRef}).MarshalJSON()
	require.NoError(t, err)
	sig, err := signer.SignMessage(bytes.NewReader(payloadBytes))
	require.NoError(t, err)

	ociSig, err := ocistatic.NewSignature(payloadBytes, base64.StdEncoding.EncodeToString(sig))
	require.NoError(t, err)

	se, err := ociremote.SignedEntity(subjectRef, testRegistryOpts(context.Background())...)
	require.NoError(t, err)
	signed, err := mutate.AttachSignatureToEntity(se, ociSig)
	require.NoError(t, err)
	require.NoError(t, ociremote.WriteSignatures(rp, signed, testRegistryOpts(context.Background())...))

	return string(pubPEM)
}

// signV3Bundle builds a genuine key-signed v0.3 sigstore bundle over an in-toto
// statement whose subject matches subjectDigestHex, and returns the bundle JSON
// plus the verifying public key PEM.
func signV3Bundle(t *testing.T, subjectDigestHex string) ([]byte, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(priv.Public())
	require.NoError(t, err)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	stmt := map[string]any{
		"_type": "https://in-toto.io/Statement/v1",
		"subject": []map[string]any{{
			"name":   "subject",
			"digest": map[string]string{"sha256": subjectDigestHex},
		}},
		"predicateType": testPredicateType,
		"predicate":     map[string]any{},
	}
	stmtBytes, err := json.Marshal(stmt)
	require.NoError(t, err)

	signer, err := sigstoresig.LoadECDSASignerVerifier(priv, crypto.SHA256)
	require.NoError(t, err)
	dsseSigner := sigdsse.WrapSigner(signer, "application/vnd.in-toto+json")
	envBytes, err := dsseSigner.SignMessage(bytes.NewReader(stmtBytes))
	require.NoError(t, err)

	var env ssldsse.Envelope
	require.NoError(t, json.Unmarshal(envBytes, &env))
	payloadBytes, err := base64.StdEncoding.DecodeString(env.Payload)
	require.NoError(t, err)
	sigBytes, err := base64.StdEncoding.DecodeString(env.Signatures[0].Sig)
	require.NoError(t, err)

	bundle := &protobundle.Bundle{
		MediaType: testBundleMediaType,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_PublicKey{
				PublicKey: &protocommon.PublicKeyIdentifier{Hint: "test"},
			},
		},
		Content: &protobundle.Bundle_DsseEnvelope{
			DsseEnvelope: &protodsse.Envelope{
				Payload:     payloadBytes,
				PayloadType: env.PayloadType,
				Signatures: []*protodsse.Signature{
					{Sig: sigBytes},
				},
			},
		},
	}
	bundleJSON, err := protojson.Marshal(bundle)
	require.NoError(t, err)
	return bundleJSON, string(pubPEM)
}

// Positive control: a genuine v0.3 bundle, key-signed, verifies end to end through
// VerifyImageSignature. This proves the call-site wiring, not just the loop.
func Test_V3Bundle_GenuineKeySigned_Verifies(t *testing.T) {
	stubTuf(t)
	ctx := context.Background()
	reg := startTestRegistry(t)
	tag, subject := pushSubject(t, reg.host, "v3-genuine")
	bundleJSON, pubPEM := signV3Bundle(t, subject.Digest.Hex)
	pushReferrer(t, reg.host, "v3-genuine", subject, testBundleMediaType, bundleJSON)

	idf, err := imagedataloader.New(nil)
	require.NoError(t, err)
	img, err := idf.FetchImageData(ctx, tag.String())
	require.NoError(t, err)

	v := NewVerifier(nil, logr.Discard())
	attestor := &v1beta1.Attestor{Name: "test", Cosign: keyAttestorFor(pubPEM)}
	require.NoError(t, v.VerifyImageSignature(ctx, img, attestor))
}

// Negative variant: the statement subject does not match the image digest.
func Test_V3Bundle_StatementSubjectMismatch_Fails(t *testing.T) {
	stubTuf(t)
	ctx := context.Background()
	reg := startTestRegistry(t)
	tag, subject := pushSubject(t, reg.host, "v3-mismatch")
	wrongDigest := strings.Repeat("a", len(subject.Digest.Hex))
	bundleJSON, pubPEM := signV3Bundle(t, wrongDigest)
	pushReferrer(t, reg.host, "v3-mismatch", subject, testBundleMediaType, bundleJSON)

	idf, err := imagedataloader.New(nil)
	require.NoError(t, err)
	img, err := idf.FetchImageData(ctx, tag.String())
	require.NoError(t, err)

	v := NewVerifier(nil, logr.Discard())
	attestor := &v1beta1.Attestor{Name: "test", Cosign: keyAttestorFor(pubPEM)}
	err = v.VerifyImageSignature(ctx, img, attestor)
	require.Error(t, err)
}

// Negative variant: the attestor carries a different public key than the one that signed the bundle.
func Test_V3Bundle_WrongKey_Fails(t *testing.T) {
	stubTuf(t)
	ctx := context.Background()
	reg := startTestRegistry(t)
	tag, subject := pushSubject(t, reg.host, "v3-wrongkey")
	bundleJSON, _ := signV3Bundle(t, subject.Digest.Hex)
	pushReferrer(t, reg.host, "v3-wrongkey", subject, testBundleMediaType, bundleJSON)

	idf, err := imagedataloader.New(nil)
	require.NoError(t, err)
	img, err := idf.FetchImageData(ctx, tag.String())
	require.NoError(t, err)

	otherPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	otherPubDER, err := x509.MarshalPKIXPublicKey(otherPriv.Public())
	require.NoError(t, err)
	otherPubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: otherPubDER})

	v := NewVerifier(nil, logr.Discard())
	attestor := &v1beta1.Attestor{Name: "test", Cosign: keyAttestorFor(string(otherPubPEM))}
	err = v.VerifyImageSignature(ctx, img, attestor)
	require.Error(t, err)
}

// The sink: a genuine bundle alongside an oversized decoy referrer still verifies,
// the decoy is skipped rather than read unbounded, and each referrer layer is
// fetched exactly once (no second enumeration during verification).
func Test_V3Bundle_OversizedDecoy_IsSkipped(t *testing.T) {
	stubTuf(t)
	ctx := context.Background()
	reg := startTestRegistry(t)
	tag, subject := pushSubject(t, reg.host, "v3-oversized-decoy")
	bundleJSON, pubPEM := signV3Bundle(t, subject.Digest.Hex)
	pushReferrer(t, reg.host, "v3-oversized-decoy", subject, testBundleMediaType, bundleJSON)
	pushReferrer(t, reg.host, "v3-oversized-decoy", subject, testBundleMediaType, expandingLayer(t, oversizedPlaintext))

	idf, err := imagedataloader.New(nil)
	require.NoError(t, err)
	img, err := idf.FetchImageData(ctx, tag.String())
	require.NoError(t, err)

	v := NewVerifier(nil, logr.Discard())
	attestor := &v1beta1.Attestor{Name: "test", Cosign: keyAttestorFor(pubPEM)}

	before := reg.blobGets.Load()
	var verifyErr error
	alloc := allocatedBy(func() {
		verifyErr = v.VerifyImageSignature(ctx, img, attestor)
	})
	gets := reg.blobGets.Load() - before
	t.Logf("verification allocated %d bytes across %d layer-blob GETs", alloc, gets)

	require.NoError(t, verifyErr)
	require.Less(t, alloc, uint64(oversizedPlaintext))
	require.Equal(t, int64(2), gets, "each candidate referrer must be read exactly once")
}

// The second call site: the same fixture through VerifyAttestationSignature must
// not read the oversized decoy unbounded either.
func Test_V3Bundle_OversizedDecoy_AttestationPathSkipsIt(t *testing.T) {
	stubTuf(t)
	ctx := context.Background()
	reg := startTestRegistry(t)
	tag, subject := pushSubject(t, reg.host, "v3-oversized-decoy-attest")
	bundleJSON, pubPEM := signV3Bundle(t, subject.Digest.Hex)
	pushReferrer(t, reg.host, "v3-oversized-decoy-attest", subject, testBundleMediaType, bundleJSON)
	pushReferrer(t, reg.host, "v3-oversized-decoy-attest", subject, testBundleMediaType, expandingLayer(t, oversizedPlaintext))

	idf, err := imagedataloader.New(nil)
	require.NoError(t, err)
	img, err := idf.FetchImageData(ctx, tag.String())
	require.NoError(t, err)

	v := NewVerifier(nil, logr.Discard())
	attestor := &v1beta1.Attestor{Name: "test", Cosign: keyAttestorFor(pubPEM)}
	attestation := &v1beta1.Attestation{Name: "test", InToto: &v1beta1.InToto{Type: testPredicateType}}

	before := reg.blobGets.Load()
	var verifyErr error
	alloc := allocatedBy(func() {
		verifyErr = v.VerifyAttestationSignature(ctx, img, attestation, attestor)
	})
	gets := reg.blobGets.Load() - before
	t.Logf("attestation verification allocated %d bytes across %d layer-blob GETs", alloc, gets)

	require.NoError(t, verifyErr)
	require.Less(t, alloc, uint64(oversizedPlaintext))
	require.Equal(t, int64(2), gets, "each candidate referrer must be read exactly once")
}

// Rule preserved on v3: a genuine bundle alongside a small junk decoy still verifies.
func Test_V3Bundle_JunkDecoy_IsSkipped(t *testing.T) {
	stubTuf(t)
	ctx := context.Background()
	reg := startTestRegistry(t)
	tag, subject := pushSubject(t, reg.host, "v3-junk-decoy")
	bundleJSON, pubPEM := signV3Bundle(t, subject.Digest.Hex)
	pushReferrer(t, reg.host, "v3-junk-decoy", subject, testBundleMediaType, bundleJSON)
	pushReferrer(t, reg.host, "v3-junk-decoy", subject, testBundleMediaType, []byte(decoyBundleJSON))

	idf, err := imagedataloader.New(nil)
	require.NoError(t, err)
	img, err := idf.FetchImageData(ctx, tag.String())
	require.NoError(t, err)

	v := NewVerifier(nil, logr.Discard())
	attestor := &v1beta1.Attestor{Name: "test", Cosign: keyAttestorFor(pubPEM)}
	require.NoError(t, v.VerifyImageSignature(ctx, img, attestor))
}

// Rule preserved on v2: a v2-signed image with an oversized decoy referrer still
// falls back to v2 and is admitted.
func Test_V2SignedImage_WithOversizedBundleReferrer_IsAdmitted(t *testing.T) {
	stubTuf(t)
	ctx := context.Background()
	reg := startTestRegistry(t)
	tag, subject := pushSubject(t, reg.host, "v2-oversized-decoy")
	pubPEM := signV2(t, reg.host, "v2-oversized-decoy", subject)
	pushReferrer(t, reg.host, "v2-oversized-decoy", subject, testBundleMediaType, expandingLayer(t, oversizedPlaintext))

	idf, err := imagedataloader.New(nil)
	require.NoError(t, err)
	img, err := idf.FetchImageData(ctx, tag.String())
	require.NoError(t, err)

	v := NewVerifier(nil, logr.Discard())
	attestor := &v1beta1.Attestor{Name: "test", Cosign: keyAttestorFor(pubPEM)}

	cOpts, bundles, _, err := v.buildCheckOptsWithBundleDetection(ctx, attestor.Cosign, img)
	require.NoError(t, err)
	require.False(t, cOpts.NewBundleFormat)
	require.Empty(t, bundles)

	require.NoError(t, v.VerifyImageSignature(ctx, img, attestor))
}

// decoyRekorV2Bundle builds a v0.3 bundle whose single transparency-log entry has
// a zero integrated time (Rekor v2 shape) and a throwaway inclusion proof, with
// content unrelated to any real subject, so it fails verification but still
// reaches the per-bundle wrapper (and its shared-CheckOpts write) beforehand.
func decoyRekorV2Bundle(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	signer, err := sigstoresig.LoadECDSASignerVerifier(priv, crypto.SHA256)
	require.NoError(t, err)
	stmtBytes := []byte(`{"_type":"https://in-toto.io/Statement/v1","subject":[{"name":"x","digest":{"sha256":"` + strings.Repeat("b", 64) + `"}}],"predicateType":"` + testPredicateType + `","predicate":{}}`)
	dsseSigner := sigdsse.WrapSigner(signer, "application/vnd.in-toto+json")
	envBytes, err := dsseSigner.SignMessage(bytes.NewReader(stmtBytes))
	require.NoError(t, err)
	var env ssldsse.Envelope
	require.NoError(t, json.Unmarshal(envBytes, &env))
	payloadBytes, err := base64.StdEncoding.DecodeString(env.Payload)
	require.NoError(t, err)
	sigBytes, err := base64.StdEncoding.DecodeString(env.Signatures[0].Sig)
	require.NoError(t, err)

	// canonicalizedBody decodes to "{}", which satisfies the Rekor v2 entry parser
	// without needing to match hashedrekord's actual schema.
	raw := map[string]any{
		"mediaType": testBundleMediaType,
		"verificationMaterial": map[string]any{
			"publicKey": map[string]any{"hint": "decoy"},
			"tlogEntries": []map[string]any{{
				"logIndex":    "1",
				"logId":       map[string]any{"keyId": "AAAA"},
				"kindVersion": map[string]any{"kind": "hashedrekord", "version": "0.0.2"},
				"inclusionProof": map[string]any{
					"logIndex":   "1",
					"rootHash":   "AAAA",
					"treeSize":   "1",
					"hashes":     []string{},
					"checkpoint": map[string]any{"envelope": "x"},
				},
				"canonicalizedBody": base64.StdEncoding.EncodeToString([]byte("{}")),
			}},
		},
		"dsseEnvelope": map[string]any{
			"payload":     base64.StdEncoding.EncodeToString(payloadBytes),
			"payloadType": env.PayloadType,
			"signatures": []map[string]any{
				{"sig": base64.StdEncoding.EncodeToString(sigBytes)},
			},
		},
	}
	out, err := json.Marshal(raw)
	require.NoError(t, err)
	return out
}

// Shared options pinned: a decoy whose transparency-log entries look like Rekor v2
// must not flip UseSignedTimestamps on the shared CheckOpts for a later bundle in
// the same request. cosign's rekorV2Bundle only ever writes that flag on the
// keyless shape (no SigVerifier, log not ignored), so this test uses CheckOpts
// shaped that way, rather than the key attestor used elsewhere in this file: with
// a key verifier set, cosign skips the write entirely and the assertion below
// would be true regardless of the per-iteration copy. The per-iteration copy in
// verifyBundles is what keeps the first bundle's write off the shared CheckOpts.
func Test_V3Bundle_DecoyBeforeGenuine_DoesNotChangeOptions(t *testing.T) {
	ctx := context.Background()
	genuineJSON, _ := signV3Bundle(t, strings.Repeat("c", 64))
	decoyJSON := decoyRekorV2Bundle(t)

	decoy := parseBundleLayer(decoyJSON)
	require.NotNil(t, decoy, "decoy must parse as a bundle to reach the wrapper")
	genuine := parseBundleLayer(genuineJSON)
	require.NotNil(t, genuine)

	hash, err := v1.NewHash("sha256:" + strings.Repeat("c", 64))
	require.NoError(t, err)

	// RootCerts only satisfies verifyBundles' own precondition; it is not enough for
	// either bundle to pass full keyless verification, so an error from the call is
	// expected. What this test pins is the flag, not the verify outcome.
	cOpts := &cosign.CheckOpts{RootCerts: x509.NewCertPool(), ClaimVerifier: cosign.IntotoSubjectClaimVerifier}

	// direct call with an explicit [decoy, genuine] order: test 1 already proves the
	// call-site wiring, this test proves the loop's option handling
	_, _, _ = verifyBundles(ctx, cOpts, []*sgbundle.Bundle{decoy, genuine}, &hash)
	assert.False(t, cOpts.UseSignedTimestamps, "a decoy earlier in the loop must not flip UseSignedTimestamps for a later bundle")
}

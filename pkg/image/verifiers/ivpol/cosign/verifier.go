package cosign

import (
	"context"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	"github.com/kyverno/kyverno/pkg/image/verifiers"
	"github.com/kyverno/kyverno/pkg/logging"
	"github.com/kyverno/sdk/extensions/imagedataloader"
	"github.com/pkg/errors"
	"github.com/sigstore/cosign/v3/pkg/cosign"
	"github.com/sigstore/cosign/v3/pkg/oci"
	ociremote "github.com/sigstore/cosign/v3/pkg/oci/remote"
	ocistatic "github.com/sigstore/cosign/v3/pkg/oci/static"
	"github.com/sigstore/cosign/v3/pkg/policy"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	sgbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	verify "github.com/sigstore/sigstore-go/pkg/verify"
)

type Verifier struct {
	secretInterface imagedataloader.SecretInterface
	log             logr.Logger
}

func NewVerifier(secretInterface imagedataloader.SecretInterface, logger logr.Logger) *Verifier {
	return &Verifier{
		log:             logging.WithName("Cosign"),
		secretInterface: secretInterface,
	}
}

// bundleDetectLog names the logger used by loadBundles, which runs outside any Verifier method.
var bundleDetectLog = logging.WithName("Cosign")

// buildCheckOptsWithBundleDetection builds CheckOpts and loads any cosign v3 bundles
// attached as referrers. Detection and fetch are the same bounded read: the bundles
// returned here are what gets verified, so there is no second, unbounded read later.
func (v *Verifier) buildCheckOptsWithBundleDetection(ctx context.Context, attestor *policiesv1beta1.Cosign, image *imagedataloader.ImageData) (*cosign.CheckOpts, []*sgbundle.Bundle, *v1.Hash, error) {
	cOpts, err := checkOptions(ctx, attestor, image.RemoteOpts(), image.NameOpts(), v.secretInterface)
	if err != nil {
		return nil, nil, nil, err
	}

	bundles, hash := loadBundles(image.NameRef(), cOpts.RegistryClientOpts)
	cOpts.NewBundleFormat = len(bundles) > 0

	return cOpts, bundles, hash, nil
}

// sigstoreBundleMediaTypePrefix identifies the layer of a cosign v3 bundle referrer.
const sigstoreBundleMediaTypePrefix = "application/vnd.dev.sigstore.bundle"

// parseBundleLayer applies cosign's own bundle acceptance check to layer bytes already
// read under a size limit, returning nil if the content is not a usable v0.3+ bundle.
func parseBundleLayer(layerBytes []byte) *sgbundle.Bundle {
	b := &sgbundle.Bundle{}
	if err := b.UnmarshalJSON(layerBytes); err != nil {
		return nil
	}
	if !b.MinVersion("v0.3") {
		return nil
	}
	return b
}

// loadBundles reads every referrer of ref once, under the bounded layer read, and
// returns the ones that parse as cosign v3 bundles, plus the hash of ref's resolved
// digest. Anything unreadable or unparseable within the limit is skipped, matching
// cosign's own GetBundles behaviour, so an oversized or junk referrer never diverts
// verification away from a genuine bundle sitting alongside it.
func loadBundles(ref name.Reference, registryClientOpts []ociremote.Option) ([]*sgbundle.Bundle, *v1.Hash) {
	digest, err := ociremote.ResolveDigest(ref, registryClientOpts...)
	if err != nil {
		return nil, nil
	}
	hash, err := v1.NewHash(digest.Identifier())
	if err != nil {
		return nil, nil
	}
	index, err := ociremote.Referrers(digest, "", registryClientOpts...)
	if err != nil {
		return nil, nil
	}
	var bundles []*sgbundle.Bundle
	for _, result := range index.Manifests {
		referrer, err := name.ParseReference(fmt.Sprintf("%s@%s", digest.Repository, result.Digest.String()))
		if err != nil {
			continue
		}
		img, err := ociremote.SignedImage(referrer, registryClientOpts...)
		if err != nil {
			bundleDetectLog.V(2).Info("skipping referrer", "digest", result.Digest.String(), "reason", err.Error())
			continue
		}
		layers, err := img.Layers()
		if err != nil || len(layers) != 1 {
			continue
		}
		mediaType, err := layers[0].MediaType()
		if err != nil || !strings.HasPrefix(string(mediaType), sigstoreBundleMediaTypePrefix) {
			continue
		}
		// anything unreadable or unparseable within the limit is not a v3 bundle, so
		// verification falls back to the v2 path instead of denying the request
		bundleBytes, err := verifiers.ReadLayerLimited(layers[0], verifiers.PayloadLimit())
		if err != nil {
			// A referrer refused for size carries the bundle media type, so it was
			// meant to be a bundle: say so at a level the operator actually sees,
			// otherwise verification just reports "no matching signatures".
			if errors.Is(err, verifiers.ErrLayerTooLarge) {
				bundleDetectLog.Info("skipping bundle referrer: exceeds the payload limit, raise --imageVerifyPayloadLimit to accept it", "digest", result.Digest.String(), "reason", err.Error())
				continue
			}
			bundleDetectLog.V(2).Info("skipping referrer", "digest", result.Digest.String(), "reason", err.Error())
			continue
		}
		bundle := parseBundleLayer(bundleBytes)
		if bundle == nil {
			bundleDetectLog.V(2).Info("skipping referrer", "digest", result.Digest.String(), "reason", "not a valid sigstore bundle")
			continue
		}
		bundles = append(bundles, bundle)
	}
	return bundles, &hash
}

// verifyBundles verifies each of bundles against hash, reproducing the per-bundle
// wrapper cosign applies around cosign.VerifyNewBundle (DSSE envelope, claim verifier,
// "no matching attestations" on total failure), without re-fetching the referrers:
// the caller already read them once during detection.
func verifyBundles(ctx context.Context, cOpts *cosign.CheckOpts, bundles []*sgbundle.Bundle, hash *v1.Hash) ([]oci.Signature, bool, error) {
	// Enforce this up front, matching cosign's own check.
	if cOpts.RootCerts == nil && cOpts.SigVerifier == nil && cOpts.TrustedMaterial == nil {
		return nil, false, stderrors.New("one of verifier, root certs, or trusted root is required")
	}

	digestBytes, err := hex.DecodeString(hash.Hex)
	if err != nil {
		return nil, false, err
	}
	artifactPolicyOption := verify.WithArtifactDigest(hash.Algorithm, digestBytes)

	var checkedAttestations []oci.Signature
	var atLeastOneVerified bool
	var errs []error
	for _, bundle := range bundles {
		// verify against a copy: VerifyNewBundle can write UseSignedTimestamps on the
		// shared CheckOpts before failing, which must not affect a later bundle
		coCopy := *cOpts
		if _, err := cosign.VerifyNewBundle(ctx, &coCopy, artifactPolicyOption, bundle); err != nil {
			errs = append(errs, err)
			continue
		}
		dsse, ok := bundle.Content.(*protobundle.Bundle_DsseEnvelope)
		if !ok {
			errs = append(errs, fmt.Errorf("bundle does not contain a DSSE envelope"))
			continue
		}
		payload, err := json.Marshal(dsse.DsseEnvelope)
		if err != nil {
			errs = append(errs, fmt.Errorf("marshaling DSSE envelope: %w", err))
			continue
		}
		att, err := ocistatic.NewAttestation(payload)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if cOpts.ClaimVerifier != nil {
			if err := cOpts.ClaimVerifier(att, *hash, cOpts.Annotations); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		checkedAttestations = append(checkedAttestations, att)
		atLeastOneVerified = true
	}

	if len(checkedAttestations) == 0 {
		return nil, false, fmt.Errorf("no matching attestations: %w", stderrors.Join(errs...))
	}

	return checkedAttestations, atLeastOneVerified, nil
}

func (v *Verifier) VerifyImageSignature(ctx context.Context, image *imagedataloader.ImageData, attestor *policiesv1beta1.Attestor) error {
	if attestor.Cosign == nil {
		return fmt.Errorf("cosign verifier only supports cosign attestor")
	}

	logger := v.log.WithValues("image", image.Image, "digest", image.Digest, "attestor", attestor.Name)
	logger.V(2).Info("verifying cosign image signature", "image", image.Image)

	cOpts, bundles, hash, err := v.buildCheckOptsWithBundleDetection(ctx, attestor.Cosign, image)
	if err != nil {
		err := errors.Wrapf(err, "failed to build cosign verification opts")
		logger.Error(err, "image verification failed")
		return err
	}

	// Set appropriate claim verifier based on format
	if cOpts.NewBundleFormat {
		cOpts.ClaimVerifier = cosign.IntotoSubjectClaimVerifier
	} else {
		cOpts.ClaimVerifier = cosign.SimpleClaimVerifier
	}

	var sigs []oci.Signature
	var verified bool

	if cOpts.NewBundleFormat {
		sigs, verified, err = verifyBundles(ctx, cOpts, bundles, hash)
	} else {
		sigs, verified, err = cosign.VerifyImageSignatures(ctx, image.NameRef(), cOpts)
	}
	if err != nil {
		err := errors.Wrapf(err, "failed to verify cosign signatures")
		logger.Error(err, "image verification failed")
		return err
	} else if !verified {
		if !(attestor.Cosign.CTLog.InsecureIgnoreTlog || attestor.Cosign.CTLog.InsecureIgnoreSCT) {
			err := fmt.Errorf("transparency log or timestamp verification failed")
			logger.Error(err, "image verification failed")
			return err
		}
	} else if len(sigs) == 0 {
		err := fmt.Errorf("signatures not found")
		logger.Error(err, "image verification failed")
		return err
	}

	if len(attestor.Cosign.Annotations) != 0 {
		var annotationErrors []error
		for _, sig := range sigs {
			if err := checkSignatureAnnotations(sig, attestor.Cosign.Annotations); err != nil {
				annotationErrors = append(annotationErrors, err)
				continue
			}
			return nil
		}
		err := fmt.Errorf("no signature matched the required annotations: %v", annotationErrors)
		logger.Error(err, "image verification failed")
		return err
	}

	return nil
}

func (v *Verifier) VerifyAttestationSignature(ctx context.Context, image *imagedataloader.ImageData, attestation *policiesv1beta1.Attestation, attestor *policiesv1beta1.Attestor) error {
	if attestation.InToto == nil {
		return fmt.Errorf("cosgin verifier only supports intoto referrers as attestations")
	}
	if attestor.Cosign == nil {
		return fmt.Errorf("cosign verifier only supports cosign attestor")
	}

	logger := v.log.WithValues("image", image.Image, "digest", image.Digest, "attestation", attestation.Name, "attestor", attestor.Name)
	logger.V(2).Info("verifying cosign attestation signature", "image", image.Image)

	cOpts, bundles, hash, err := v.buildCheckOptsWithBundleDetection(ctx, attestor.Cosign, image)
	if err != nil {
		err := errors.Wrapf(err, "failed to build cosign verification opts")
		logger.Error(err, "image verification failed")
		return err
	}

	// Attestations always use IntotoSubjectClaimVerifier
	cOpts.ClaimVerifier = cosign.IntotoSubjectClaimVerifier

	var sigs []oci.Signature
	var verified bool
	if cOpts.NewBundleFormat {
		sigs, verified, err = verifyBundles(ctx, cOpts, bundles, hash)
	} else {
		sigs, verified, err = cosign.VerifyImageAttestations(ctx, image.NameRef(), cOpts)
	}
	if err != nil {
		err := errors.Wrapf(err, "failed to verify cosign signatures")
		logger.Error(err, "image verification failed")
		return err
	} else if !verified {
		err := fmt.Errorf("cosign bundle verification failed")
		logger.Error(err, "image verification failed")
		return err
	}

	checkedTypes := []string{}
	found := false
	var annotationErrors []error
	for _, s := range sigs {
		payload, gotType, err := policy.AttestationToPayloadJSON(ctx, attestation.InToto.Type, s)
		if err != nil {
			return fmt.Errorf("converting to consumable policy validation: %w", err)
		}
		checkedTypes = append(checkedTypes, gotType)
		if len(payload) == 0 {
			// This is not the predicate type we're looking for.
			continue
		}

		if len(attestor.Cosign.Annotations) != 0 {
			if err := checkSignatureAnnotations(s, attestor.Cosign.Annotations); err != nil {
				annotationErrors = append(annotationErrors, err)
				continue
			}
		}

		found = true
		image.AddVerifiedIntotoPayloads(gotType, payload)
	}

	if !found {
		if len(annotationErrors) > 0 {
			err := fmt.Errorf("no attestation matched the required annotations: %v", annotationErrors)
			logger.Error(err, "image verification failed")
			return err
		}
		err := fmt.Errorf("required predicate type %s not found, found %v", attestation.InToto.Type, checkedTypes)
		logger.Error(err, "image verification failed")
		return err
	}

	return nil
}

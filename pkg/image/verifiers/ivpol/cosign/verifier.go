package cosign

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/name"
	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	"github.com/kyverno/kyverno/pkg/logging"
	"github.com/kyverno/sdk/extensions/imagedataloader"
	"github.com/pkg/errors"
	"github.com/sigstore/cosign/v3/pkg/cosign"
	"github.com/sigstore/cosign/v3/pkg/oci"
	ociremote "github.com/sigstore/cosign/v3/pkg/oci/remote"
	"github.com/sigstore/cosign/v3/pkg/policy"
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

// buildCheckOptsWithBundleDetection builds CheckOpts and auto-detects cosign v3 bundle format
func (v *Verifier) buildCheckOptsWithBundleDetection(ctx context.Context, attestor *policiesv1beta1.Cosign, image *imagedataloader.ImageData) (*cosign.CheckOpts, error) {
	cOpts, err := checkOptions(ctx, attestor, image.RemoteOpts(), image.NameOpts(), v.secretInterface)
	if err != nil {
		return nil, err
	}

	// Auto-detect if new bundle format is actually present (cosign v3)
	cOpts.NewBundleFormat = hasBundleReferrer(image.NameRef(), cOpts.RegistryClientOpts)

	return cOpts, nil
}

// sigstoreBundleMediaTypePrefix identifies the layer of a cosign v3 bundle referrer.
const sigstoreBundleMediaTypePrefix = "application/vnd.dev.sigstore.bundle"

// hasBundleReferrer reports whether a cosign v3 bundle is attached to ref. It
// reads only the referrers index and the referrer manifests, never a layer blob,
// because detection runs before anything about the image has been verified.
func hasBundleReferrer(ref name.Reference, registryClientOpts []ociremote.Option) bool {
	digest, err := ociremote.ResolveDigest(ref, registryClientOpts...)
	if err != nil {
		return false
	}
	index, err := ociremote.Referrers(digest, "", registryClientOpts...)
	if err != nil {
		return false
	}
	for _, result := range index.Manifests {
		referrer, err := name.ParseReference(fmt.Sprintf("%s@%s", digest.Repository, result.Digest.String()))
		if err != nil {
			continue
		}
		img, err := ociremote.SignedImage(referrer, registryClientOpts...)
		if err != nil {
			continue
		}
		manifest, err := img.Manifest()
		if err != nil {
			continue
		}
		// mirrors cosign's own bundle layout check, minus the layer read
		if len(manifest.Layers) != 1 {
			continue
		}
		if strings.HasPrefix(string(manifest.Layers[0].MediaType), sigstoreBundleMediaTypePrefix) {
			return true
		}
	}
	return false
}

func (v *Verifier) VerifyImageSignature(ctx context.Context, image *imagedataloader.ImageData, attestor *policiesv1beta1.Attestor) error {
	if attestor.Cosign == nil {
		return fmt.Errorf("cosign verifier only supports cosign attestor")
	}

	logger := v.log.WithValues("image", image.Image, "digest", image.Digest, "attestor", attestor.Name)
	logger.V(2).Info("verifying cosign image signature", "image", image.Image)

	cOpts, err := v.buildCheckOptsWithBundleDetection(ctx, attestor.Cosign, image)
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
		sigs, verified, err = cosign.VerifyImageAttestations(ctx, image.NameRef(), cOpts)
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

	cOpts, err := v.buildCheckOptsWithBundleDetection(ctx, attestor.Cosign, image)
	if err != nil {
		err := errors.Wrapf(err, "failed to build cosign verification opts")
		logger.Error(err, "image verification failed")
		return err
	}

	// Attestations always use IntotoSubjectClaimVerifier
	cOpts.ClaimVerifier = cosign.IntotoSubjectClaimVerifier

	sigs, verified, err := cosign.VerifyImageAttestations(ctx, image.NameRef(), cOpts)
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

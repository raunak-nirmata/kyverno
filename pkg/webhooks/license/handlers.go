// Package license implements the Nirmata licensing capacity gate.
//
// It is deliberately separate from pkg/webhooks/policy: the gate has to cover
// every policy kind that counts toward the licensed cap, including kinds whose
// content Kyverno does not validate at admission. Keeping it in its own webhook
// means adding a kind to the cap never changes what gets content-validated.
package license

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"github.com/kyverno/kyverno/pkg/nirmata/license"
	admissionutils "github.com/kyverno/kyverno/pkg/utils/admission"
	"github.com/kyverno/kyverno/pkg/webhooks/handlers"
)

type licenseHandlers struct{}

func NewHandlers() *licenseHandlers {
	return &licenseHandlers{}
}

// Validate refuses a policy CREATE that would exceed the licensed policy cap.
// It never inspects the object: the decision depends only on the operation and
// the current license posture, so an unparseable or unknown policy kind cannot
// turn this into an outage.
func (h *licenseHandlers) Validate(_ context.Context, logger logr.Logger, request handlers.AdmissionRequest, _ string, _ time.Time) handlers.AdmissionResponse {
	if err := license.AdmitPolicyCreate(request.Operation); err != nil {
		logger.Info("policy create refused by the Nirmata license gate",
			"kind", request.Kind.Kind, "name", request.Name, "namespace", request.Namespace, "error", err.Error())
		return admissionutils.Response(request.UID, err)
	}
	return admissionutils.ResponseSuccess(request.UID)
}

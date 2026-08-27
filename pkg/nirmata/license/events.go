package license

import (
	"context"
	"fmt"
	"time"

	"github.com/kyverno/kyverno/pkg/event"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Event reason and source values. event.Reason and friends are open named string
// types, so these need no edit to pkg/event.
//
// Reason must not be event.PolicyApplied, which the controller drops unless
// generateSuccessEvents is on. These are also absent from the chart's default
// omitEvents list, so they pass through.
const (
	reasonLicenseStatus  event.Reason = "LicenseStatus"
	reasonLicenseWarning event.Reason = "LicenseWarning"
	reasonLicenseLimit   event.Reason = "LicenseLimitExceeded"

	sourceLicense event.Source = "kyverno-license"

	// limitReminderInterval re-surfaces a long-running overrun, since events
	// otherwise fire only on transition.
	limitReminderInterval = 24 * time.Hour
)

// resolveDeploymentRef builds an ObjectReference for the admission controller
// Deployment.
//
// The Deployment, not a policy or Namespace: the event controller drops events with
// an empty Regarding.Name, drops events regarding Kyverno policy objects unless
// generatePolicyEvents is on, and rewrites an empty namespace to "default" for
// cluster-scoped targets. A namespaced Deployment avoids all three.
func resolveDeploymentRef(ctx context.Context, client kubernetes.Interface, namespace, name string) (corev1.ObjectReference, error) {
	d, err := client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return corev1.ObjectReference{}, err
	}
	return corev1.ObjectReference{
		APIVersion:      "apps/v1",
		Kind:            "Deployment",
		Name:            d.Name,
		Namespace:       d.Namespace,
		UID:             d.UID,
		ResourceVersion: d.ResourceVersion,
	}, nil
}

// stateEvent describes a state transition.
func stateEvent(ref corev1.ObjectReference, from, to State, snap Snapshot) event.Info {
	eventType := corev1.EventTypeWarning
	reason := reasonLicenseWarning
	if to.IsHealthy() {
		eventType = corev1.EventTypeNormal
		reason = reasonLicenseStatus
	}

	message := fmt.Sprintf("Nirmata license state changed from %s to %s: %s.", from, to, snap.Reason)
	if snap.DaysRemaining > 0 {
		message += fmt.Sprintf(" %d days remaining.", snap.DaysRemaining)
	}

	return event.Info{
		Regarding: ref,
		Reason:    reason,
		Message:   message,
		Action:    event.None,
		Source:    sourceLicense,
		Type:      eventType,
	}
}

// limitEvent describes a capacity overrun.
func limitEvent(ref corev1.ObjectReference, resource string, used int, cap Cap, state State) event.Info {
	msg := fmt.Sprintf(
		"Nirmata licensed %s limit exceeded: %d in use, %s permitted (state=%s).",
		resource, used, cap, state)
	if state.EnforcesPolicyCap() {
		msg += " New over-cap policy creates will be refused; existing policies continue to enforce."
	} else {
		msg += " Full function is preserved during warning/grace; contact Nirmata to extend the license."
	}
	return event.Info{
		Regarding: ref,
		Reason:    reasonLicenseLimit,
		Message:   msg,
		Action:    event.None,
		Source:    sourceLicense,
		Type:      corev1.EventTypeWarning,
	}
}

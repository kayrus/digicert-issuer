// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and sapcc contributors
// SPDX-License-Identifier: Apache-2.0

/*
Copyright 2022 SAP SE.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package certmanager

import (
	"context"
	"fmt"
	"strings"
	"time"

	apiutil "github.com/cert-manager/cert-manager/pkg/api/util"
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"github.com/go-logr/logr"
	certmanagerv1beta1 "github.com/sapcc/digicert-issuer/apis/certmanager/v1beta1"
	"github.com/sapcc/digicert-issuer/pkg/k8sutils"
	"github.com/sapcc/digicert-issuer/pkg/provisioners"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// CertificateRequestReconciler reconciles a DigicertIssuer object.
type CertificateRequestReconciler struct {
	client.Client
	log                                logr.Logger
	BackoffDurationProvisionerNotReady time.Duration
	BackoffDurationRequestPending      time.Duration
	recorder                           record.EventRecorder
	DefaultProviderNamespace           string
	DisableRootCA                      bool
}

// SetupWithManager initializes the CertificateRequest controller into the
// controller runtime.
func (r *CertificateRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	filter := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			cr := e.ObjectNew.(*cmapi.CertificateRequest)
			return !isCertificateRequestStatusTrue(cr) ||
				!isCertificateRequestIssued(cr) ||
				len(cr.Status.Certificate) == 0
		},
	}

	r.recorder = mgr.GetEventRecorderFor("certificateRequestController")
	r.log = mgr.GetLogger().WithName("controllers").WithName("CertificateRequest")
	r.Client = mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).
		For(&cmapi.CertificateRequest{}).
		WithEventFilter(filter).
		Complete(r)
}

// +kubebuilder:rbac:groups=cert-manager.io,resources=certificaterequests,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificaterequests/status,verbs=get;update;patch

// Reconcile will read and validate a DigicertIssuer resource associated to the
// CertificateRequest resource, and it will sign the CertificateRequest with the
// provisioner in the DigicertIssuer.
func (r *CertificateRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.log.WithValues("certificaterequest", req.NamespacedName)

	// Fetch the CertificateRequest resource being reconciled.
	// Just ignore the request if the certificate request has been deleted.
	curCR := new(cmapi.CertificateRequest)
	if err := r.Client.Get(ctx, req.NamespacedName, curCR); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to retrieve CertificateRequest resource")
		return ctrl.Result{}, err
	}
	cr := curCR.DeepCopy()

	// Check the CertificateRequest's issuerRef and if it does not match the api
	// group name, log a message at a debug level and stop processing.
	if cr.Spec.IssuerRef.Group != certmanagerv1beta1.GroupVersion.Group {
		log.V(4).Info("resource does not specify an issuerRef group name that we are responsible for", "group", cr.Spec.IssuerRef.Group)
		return ctrl.Result{}, nil
	}

	// If the certificate data is already set then we skip this request as it
	// has already been completed in the past.
	if len(cr.Status.Certificate) > 0 {
		log.V(4).Info("existing certificate data found in status, skipping already completed CertificateRequest")
		return ctrl.Result{}, nil
	}

	var (
		iss              k8sutils.Issuer
		issNamespaceName types.NamespacedName
	)
	if strings.EqualFold(cr.Spec.IssuerRef.Kind, certmanagerv1beta1.ClusterDigicertIssuerKind) {
		iss = k8sutils.NewClusterDigicertIssuer()
		issNamespaceName = types.NamespacedName{
			Name: cr.Spec.IssuerRef.Name,
		}
	} else {
		iss = k8sutils.NewDigicertIssuer()
		issNamespaceName = types.NamespacedName{
			Namespace: req.Namespace,
			Name:      cr.Spec.IssuerRef.Name,
		}
	}

	if err := iss.Get(ctx, r.Client, issNamespaceName); err != nil {
		log.V(4).Info("Failed to retrieve DigicertIssuer resource, falling back to default namespace", "kind", iss.Kind(), "namespace", req.Namespace, "name", cr.Spec.IssuerRef.Name)
		issNamespaceName.Namespace = r.DefaultProviderNamespace
		err = iss.Get(ctx, r.Client, issNamespaceName)
		if err != nil {
			log.Error(err, "No DigicertIssuer resource found", "kind", iss.Kind(), "namespace", r.DefaultProviderNamespace, "name", cr.Spec.IssuerRef.Name)
			_ = r.setStatus(ctx, cr, curCR, cmmeta.ConditionFalse, cmapi.CertificateRequestReasonPending, "Failed to retrieve %s resource %s: %v", iss.Kind(), issNamespaceName, err)
			metricIssuerNotReady.WithLabelValues(issNamespaceName.String(), "issuer not found").Inc()
			return ctrl.Result{}, err
		}
	}

	if !isDigicertIssuerReady(iss) {
		log.Info("issuer is not ready", "name", issNamespaceName.String())
		metricIssuerNotReady.WithLabelValues(issNamespaceName.String(), "issuer not ready").Inc()
		if err := r.setStatus(ctx, cr, curCR, cmmeta.ConditionFalse, cmapi.CertificateRequestReasonPending, "%s resource %s is not Ready", iss.Kind(), issNamespaceName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.BackoffDurationProvisionerNotReady}, nil
	}

	// Load the provisioner that will sign the CertificateRequest.
	provisioner, ok := provisioners.Load(issNamespaceName)
	if !ok {
		log.Info("provisioner not found", "name", issNamespaceName)
		metricIssuerNotReady.WithLabelValues(issNamespaceName.String(), "provisioner not found").Inc()
		if err := r.setStatus(ctx, cr, curCR, cmmeta.ConditionFalse, cmapi.CertificateRequestReasonPending, "Failed to load provisioner for %s resource %s", iss.Kind(), issNamespaceName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.BackoffDurationProvisionerNotReady}, nil
	}

	// Download pending certificate
	if isCertificateRequestPending(cr) {
		log.V(4).Info("CertificateRequest is in pending state, trying to download certificate.", "name", cr.ObjectMeta.Name)
		caPEM, certPEM, err := provisioner.Download(ctx, cr)

		if err != nil || len(certPEM) < 1 {
			log.V(4).Info("Download of pending certificate failed, reqeueing.", "name", cr.ObjectMeta.Name)
			_ = r.setStatus(ctx, cr, curCR, cmmeta.ConditionFalse, cmapi.CertificateRequestReasonPending, "Certificate request pending")
			metricRequestsPending.WithLabelValues(cr.ObjectMeta.Name,
				cr.ObjectMeta.GetAnnotations()["cert-manager.io/certificate-name"],
				cr.ObjectMeta.GetAnnotations()["cert-manager.io/private-key-secret-name"],
				cr.ObjectMeta.GetAnnotations()["certmanager.cloud.sap/digicert-order-id"],
			).Inc()

			return ctrl.Result{Requeue: true, RequeueAfter: r.BackoffDurationRequestPending}, err
		}

		if len(caPEM) > 0 && !r.DisableRootCA {
			cr.Status.CA = caPEM
		}
		cr.Status.Certificate = certPEM
		err = r.setStatus(ctx, cr, curCR, cmmeta.ConditionTrue, cmapi.CertificateRequestReasonIssued, "Certificate issued")

		return ctrl.Result{}, err
	}

	// Sign CertificateRequest.
	caPEM, certPEM, order, err := provisioner.Sign(ctx, cr)
	if err != nil {
		log.Error(err, "failed to sign certificate request")
		metricRequestErrors.WithLabelValues(
			cr.ObjectMeta.Name,
			cr.ObjectMeta.GetAnnotations()["cert-manager.io/certificate-name"],
			cr.ObjectMeta.GetAnnotations()["cert-manager.io/private-key-secret-name"],
			"Failed to sign certificate request",
		).Inc()
		return ctrl.Result{}, r.setStatus(ctx, cr, curCR, cmmeta.ConditionFalse, cmapi.CertificateRequestReasonFailed, "Failed to sign certificate request: %v", err)
	}

	// Patch annotations.
	annotations := cr.ObjectMeta.GetAnnotations()
	annotations["certmanager.cloud.sap/digicert-issuer"] = "true"
	if order.ID > 0 {
		annotations["certmanager.cloud.sap/digicert-order-id"] = fmt.Sprintf("%d", order.ID)
	}
	if order.CertificateID > 0 {
		annotations["certmanager.cloud.sap/digicert-cert-id"] = fmt.Sprintf("%d", order.CertificateID)
	}
	cr.ObjectMeta.SetAnnotations(annotations)
	if err := r.Client.Patch(ctx, cr, client.MergeFrom(curCR)); err != nil {
		log.Error(err, "failed to update certificate request annotations")
		return ctrl.Result{}, err
	}

	// Update CertificateRequest status
	if len(certPEM) > 0 {
		if len(caPEM) > 0 && !r.DisableRootCA {
			cr.Status.CA = caPEM
		}
		cr.Status.Certificate = certPEM
		err = r.setStatus(ctx, cr, curCR, cmmeta.ConditionTrue, cmapi.CertificateRequestReasonIssued, "Certificate issued")
	} else if order.CertificateID > 0 {
		err = r.setStatus(ctx, cr, curCR, cmmeta.ConditionFalse, cmapi.CertificateRequestReasonPending, "Certificate request pending")
		return ctrl.Result{Requeue: true, RequeueAfter: r.BackoffDurationProvisionerNotReady}, err
	} else {
		metricRequestErrors.WithLabelValues(
			cr.ObjectMeta.Name,
			cr.ObjectMeta.GetAnnotations()["cert-manager.io/certificate-name"],
			cr.ObjectMeta.GetAnnotations()["cert-manager.io/private-key-secret-name"],
			"Certificate request failed",
		).Inc()
		err = r.setStatus(ctx, cr, curCR, cmmeta.ConditionUnknown, cmapi.CertificateRequestReasonFailed, "Certificate request failed")
	}

	// return err from the "r.setStatus" call
	return ctrl.Result{}, err
}

func isDigicertIssuerReady(issuer k8sutils.Issuer) bool {
	status := issuer.Status()
	if status == nil {
		return false
	}

	for _, condition := range status.Conditions {
		if condition.Type == certmanagerv1beta1.ConditionReady && condition.Status == certmanagerv1beta1.ConditionTrue {
			return true
		}
	}

	return false
}

func isCertificateRequestPending(cr *cmapi.CertificateRequest) bool {
	status := cr.Status
	// this is a hack that allows digicert-issuer to distinguish fake ACME pending requests
	digicertAcquired := cr.ObjectMeta.GetAnnotations()["certmanager.cloud.sap/digicert-issuer"] == "true"

	for _, condition := range status.Conditions {
		if condition.Reason == "Pending" && digicertAcquired {
			return true
		}
	}

	return false
}

func isCertificateRequestIssued(cr *cmapi.CertificateRequest) bool {
	status := cr.Status

	for _, condition := range status.Conditions {
		if condition.Reason == "Issued" {
			return true
		}
	}

	return false
}

func isCertificateRequestStatusTrue(cr *cmapi.CertificateRequest) bool {
	status := cr.Status
	for _, condition := range status.Conditions {
		if condition.Status == "True" {
			return true
		}
	}
	return false
}

func (r *CertificateRequestReconciler) setStatus(ctx context.Context, cr, curCR *cmapi.CertificateRequest, status cmmeta.ConditionStatus, reason, message string, args ...interface{}) error {
	completeMessage := fmt.Sprintf(message, args...)
	apiutil.SetCertificateRequestCondition(cr, cmapi.CertificateRequestConditionReady, status, reason, completeMessage)

	// Fire an Event to additionally inform users of the change
	eventType := core.EventTypeNormal
	if status == cmmeta.ConditionFalse {
		eventType = core.EventTypeWarning
	}
	r.recorder.Event(cr, eventType, reason, completeMessage)

	return r.Client.Status().Patch(ctx, cr, client.MergeFrom(curCR))
}

package csrapprover

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"reflect"
	"strings"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/certificate/csr"
	"k8s.io/klog/v2"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	ControllerName = "ovnkube-csr-approver-controller"
	NamePrefix     = "system:ovn-node"
	MaxDuration    = time.Hour * 24 * 365
)

var (
	Organization = []string{"system:ovn-nodes"}
	Groups       = sets.New[string]("system:nodes", "system:ovn-nodes", "system:authenticated")
	UserPrefixes = sets.New[string]("system:node", NamePrefix)
	Usages       = sets.New[certificatesv1.KeyUsage](
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageClientAuth)
)

// OVNKubeCSRController approves certificate signing requests (CSRs) by applying the following conditions:
// - CSRs with a CommonName that does not start with "commonNamePrefix" are ignored
// - CSRs with .Spec.SignerName not equal to kubernetes.io/kube-apiserver-client are ignored
// - CSRs .Spec.Username has a format of <prefix>:<nodeName> where <prefix> must exist in "userPrefixes"
// - The node name extracted from .Spec.Username is a valid DNS subdomain
// - The .Spec.Usages in the CSR matches the "usages" value in the controller
// - All elements in .Spec.Groups in the CSR exist in the "groups"
// - The .Spec.ExpirationSeconds is set and is not higher than "maxDuration"
// - The parsed CSR in .Spec.Request has a .Subject.Organization equal to "organization"
// - The parsed CSR in .Spec.Request has a .Subject.CommonName in the format of "<commonNamePrefix>:<nodeName>",
// where the nodeName value is extracted from .Spec.Username.
type OVNKubeCSRController struct {
	name               string
	commonNamePrefixes string
	organization       []string
	groups             sets.Set[string]
	userPrefixes       sets.Set[string]
	usages             sets.Set[certificatesv1.KeyUsage]
	maxDuration        time.Duration

	client   crclient.Client
	recorder record.EventRecorder
}

var Predicate = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool {
		return true
	},
	UpdateFunc: func(e event.UpdateEvent) bool {
		return true
	},
	DeleteFunc: func(e event.DeleteEvent) bool {
		// Ignore delete events
		return false
	},
}

// NewController creates a new OVNKubeCSRController
func NewController(client crclient.Client,
	commonNamePrefixes string,
	organization []string,
	groups, userPrefixes sets.Set[string],
	usages sets.Set[certificatesv1.KeyUsage],
	maxDuration time.Duration,
	recorder record.EventRecorder) *OVNKubeCSRController {
	c := &OVNKubeCSRController{
		name:               ControllerName,
		client:             client,
		commonNamePrefixes: commonNamePrefixes,
		organization:       organization,
		groups:             groups,
		usages:             usages,
		userPrefixes:       userPrefixes,
		maxDuration:        maxDuration,
		recorder:           recorder,
	}
	return c
}

func (c *OVNKubeCSRController) filterCSR(csr *certificatesv1.CertificateSigningRequest) bool {
	nsn := types.NamespacedName{Namespace: csr.Namespace, Name: csr.Name}
	csrPEM, _ := pem.Decode(csr.Spec.Request)
	if csrPEM == nil {
		klog.Errorf("Failed to PEM-parse the CSR block in .spec.request: no CSRs were found in %s", nsn)
		return false
	}

	x509CSR, err := x509.ParseCertificateRequest(csrPEM.Bytes)
	if err != nil {
		klog.Errorf("Failed to parse the CSR .spec.request of %q: %v", nsn, err)
		return false
	}

	return strings.HasPrefix(x509CSR.Subject.CommonName, c.commonNamePrefixes) &&
		csr.Spec.SignerName == certificatesv1.KubeAPIServerClientSignerName
}

func (c *OVNKubeCSRController) denyCSR(ctx context.Context, csr *certificatesv1.CertificateSigningRequest, message string) error {
	csr.Status.Conditions = append(csr.Status.Conditions,
		certificatesv1.CertificateSigningRequestCondition{
			Type:    certificatesv1.CertificateDenied,
			Status:  corev1.ConditionTrue,
			Reason:  "CSRDenied",
			Message: message,
		},
	)

	c.recorder.Eventf(&corev1.ObjectReference{
		Kind: "CertificateSigningRequest",
		Name: csr.Name,
	}, corev1.EventTypeWarning, "CSRDenied", "The CSR %q has been denied: %s", csr.Name, message)
	return c.client.SubResource("approval").Update(ctx, csr)
}

func (c *OVNKubeCSRController) approveCSR(ctx context.Context, csr *certificatesv1.CertificateSigningRequest) error {
	csr.Status.Conditions = append(csr.Status.Conditions,
		certificatesv1.CertificateSigningRequestCondition{
			Type:    certificatesv1.CertificateApproved,
			Status:  corev1.ConditionTrue,
			Reason:  "AutoApproved",
			Message: fmt.Sprintf("Auto-approved CSR %q", csr.Name),
		})

	c.recorder.Eventf(&corev1.ObjectReference{
		Kind: "CertificateSigningRequest",
		Name: csr.Name,
	}, corev1.EventTypeNormal, "CSRApproved", "CSR %q has been approved", csr.Name)
	return c.client.SubResource("approval").Update(ctx, csr)
}

func isApprovedOrDenied(status *certificatesv1.CertificateSigningRequestStatus) bool {
	for _, c := range status.Conditions {
		if c.Type == certificatesv1.CertificateApproved || c.Type == certificatesv1.CertificateDenied {
			return true
		}
	}
	return false
}

func (c *OVNKubeCSRController) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	startTime := time.Now()

	req := &certificatesv1.CertificateSigningRequest{}
	err := c.client.Get(ctx, request.NamespacedName, req)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if !c.filterCSR(req) {
		klog.V(5).Infof("Ignoring CSR %s", req.Name)
		return reconcile.Result{}, nil
	}
	var nodeName = "unknown"
	defer func() {
		klog.Infof("Finished syncing CSR %s for %s node in %v", request.Name, nodeName, time.Since(startTime))
	}()

	if len(req.Status.Certificate) > 0 {
		klog.V(5).Infof("CSR %s is already signed", req.Name)
		return reconcile.Result{}, nil
	}

	if isApprovedOrDenied(&req.Status) {
		klog.V(5).Infof("CSR %s is already approved/denied", req.Name)
		return reconcile.Result{}, nil
	}

	csrPEM, _ := pem.Decode(req.Spec.Request)
	if csrPEM == nil {
		return reconcile.Result{}, fmt.Errorf("failed to PEM-parse the CSR block in .spec.request: no CSRs were found")
	}

	x509CSR, err := x509.ParseCertificateRequest(csrPEM.Bytes)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to parse the CSR bytes: %v", err)
	}

	// expected format: userPrefix:nodeName
	// example: system:ovn-node:ovn-worker2
	i := strings.LastIndex(req.Spec.Username, ":")
	if i == -1 || i == len(req.Spec.Username)-1 {
		return reconcile.Result{}, fmt.Errorf("failed to parse the username: %s", req.Spec.Username)
	}

	prefix := req.Spec.Username[:i]
	nodeName = req.Spec.Username[i+1:]
	if !c.userPrefixes.Has(prefix) {
		return reconcile.Result{}, c.denyCSR(ctx, req, fmt.Sprintf("CSR %q was created by an unexpected user: %q", req.Name, req.Spec.Username))
	}

	if errs := validation.IsDNS1123Subdomain(nodeName); len(errs) != 0 {
		return reconcile.Result{}, c.denyCSR(ctx, req, fmt.Sprintf("extracted node name %q is not a valid DNS subdomain %v", nodeName, errs))
	}

	if usages := sets.New[certificatesv1.KeyUsage](req.Spec.Usages...); !usages.Equal(c.usages) {
		return reconcile.Result{}, c.denyCSR(ctx, req, fmt.Sprintf("CSR %q was created with unexpected usages: %v", req.Name, usages.UnsortedList()))
	}

	if !c.groups.HasAll(req.Spec.Groups...) {
		return reconcile.Result{}, c.denyCSR(ctx, req, fmt.Sprintf("CSR %q was created by a user with unexpected groups: %v", req.Name, req.Spec.Groups))
	}

	expectedSubject := fmt.Sprintf("%s:%s", c.commonNamePrefixes, nodeName)
	if x509CSR.Subject.CommonName != expectedSubject {
		return reconcile.Result{}, c.denyCSR(ctx, req, fmt.Sprintf("expected the CSR's commonName to be %q, but it is %q", expectedSubject, x509CSR.Subject.CommonName))
	}

	if !reflect.DeepEqual(x509CSR.Subject.Organization, c.organization) {
		return reconcile.Result{}, c.denyCSR(ctx, req, fmt.Sprintf("expected the CSR's organization to be %v, but it is %v", c.organization, x509CSR.Subject.Organization))
	}

	if req.Spec.ExpirationSeconds == nil {
		return reconcile.Result{}, c.denyCSR(ctx, req, fmt.Sprintf("CSR %q was created without specyfying the expirationSeconds", req.Name))
	}

	if csr.ExpirationSecondsToDuration(*req.Spec.ExpirationSeconds) > c.maxDuration {
		return reconcile.Result{}, c.denyCSR(ctx, req, fmt.Sprintf("CSR %q was created with invalid expirationSeconds value: %d", req.Name, *req.Spec.ExpirationSeconds))
	}

	return reconcile.Result{}, c.approveCSR(ctx, req)
}

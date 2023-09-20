package csrapprover

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// CSRAcceptanceCondition specifies conditions which CSRs are approved by csrapprover.
// csrapprover will check these condition and decide to approve by following rules:
// - CSRs with a CommonName that does not start with "CommonNamePrefix" are ignored
// - CSRs with .Spec.SignerName not equal to kubernetes.io/kube-apiserver-client are ignored
// - CSRs .Spec.Username has a format of <prefix>:<nodeName> where <prefix> must exist in "UserPrefixes"
// - The node name extracted from .Spec.Username is a valid DNS subdomain
// - The .Spec.Usages in the CSR matches the "usages" value in the controller
// - All elements in .Spec.Groups in the CSR exist in the "Groups"
// - The .Spec.ExpirationSeconds is set and is not higher than "maxDuration"
// - The parsed CSR in .Spec.Request has a .Subject.Organization equal to "organization"
// - The parsed CSR in .Spec.Request has a .Subject.CommonName in the format of "<commonNamePrefix>:<nodeName>",
// where the nodeName value is extracted from .Spec.Username.
type CSRAcceptanceCondition struct {
	// CommonNamePrefix specifies common name in target CSRs
	CommonNamePrefix string `json:"commonNamePrefix"`
	// Organization specifies Organization in target CSRs
	Organizations []string `json:"organizations"`
	// Groups specifies groups in target CSRs
	Groups []string `json:"groups"`
	// UserPrefixes specifies prefix of user field in target CSRs
	UserPrefixes []string `json:"userPrefixes"`
	// Default should be true if the target CSR is for ovn-node
	Default bool
	// groupsSet contains Groups value as sets.Set[]
	groupsSet sets.Set[string]
	// userPrefixesSet contains UserPrefixes value as sets.Set[]
	userPrefixesSet sets.Set[string]
}

// InitCSRAcceptanceConditions initializes CSRAcceptanceCondition: Load json from fileName and
// add default CSRAcceptanceCondition
func InitCSRAcceptanceConditions(fileName string) (conditions []CSRAcceptanceCondition, err error) {
	if fileName != "" {
		file, err := os.ReadFile(fileName)
		if err != nil {
			return nil, err
		}

		if err = json.Unmarshal(file, &conditions); err != nil {
			return nil, err
		}
	}

	conditions = append([]CSRAcceptanceCondition{DefaultCSRAcceptanceCondition}, conditions...)
	// initialize Sets from slices
	for i, v := range conditions {
		conditions[i].groupsSet = sets.New[string](v.Groups...)
		conditions[i].userPrefixesSet = sets.New[string](v.UserPrefixes...)
	}

	return conditions, nil
}

var (
	DefaultCSRAcceptanceCondition = CSRAcceptanceCondition{
		CommonNamePrefix: NamePrefix,
		Organizations:    []string{"system:ovn-nodes"},
		Groups:           []string{"system:nodes", "system:ovn-nodes", "system:authenticated"},
		UserPrefixes:     []string{"system:node", NamePrefix},
		Default:          true,
	}
	Usages = sets.New[certificatesv1.KeyUsage](
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageClientAuth)
)

// OVNKubeCSRController approves certificate signing requests (CSRs) by applying the conditions, which is defined
// in CSRAcceptanceCondition.
type OVNKubeCSRController struct {
	name                    string
	csrAcceptanceConditions []CSRAcceptanceCondition
	maxDuration             time.Duration
	usages                  sets.Set[certificatesv1.KeyUsage]

	commonNamePrefixes []string
	client             crclient.Client
	recorder           record.EventRecorder
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
	csrAcceptanceConditions []CSRAcceptanceCondition,
	usages sets.Set[certificatesv1.KeyUsage],
	maxDuration time.Duration,
	recorder record.EventRecorder) *OVNKubeCSRController {

	commonNamePrefixes := []string{}

	// create commonNamePrefixes from csrAcceptanceConditions
	for _, v := range csrAcceptanceConditions {
		commonNamePrefixes = append(commonNamePrefixes, v.CommonNamePrefix)
	}

	return &OVNKubeCSRController{
		name:                    ControllerName,
		client:                  client,
		csrAcceptanceConditions: csrAcceptanceConditions,
		commonNamePrefixes:      commonNamePrefixes,
		usages:                  usages,
		maxDuration:             maxDuration,
		recorder:                recorder,
	}
}

func (c *OVNKubeCSRController) filterCSR(csr *certificatesv1.CertificateSigningRequest, x509CSR *x509.CertificateRequest) bool {
	for _, v := range c.commonNamePrefixes {
		if strings.HasPrefix(x509CSR.Subject.CommonName, v) {
			return csr.Spec.SignerName == certificatesv1.KubeAPIServerClientSignerName
		}
	}
	return false
}

func (c *OVNKubeCSRController) denyCSR(ctx context.Context, csr *certificatesv1.CertificateSigningRequest, message error) error {
	csr.Status.Conditions = append(csr.Status.Conditions,
		certificatesv1.CertificateSigningRequestCondition{
			Type:    certificatesv1.CertificateDenied,
			Status:  corev1.ConditionTrue,
			Reason:  "CSRDenied",
			Message: message.Error(),
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

	csrPEM, _ := pem.Decode(req.Spec.Request)
	if csrPEM == nil {
		return reconcile.Result{}, fmt.Errorf("failed to decode %q PEM block in .spec.request: no CSRs were found", request.Name)
	}

	x509CSR, err := x509.ParseCertificateRequest(csrPEM.Bytes)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to parse %s PEM bytes: %v", request.Name, err)
	}

	if !c.filterCSR(req, x509CSR) {
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

	// expected common name format: userPrefix:nodeName
	// example: system:ovn-node:ovn-worker2
	i := strings.LastIndex(x509CSR.Subject.CommonName, ":")
	if i == -1 || i == len(x509CSR.Subject.CommonName)-1 {
		return reconcile.Result{}, fmt.Errorf("failed to parse the common name: %s", x509CSR.Subject.CommonName)
	}

	matched := false
	prefix := x509CSR.Subject.CommonName[:i]
	nodeName = x509CSR.Subject.CommonName[i+1:]
	for _, v := range c.csrAcceptanceConditions {
		if prefix == v.CommonNamePrefix {
			matched = true
			if err := v.validateCSR(req, x509CSR, c.usages); err != nil {
				return reconcile.Result{}, c.denyCSR(ctx, req, err)
			}
		}
	}

	if !matched {
		return reconcile.Result{}, c.denyCSR(ctx, req, fmt.Errorf("CSR %q was created with unexpected common name: %q", req.Name, x509CSR.Subject.CommonName))
	}

	if req.Spec.ExpirationSeconds == nil {
		return reconcile.Result{}, c.denyCSR(ctx, req, fmt.Errorf("CSR %q was created without specyfying the expirationSeconds", req.Name))
	}

	if csr.ExpirationSecondsToDuration(*req.Spec.ExpirationSeconds) > c.maxDuration {
		return reconcile.Result{}, c.denyCSR(ctx, req, fmt.Errorf("CSR %q was created with invalid expirationSeconds value: %d", req.Name, *req.Spec.ExpirationSeconds))
	}

	return reconcile.Result{}, c.approveCSR(ctx, req)
}

func (c *CSRAcceptanceCondition) validateCSR(req *certificatesv1.CertificateSigningRequest, x509CSR *x509.CertificateRequest, acceptUsages sets.Set[certificatesv1.KeyUsage]) error {

	// expected username format: userPrefix:nodeName
	// example: system:ovn-node:ovn-worker2
	i := strings.LastIndex(req.Spec.Username, ":")
	if i == -1 || i == len(req.Spec.Username)-1 {
		return fmt.Errorf("failed to parse the username: %s", req.Spec.Username)
	}
	prefix := req.Spec.Username[:i]
	nodeName := req.Spec.Username[i+1:]
	if !c.userPrefixesSet.Has(prefix) {
		return fmt.Errorf("CSR %q was created by an unexpected user: %q", req.Name, req.Spec.Username)
	}

	if errs := validation.IsDNS1123Subdomain(nodeName); len(errs) != 0 {
		return fmt.Errorf("extracted node name %q is not a valid DNS subdomain %v", nodeName, errs)
	}

	if usages := sets.New[certificatesv1.KeyUsage](req.Spec.Usages...); !usages.Equal(acceptUsages) {
		return fmt.Errorf("CSR %q was created with unexpected usages: %v", req.Name, usages.UnsortedList())
	}

	if !c.groupsSet.HasAll(req.Spec.Groups...) {
		return fmt.Errorf("CSR %q was created by a user with unexpected groups: %v", req.Name, req.Spec.Groups)
	}

	expectedSubject := fmt.Sprintf("%s:%s", c.CommonNamePrefix, nodeName)
	if x509CSR.Subject.CommonName != expectedSubject {
		return fmt.Errorf("expected the CSR's commonName to be %q, but it is %q", expectedSubject, x509CSR.Subject.CommonName)
	}

	if !reflect.DeepEqual(x509CSR.Subject.Organization, c.Organizations) {
		return fmt.Errorf("expected the CSR's organization to be %v, but it is %v", c.Organizations, x509CSR.Subject.Organization)
	}
	return nil
}

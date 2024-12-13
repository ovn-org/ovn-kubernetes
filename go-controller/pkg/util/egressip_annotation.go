package util

import (
	"fmt"
	"strconv"
)

const (
	EgressIPMarkAnnotation = "k8s.ovn.org/egressip-mark"
	EgressIPMarkBase       = 50000
	EgressIPMarkMax        = 55000
)

type EgressIPMark struct {
	strValue string
	intValue int
}

func (em EgressIPMark) String() string {
	return em.strValue
}

func (em EgressIPMark) ToInt() int {
	return em.intValue
}

func (em EgressIPMark) IsValid() bool {
	return IsEgressIPMarkValid(em.intValue)
}

func (em EgressIPMark) IsAvailable() bool {
	return em.strValue != ""
}

func ParseEgressIPMark(annotations map[string]string) (EgressIPMark, error) {
	eipMark := EgressIPMark{}
	if annotations == nil {
		return eipMark, fmt.Errorf("failed to parse EgressIP mark from annotation because annotation is nil")
	}
	markStr, ok := annotations[EgressIPMarkAnnotation]
	if !ok {
		return eipMark, nil
	}
	eipMark.strValue = markStr
	mark, err := strconv.Atoi(markStr)
	if err != nil {
		return eipMark, fmt.Errorf("failed to parse EgressIP mark annotation string %q to an integer", markStr)
	}
	eipMark.intValue = mark
	return eipMark, nil
}

func IsEgressIPMarkSet(annotations map[string]string) bool {
	if annotations == nil {
		return false
	}
	_, ok := annotations[EgressIPMarkAnnotation]
	return ok
}

func IsEgressIPMarkValid(mark int) bool {
	return mark >= EgressIPMarkBase && mark <= EgressIPMarkMax
}

// EgressIPMarkAnnotationChanged returns true if the EgressIP mark annotation changed
func EgressIPMarkAnnotationChanged(annotationA, annotationB map[string]string) bool {
	return annotationA[EgressIPMarkAnnotation] != annotationB[EgressIPMarkAnnotation]
}

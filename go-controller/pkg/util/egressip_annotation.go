package util

import (
	"fmt"
	"math"
	"strconv"
)

const (
	EgressIPMarkAnnotation = "k8s.ovn.org/egressip-mark"
	EgressIPMarkBase       = 50000
	EgressIPMarkMax        = math.MaxUint16
)

type EgressIPMark struct {
	s string
	i int
}

func (em EgressIPMark) ToString() string {
	return em.s
}

func (em EgressIPMark) ToInt() int {
	return em.i
}

func (em EgressIPMark) IsValid() bool {
	return IsEgressIPMarkValid(em.i)
}

func (em EgressIPMark) IsAvailable() bool {
	return em.s != ""
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
	eipMark.s = markStr
	mark, err := strconv.Atoi(markStr)
	if err != nil {
		return eipMark, fmt.Errorf("failed to parse EgressIP mark annotation string %q to an integer", markStr)
	}
	eipMark.i = mark
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
func EgressIPMarkAnnotationChanged(annotationA, AnnotationB map[string]string) bool {
	return annotationA[EgressIPMarkAnnotation] != AnnotationB[EgressIPMarkAnnotation]
}

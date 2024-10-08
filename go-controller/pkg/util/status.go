package util

import corev1 "k8s.io/api/core/v1"

type EventType = string

// There are only 2 allowed event types for now: Normal and Warning
const (
	EventTypeNormal  EventType = corev1.EventTypeNormal
	EventTypeWarning EventType = corev1.EventTypeWarning
)

// EventDetails may be used to pass event details to the event recorder, that is not used directly.
// It based on the EventRecorder interface for core.Events. It doesn't have related objects,
// as they are not used in the current implementation.
type EventDetails struct {
	EventType    EventType
	Reason, Note string
}

package unidling

import (
	"encoding/json"
	"errors"

	kapi "k8s.io/api/core/v1"
)

type eventRecord struct {
	Data     [][]interface{} `json:"Data"`
	Headings []string        `json:"Headings"`
}

type emptyLBBackendEvent struct {
	vip      string
	protocol kapi.Protocol
	uuid     string
}

func extractEmptyLBBackendsEvents(out []byte) ([]emptyLBBackendEvent, error) {
	events := make([]emptyLBBackendEvent, 0, 4)

	var f eventRecord
	err := json.Unmarshal(out, &f)
	if err != nil {
		return events, err
	}
	if len(f.Data) == 0 {
		return events, nil
	}

	var eventInfoIndex int
	var eventTypeIndex int
	var uuidIndex int
	for idx, val := range f.Headings {
		switch val {
		case "event_info":
			eventInfoIndex = idx
		case "event_type":
			eventTypeIndex = idx
		case "_uuid":
			uuidIndex = idx
		}
	}

	for _, val := range f.Data {
		if len(val) <= eventTypeIndex {
			return events, errors.New("Mismatched Data and Headings in controller event")
		}
		if val[eventTypeIndex] != "empty_lb_backends" {
			continue
		}

		uuidArray, ok := val[uuidIndex].([]interface{})
		if !ok {
			return events, errors.New("Unexpected '_uuid' data in controller event")
		}
		if len(uuidArray) < 2 {
			return events, errors.New("Malformed UUID presented in controller event")
		}
		uuid, ok := uuidArray[1].(string)
		if !ok {
			return events, errors.New("Failed to parse UUID in controller event")
		}

		// Unpack the data. There's probably a better way to do this.
		info, ok := val[eventInfoIndex].([]interface{})
		if !ok {
			return events, errors.New("Unexpected 'event_info' data in controller event")
		}
		if len(info) < 2 {
			return events, errors.New("Malformed event_info in controller event")
		}
		eventMap, ok := info[1].([]interface{})
		if !ok {
			return events, errors.New("'event_info' data is not the expected type")
		}

		var vip string
		var protocol kapi.Protocol
		for _, x := range eventMap {
			tuple, ok := x.([]interface{})
			if !ok {
				return events, errors.New("event map item failed to parse")
			}
			if len(tuple) < 2 {
				return events, errors.New("event map contains malformed data")
			}
			switch tuple[0] {
			case "vip":
				vip, ok = tuple[1].(string)
				if !ok {
					return events, errors.New("Failed to parse vip in controller event")
				}
			case "protocol":
				prot, ok := tuple[1].(string)
				if !ok {
					return events, errors.New("Failed to parse protocol in controller event")
				}
				if prot == "udp" {
					protocol = kapi.ProtocolUDP
				} else if prot == "sctp" {
					protocol = kapi.ProtocolSCTP
				} else {
					protocol = kapi.ProtocolTCP
				}
			}
		}
		events = append(events, emptyLBBackendEvent{vip, protocol, uuid})
	}

	return events, nil
}

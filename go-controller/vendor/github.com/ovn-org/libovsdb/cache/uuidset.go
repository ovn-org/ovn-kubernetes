package cache

type void struct{}
type uuidset map[string]void

func newUUIDSet(uuids ...string) uuidset {
	s := uuidset{}
	for _, uuid := range uuids {
		s[uuid] = void{}
	}
	return s
}

func (s uuidset) add(uuid string) {
	s[uuid] = void{}
}

func (s uuidset) remove(uuid string) {
	delete(s, uuid)
}

func (s uuidset) has(uuid string) bool {
	_, ok := s[uuid]
	return ok
}

func (s uuidset) equals(o uuidset) bool {
	if len(s) != len(o) {
		return false
	}
	for uuid := range s {
		if !o.has(uuid) {
			return false
		}
	}
	return true
}

func (s uuidset) getAny() string {
	for k := range s {
		return k
	}
	return ""
}

func (s uuidset) list() []string {
	uuids := make([]string, 0, len(s))
	for uuid := range s {
		uuids = append(uuids, uuid)
	}
	return uuids
}

func (s uuidset) empty() bool {
	return len(s) == 0
}

func addUUIDSet(s1, s2 uuidset) uuidset {
	if len(s2) == 0 {
		return s1
	}
	if s1 == nil {
		s1 = uuidset{}
	}
	for uuid := range s2 {
		s1.add(uuid)
	}
	return s1
}

func substractUUIDSet(s1, s2 uuidset) uuidset {
	if len(s1) == 0 || len(s2) == 0 {
		return s1
	}
	for uuid := range s2 {
		s1.remove(uuid)
	}
	return s1
}

func intersectUUIDSets(s1, s2 uuidset) uuidset {
	if len(s1) == 0 || len(s2) == 0 {
		return nil
	}
	var big uuidset
	var small uuidset
	if len(s1) > len(s2) {
		big = s1
		small = s2
	} else {
		big = s2
		small = s1
	}
	f := uuidset{}
	for uuid := range small {
		if big.has(uuid) {
			f.add(uuid)
		}
	}
	return f
}

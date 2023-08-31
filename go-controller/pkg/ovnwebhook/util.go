package ovnwebhook

type action int

const (
	removed action = iota
	changed
	added
)

type annotationChange struct {
	action action
	value  string
}

func mapDiff(old, new map[string]string) (changes map[string]annotationChange) {
	changes = make(map[string]annotationChange)

	for key, value := range old {
		v, found := new[key]
		if !found {
			changes[key] = annotationChange{
				action: removed,
				value:  value,
			}
			continue
		}
		if v != value {
			changes[key] = annotationChange{
				action: changed,
				value:  v, // On changes use the value of new
			}
		}
	}
	for key, value := range new {
		if _, found := old[key]; !found {
			changes[key] = annotationChange{
				action: added,
				value:  value,
			}
		}
	}
	return
}

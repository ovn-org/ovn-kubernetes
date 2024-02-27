package database

// References tracks the references to rows from other rows at specific
// locations in the schema.
type References map[ReferenceSpec]Reference

// ReferenceSpec specifies details about where in the schema a reference occurs.
type ReferenceSpec struct {
	// ToTable is the table of the row to which the reference is made
	ToTable string

	// FromTable is the table of the row from which the reference is made
	FromTable string

	// FromColumn is the column of the row from which the reference is made
	FromColumn string

	// FromValue flags if the reference is made on a map key or map value when
	// the column is a map
	FromValue bool
}

// Reference maps the UUIDs of rows to which the reference is made to the
// rows it is made from
type Reference map[string][]string

// GetReferences gets references to a row
func (rs References) GetReferences(table, uuid string) References {
	refs := References{}
	for spec, values := range rs {
		if spec.ToTable != table {
			continue
		}
		if _, ok := values[uuid]; ok {
			refs[spec] = Reference{uuid: values[uuid]}
		}
	}
	return refs
}

// UpdateReferences updates the references with the provided ones. Dangling
// references, that is, the references of rows that are no longer referenced
// from anywhere, are cleaned up.
func (rs References) UpdateReferences(other References) {
	for spec, otherRefs := range other {
		for to, from := range otherRefs {
			rs.updateReference(spec, to, from)
		}
	}
}

// updateReference updates the references to a row at a specific location in the
// schema
func (rs References) updateReference(spec ReferenceSpec, to string, from []string) {
	thisRefs, ok := rs[spec]
	if !ok && len(from) > 0 {
		// add references from a previously untracked location
		rs[spec] = Reference{to: from}
		return
	}
	if len(from) > 0 {
		// replace references to this row at this specific location
		thisRefs[to] = from
		return
	}
	// otherwise remove previously tracked references
	delete(thisRefs, to)
	if len(thisRefs) == 0 {
		delete(rs, spec)
	}
}

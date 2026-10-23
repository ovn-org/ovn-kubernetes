package modelgen

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"text/template"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// extendedGenTemplate include additional code generation that is optional, like
// deep copy methods.
var extendedGenTemplate = `
{{- define "deepCopyExtraFields" }}{{ end }}
{{- define "equalExtraFields" }}{{ end }}
{{- define "extendedGenImports" }}
{{- if index . "WithExtendedGen" }}
import "github.com/ovn-kubernetes/libovsdb/model"
{{- end }}
{{- end }}
{{- define "extendedGen" }}
{{- if index . "WithExtendedGen" }}
{{- $tableName := index . "TableName" }}
{{- $structName := index . "StructName" }}
{{- range $field := index . "Fields" }}
{{- $fieldName := FieldName $field.Column }}
{{- $type := "" }}
{{- if index $ "WithEnumTypes" }}
{{- $type = FieldTypeWithEnums $tableName $field.Column $field.Schema }}
{{- else }}
{{- $type = FieldType $tableName $field.Column $field.Schema }}
{{- end }}

func (a *{{ $structName }}) Get{{ $fieldName }}() {{ $type }} {
	return a.{{ $fieldName }}
}

{{ if or (eq (index $type 0) '*') (eq (slice $type 0 2) "[]") (eq (slice $type 0 3) "map") }}
func copy{{ $structName }}{{ $fieldName }}(a {{ $type }}) {{ $type }} {
	if a == nil {
		return nil
	}
	{{- if eq (index $type 0) '*' }}
	b := *a
	return &b
	{{- else if eq (slice $type 0 2) "[]" }}
	b := make({{ $type }}, len(a))
	copy(b, a)
	return b
	{{- else }}
	b := make({{ $type }}, len(a))
	for k, v := range a {
		b[k] = v
	}
	return b
	{{- end }}
}

func equal{{ $structName }}{{ $fieldName }}(a, b {{ $type }}) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	{{- if eq (index $type 0) '*' }}
	if a == b {
		return true
	}
	return *a == *b
	{{- else if eq (slice $type 0 2) "[]" }}
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if b[i] != v {
			return false
		}
	}
	return true
	{{- else }}
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
	{{- end }}
}

{{ end }}
{{ end }}

func (a *{{ $structName }}) DeepCopyInto(b *{{ $structName }}) {
	*b = *a
	{{- range $field := index . "Fields" }}
	{{- $fieldName := FieldName $field.Column }}
	{{- $type := "" }}
	{{- if index $ "WithEnumTypes" }}
	{{- $type = FieldTypeWithEnums $tableName $field.Column $field.Schema }}
	{{- else }}
	{{- $type = FieldType $tableName $field.Column $field.Schema }}
	{{- end }}
	{{- if or (eq (index $type 0) '*') (eq (slice $type 0 2) "[]") (eq (slice $type 0 3) "map") }}
	b.{{ $fieldName }} = copy{{ $structName }}{{ $fieldName }}(a.{{ $fieldName }})
	{{- end }}
	{{- end }}
	{{- template "deepCopyExtraFields" . }}
}

func (a *{{ $structName }}) DeepCopy() *{{ $structName }} {
	b := new({{ $structName }})
	a.DeepCopyInto(b)
	return b
}

func (a *{{ $structName }}) CloneModelInto(b model.Model) {
	c := b.(*{{ $structName }})
	a.DeepCopyInto(c)
}

func (a *{{ $structName }}) CloneModel() model.Model {
	return a.DeepCopy()
}

func (a *{{ $structName }}) Equals(b *{{ $structName }}) bool {
	{{- range $i, $field := index . "Fields" }}
	{{- $fieldName := FieldName $field.Column }}
	{{- $type := "" }}
	{{- if index $ "WithEnumTypes" }}
	{{- $type = FieldTypeWithEnums $tableName $field.Column $field.Schema }}
	{{- else }}
	{{- $type = FieldType $tableName $field.Column $field.Schema }}
	{{- end }}
	{{- if $i }}&&
	{{ else }}return {{ end }}
	{{- if or (eq (index $type 0) '*') (eq (slice $type 0 2) "[]") (eq (slice $type 0 3) "map") -}}
	equal{{ $structName }}{{ $fieldName }}(a.{{ $fieldName }}, b.{{ $fieldName }})
	{{- else -}}
	a.{{ $fieldName }} == b.{{ $fieldName }}
	{{- end }}
	{{- end }}
	{{- template "equalExtraFields" . }}
}

func (a *{{ $structName }}) EqualsModel(b model.Model) bool {
	c := b.(*{{ $structName }})
	return a.Equals(c)
}

var _ model.CloneableModel = &{{ $structName }}{}
var _ model.ComparableModel = &{{ $structName }}{}
{{- end }}
{{- end }}
`

// NewTableTemplate returns a new table template. It includes the following
// other templates that can be overridden to customize the generated file:
//
//   - `header`: override the comment as header before package definition
//   - `preStructDefinitions`: deprecated in favor of `extraImports`
//   - `extraImports`: include additional imports
//   - `structComment`: override the comment generated for the table
//   - `extraFields`: add extra fields to the table
//   - `extraTags`: add tags to the extra fields
//   - `deepCopyExtraFields`: copy extra fields when copying a table
//   - `equalExtraFields`: compare extra fields when comparing a table
//   - `postStructDefinitions`: deprecated in favor of `extraDefinitions`
//   - `extraDefinitions`: include additional definitions like functions etc.
//
// It is designed to be used with a map[string] interface and some defined keys
// (see GetTableTemplateData). In addition, the following functions can be used
// within the template:
//
//   - `PrintVal`: prints a field value
//   - `FieldName`: prints the name of a field based on its column
//   - `FieldType`: prints the field type based on its column and schema
//   - `FieldTypeWithEnums`: same as FieldType but with enum type expansion
//   - `OvsdbTag`: prints the ovsdb tag
//   - `ValidationTag`: generates the 'validate' struct tag based on OVSDB schema constraints
//   - `EnumAliasSuffix`: print enum type alias suffix based on column type
func NewTableTemplate() *template.Template {
	return template.Must(template.New("").Funcs(
		template.FuncMap{
			"PrintVal":           printVal,
			"FieldName":          FieldName,
			"FieldType":          FieldType,
			"FieldTypeWithEnums": FieldTypeWithEnums,
			"OvsdbTag":           Tag,
			"ValidationTag":      ValidationTag,
			"AtomicType":         AtomicType,
			"EnumAliasSuffix":    enumAliasSuffix,
		},
	).Parse(extendedGenTemplate + `
{{- define "header" }}
// Code generated by "libovsdb.modelgen"
// DO NOT EDIT.
{{- end }}
{{ define "extraImports" }}{{ end }}
{{ define "preStructDefinitions" }}{{ end }}
{{- define "structComment" }}
// {{ index . "StructName" }} defines an object in {{ index . "TableName" }} table
{{- end }}
{{- define "showTableName" }}
const {{ index . "StructName" }}Table = "{{ index . "TableName" }}"
{{- end }}
{{ define "extraTags" }}{{ end }}
{{ define "extraFields" }}{{ end }}
{{ define "extraDefinitions" }}{{ end }}
{{ define "postStructDefinitions" }}{{ end }}
{{ template "header" . }}
{{ define "enums" }}
{{ if index . "WithEnumTypes" }}
{{ if index . "Enums" }}
type (
{{ range index . "Enums" }}
{{ .Alias }} = {{ AtomicType .Type }}
{{- end }}
)

var (
{{ range  index . "Enums" }}
{{- $e := . }}
{{- range .Sets }}
{{ $e.Alias }}{{ EnumAliasSuffix . $e.Type }} {{ $e.Alias }} = {{ PrintVal . $e.Type }}
{{- end }}
{{- end }}
)
{{- end }}
{{- end }}
{{- end }}
package {{ index . "PackageName" }}
{{ template "extendedGenImports" . }}
{{ template "extraImports" . }}
{{ template "preStructDefinitions" . }}
{{ template "showTableName" . }}
{{ template "enums" . }}
{{ template "structComment" . }}
type {{ index . "StructName" }} struct {
{{- $tableName := index . "TableName" }}
{{ if index . "WithEnumTypes" }}
{{ range $field := index . "Fields" }}	{{ FieldName $field.Column }}  {{ FieldTypeWithEnums $tableName $field.Column $field.Schema }} ` + "`" + `{{ OvsdbTag $field.Column }}{{ ValidationTag $field.Schema }}{{ template "extraTags" . }}` + "`" + `
{{ end }}
{{ else }}
{{ range  $field := index . "Fields" }}	{{ FieldName $field.Column }}  {{ FieldType $tableName $field.Column $field.Schema }} ` + "`" + `{{ OvsdbTag $field.Column }}{{ ValidationTag $field.Schema }}{{ template "extraTags" . }}` + "`" + `
{{ end }}
{{ end }}
{{ template "extraFields" . }}
}
{{ template "postStructDefinitions" . }}
{{ template "extraDefinitions" . }}
{{ template "extendedGen" . }}
`))
}

// Enum represents the enum schema type
type Enum struct {
	Type  string
	Alias string
	Sets  []any
}

// Field represents the field information
type Field struct {
	Column string
	Schema *ovsdb.ColumnSchema
}

// TableTemplateData represents the data used by the Table Template
type TableTemplateData map[string]any

// WithEnumTypes configures whether the Template should expand enum types or not
// Enum expansion (true by default) makes the template define an type alias for each enum type
// and a const for each possible enum value
func (t TableTemplateData) WithEnumTypes(val bool) {
	t["WithEnumTypes"] = val
}

// WithExtendedGen configures whether the Template should generate code to deep
// copy models.
func (t TableTemplateData) WithExtendedGen(val bool) {
	t["WithExtendedGen"] = val
}

// GetTableTemplateData returns the TableTemplateData map. It has the following
// keys:
//
//   - `TableName`: (string) the table name
//   - `TPackageName`: (string) the package name
//   - `TStructName`: (string) the struct name
//   - `TFields`: []Field a list of Fields that the struct has
func GetTableTemplateData(pkg, name string, table *ovsdb.TableSchema) TableTemplateData {
	data := map[string]any{}
	data["TableName"] = name
	data["PackageName"] = pkg
	data["StructName"] = StructName(name)
	Fields := []Field{}
	Enums := []Enum{}

	// Map iteration order is random, so for predictable generation
	// lets sort fields by name
	var order sort.StringSlice
	for columnName := range table.Columns {
		order = append(order, columnName)
	}
	order.Sort()

	for _, columnName := range append([]string{"_uuid"}, order...) {
		columnSchema := table.Column(columnName)
		Fields = append(Fields, Field{
			Column: columnName,
			Schema: columnSchema,
		})
		if enum := FieldEnum(name, columnName, columnSchema); enum != nil {
			Enums = append(Enums, *enum)
		}
	}
	data["Fields"] = Fields
	data["Enums"] = Enums
	data["WithEnumTypes"] = true
	data["WithExtendedGen"] = false
	return data
}

// FieldName returns the name of a column field
func FieldName(column string) string {
	return camelCase(strings.Trim(column, "_"))
}

// StructName returns the name of the table struct
func StructName(tableName string) string {
	return cases.Title(language.Und, cases.NoLower).String(strings.ReplaceAll(tableName, "_", ""))
}

func fieldType(tableName, columnName string, column *ovsdb.ColumnSchema, enumTypes bool) string {
	switch column.Type {
	case ovsdb.TypeEnum:
		if enumTypes {
			return enumName(tableName, columnName)
		}
		return AtomicType(column.TypeObj.Key.Type)
	case ovsdb.TypeMap:
		return fmt.Sprintf("map[%s]%s", AtomicType(column.TypeObj.Key.Type),
			AtomicType(column.TypeObj.Value.Type))
	case ovsdb.TypeSet:
		// optional with max 1 element
		if column.TypeObj.Min() == 0 && column.TypeObj.Max() == 1 {
			if enumTypes && FieldEnum(tableName, columnName, column) != nil {
				return fmt.Sprintf("*%s", enumName(tableName, columnName))
			}
			return fmt.Sprintf("*%s", AtomicType(column.TypeObj.Key.Type))
		}
		// required, max 1 element
		if column.TypeObj.Min() == 1 && column.TypeObj.Max() == 1 {
			if enumTypes && FieldEnum(tableName, columnName, column) != nil {
				return enumName(tableName, columnName)
			}
			return AtomicType(column.TypeObj.Key.Type)
		}
		// use a slice
		if enumTypes && FieldEnum(tableName, columnName, column) != nil {
			return fmt.Sprintf("[]%s", enumName(tableName, columnName))
		}
		return fmt.Sprintf("[]%s", AtomicType(column.TypeObj.Key.Type))
	default:
		return AtomicType(column.Type)
	}
}

// EnumName returns the name of the enum field
func enumName(tableName, columnName string) string {
	return cases.Title(language.Und, cases.NoLower).String(StructName(tableName)) + camelCase(columnName)
}

// FieldType returns the string representation of a column type without enum types expansion
func FieldType(tableName, columnName string, column *ovsdb.ColumnSchema) string {
	return fieldType(tableName, columnName, column, false)
}

// FieldTypeWithEnums returns the string representation of a column type where Enums
// are expanded into their own types
func FieldTypeWithEnums(tableName, columnName string, column *ovsdb.ColumnSchema) string {
	return fieldType(tableName, columnName, column, true)
}

// FieldEnum returns the Enum if the column is an enum type
func FieldEnum(tableName, columnName string, column *ovsdb.ColumnSchema) *Enum {
	if column.TypeObj == nil || column.TypeObj.Key.Enum == nil {
		return nil
	}
	return &Enum{
		Type:  column.TypeObj.Key.Type,
		Alias: enumName(tableName, columnName),
		Sets:  column.TypeObj.Key.Enum,
	}
}

// AtomicType returns the string type of an AtomicType
func AtomicType(atype string) string {
	switch atype {
	case ovsdb.TypeInteger:
		return "int"
	case ovsdb.TypeReal:
		return "float64"
	case ovsdb.TypeBoolean:
		return "bool"
	case ovsdb.TypeString:
		return "string"
	case ovsdb.TypeUUID:
		return "string"
	}
	return ""
}

// Tag returns the Tag string of a column
func Tag(column string) string {
	return fmt.Sprintf(`ovsdb:"%s"`, column)
}

// getAtomicValidations generates validation tags for a single ovsdb.BaseType.
func getAtomicValidations(atomicSchema *ovsdb.BaseType) []string {
	if atomicSchema == nil {
		return nil
	}

	switch atomicSchema.Type {
	case ovsdb.TypeInteger:
		return getIntegerValidations(atomicSchema)
	case ovsdb.TypeReal:
		return getRealValidations(atomicSchema)
	case ovsdb.TypeString:
		return getStringValidations(atomicSchema)
	case ovsdb.TypeUUID:
		return getUUIDValidations(atomicSchema)
	case ovsdb.TypeBoolean:
		return getBooleanValidations(atomicSchema)
	default:
		return nil
	}
}

// getIntegerValidations generates validation tags for integer types.
func getIntegerValidations(atomicSchema *ovsdb.BaseType) []string {
	var validations []string

	if minVal, err := atomicSchema.MinInteger(); err == nil {
		if minVal != math.MinInt64 {
			validations = append(validations, fmt.Sprintf("min=%d", minVal))
		}
	}

	if maxVal, err := atomicSchema.MaxInteger(); err == nil {
		if maxVal != math.MaxInt64 {
			validations = append(validations, fmt.Sprintf("max=%d", maxVal))
		}
	}

	if len(atomicSchema.Enum) > 0 {
		var enumValues []string
		for _, val := range atomicSchema.Enum {
			enumValues = append(enumValues, fmt.Sprintf("%v", val))
		}
		validations = append(validations, "oneof="+strings.Join(enumValues, " "))
	}

	return validations
}

// getRealValidations generates validation tags for real (float) types.
func getRealValidations(atomicSchema *ovsdb.BaseType) []string {
	var validations []string

	if minVal, err := atomicSchema.MinReal(); err == nil {
		if !floatEqual(minVal, math.SmallestNonzeroFloat64) {
			validations = append(validations, fmt.Sprintf("min=%g", minVal))
		}
	}

	if maxVal, err := atomicSchema.MaxReal(); err == nil {
		if !floatEqual(maxVal, math.MaxFloat64) {
			validations = append(validations, fmt.Sprintf("max=%g", maxVal))
		}
	}

	if len(atomicSchema.Enum) > 0 {
		// github.com/go-playground/validator/v10 don't support oneof float64
		var eqParts []string
		for _, val := range atomicSchema.Enum {
			eqParts = append(eqParts, fmt.Sprintf("eq=%g", val))
		}
		validations = append(validations, strings.Join(eqParts, "|"))
	}

	return validations
}

// getStringValidations generates validation tags for string types.
func getStringValidations(atomicSchema *ovsdb.BaseType) []string {
	var validations []string

	if maxVal, err := atomicSchema.MaxLength(); err == nil {
		if maxVal != math.MaxInt32 && maxVal != math.MaxInt64 {
			validations = append(validations, fmt.Sprintf("max=%d", maxVal))
		}
	}

	if len(atomicSchema.Enum) > 0 {
		var enumValues []string
		for _, val := range atomicSchema.Enum {
			enumValues = append(enumValues, fmt.Sprintf("'%s'", val))
		}
		validations = append(validations, "oneof="+strings.Join(enumValues, " "))
	}

	return validations
}

// getUUIDValidations generates validation tags for UUID types.
func getUUIDValidations(atomicSchema *ovsdb.BaseType) []string {
	var validations []string

	// named-uuid, cannot validate in uuid
	if len(atomicSchema.Enum) > 0 {
		var enumValues []string
		for _, val := range atomicSchema.Enum {
			enumValues = append(enumValues, fmt.Sprintf("'%s'", val))
		}
		validations = append(validations, "oneof="+strings.Join(enumValues, " "))
	}

	return validations
}

// getBooleanValidations generates validation tags for boolean types.
func getBooleanValidations(atomicSchema *ovsdb.BaseType) []string {
	var validations []string

	if len(atomicSchema.Enum) > 0 {
		// github.com/go-playground/validator/v10 don't support oneof boolean
		var includeTrue, includeFalse bool
		for _, val := range atomicSchema.Enum {
			if val == true {
				includeTrue = true
			} else {
				includeFalse = true
			}
		}

		// If both true and false are allowed, no validation needed
		if includeTrue && includeFalse {
			return validations
		}

		if includeTrue {
			validations = append(validations, "eq=true")
		}
		if includeFalse {
			validations = append(validations, "eq=false")
		}
	}

	return validations
}

func getCollectionSizeValidations(typeObj *ovsdb.ColumnType) []string {
	var validations []string
	minCount := typeObj.Min()
	maxCount := typeObj.Max()
	if minCount > 0 {
		validations = append(validations, fmt.Sprintf("min=%d", minCount))
	}
	if maxCount != ovsdb.Unlimited {
		validations = append(validations, fmt.Sprintf("max=%d", maxCount))
	}
	return validations
}

func getSetValidations(schema *ovsdb.ColumnSchema) []string {
	var validations []string
	validations = append(validations, getCollectionSizeValidations(schema.TypeObj)...)
	if schema.TypeObj.Key != nil {
		elementValidations := getAtomicValidations(schema.TypeObj.Key)
		if len(elementValidations) > 0 {
			validations = append(validations, "dive")
			validations = append(validations, elementValidations...)
		}
	}
	return validations
}

func getMapValidations(schema *ovsdb.ColumnSchema) []string {
	var validations []string
	validations = append(validations, getCollectionSizeValidations(schema.TypeObj)...)

	// Check if we have key or value validations
	var keyAtomValidations []string
	var valueAtomValidations []string

	if schema.TypeObj.Key != nil {
		keyAtomValidations = getAtomicValidations(schema.TypeObj.Key)
	}
	if schema.TypeObj.Value != nil {
		valueAtomValidations = getAtomicValidations(schema.TypeObj.Value)
	}

	hasKeyValidations := len(keyAtomValidations) > 0
	hasValueValidations := len(valueAtomValidations) > 0

	if !hasKeyValidations && !hasValueValidations {
		return validations
	}
	// Only add dive validations if we have key or value validations
	var diveValidations []string
	diveValidations = append(diveValidations, "dive")

	// Add key validations if they exist
	if hasKeyValidations {
		diveValidations = append(diveValidations, "keys")
		diveValidations = append(diveValidations, keyAtomValidations...)
	}
	if hasKeyValidations && hasValueValidations {
		diveValidations = append(diveValidations, "endkeys")
	}
	// Add value validations if they exist
	if hasValueValidations {
		diveValidations = append(diveValidations, valueAtomValidations...)
	}

	validations = append(validations, diveValidations...)

	return validations
}

func getAtomicTypeValidations(schema *ovsdb.ColumnSchema) []string {
	var baseTypeForAtomic *ovsdb.BaseType
	if schema.TypeObj != nil && schema.TypeObj.Key != nil {
		baseTypeForAtomic = schema.TypeObj.Key
	} else if schema.TypeObj == nil {
		baseTypeForAtomic = &ovsdb.BaseType{Type: schema.Type}
	}

	if baseTypeForAtomic != nil {
		return getAtomicValidations(baseTypeForAtomic)
	}
	return nil
}

// ValidationTag generates the 'validate' struct tag based on OVSDB schema constraints.
func ValidationTag(schema *ovsdb.ColumnSchema) string {
	var finalValidations []string

	if schema.TypeObj == nil {
		finalValidations = getAtomicTypeValidations(schema)
	} else {
		switch schema.Type {
		case ovsdb.TypeSet:
			isPointerForOptionalSet := schema.TypeObj.Min() == 0 && schema.TypeObj.Max() == 1
			isScalarFromSet := schema.TypeObj.Min() == 1 && schema.TypeObj.Max() == 1
			switch {
			case isPointerForOptionalSet:
				if schema.TypeObj.Key != nil {
					elementValidations := getAtomicValidations(schema.TypeObj.Key)
					if len(elementValidations) > 0 {
						finalValidations = append(finalValidations, "omitempty")
						finalValidations = append(finalValidations, elementValidations...)
					}
				}
			case isScalarFromSet:
				if schema.TypeObj.Key != nil {
					finalValidations = append(finalValidations, getAtomicValidations(schema.TypeObj.Key)...)
				}
			default:
				finalValidations = getSetValidations(schema)
			}
		case ovsdb.TypeMap:
			finalValidations = getMapValidations(schema)
		default:
			finalValidations = getAtomicTypeValidations(schema)
		}
	}

	// Filter out any genuinely empty strings that might have been added.
	var nonEmptyValidations []string
	for _, v := range finalValidations {
		if v != "" {
			nonEmptyValidations = append(nonEmptyValidations, v)
		}
	}

	if len(nonEmptyValidations) == 0 {
		return ""
	}

	return fmt.Sprintf(` validate:"%s"`, strings.Join(nonEmptyValidations, ","))
}

// FileName returns the filename of a table
func FileName(table string) string {
	return fmt.Sprintf("%s.go", strings.ToLower(table))
}

// common initialisms used in ovsdb schemas
var initialisms = map[string]bool{
	"ACL":   true,
	"BFD":   true,
	"CFM":   true,
	"CT":    true,
	"CVLAN": true,
	"DNS":   true,
	"DSCP":  true,
	"ID":    true,
	"IP":    true,
	"IPFIX": true,
	"LACP":  true,
	"LLDP":  true,
	"MAC":   true,
	"MTU":   true,
	"OVS":   true,
	"QOS":   true,
	"RSTP":  true,
	"SSL":   true,
	"STP":   true,
	"TCP":   true,
	"SCTP":  true,
	"UDP":   true,
	"UUID":  true,
	"VLAN":  true,
	"STT":   true,
	"DNAT":  true,
	"SNAT":  true,
	"ICMP":  true,
	"SLB":   true,
}

func camelCase(field string) string {
	s := strings.ToLower(field)
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-'
	})
	if len(parts) > 1 {
		s = ""
		for _, p := range parts {
			s += cases.Title(language.Und, cases.NoLower).String(expandInitilaisms(p))
		}
	} else {
		s = cases.Title(language.Und, cases.NoLower).String(expandInitilaisms(s))
	}
	return s
}

func expandInitilaisms(s string) string {
	// check initialisms
	if u := strings.ToUpper(s); initialisms[u] {
		return strings.ToUpper(s)
	}
	// check for plurals too
	if strings.HasSuffix(s, "s") {
		sub := s[:len(s)-1]
		if u := strings.ToUpper(sub); initialisms[u] {
			return strings.ToUpper(sub) + "s"
		}
	}
	return s
}

func printVal(v any, t string) string {
	switch t {
	case "integer":
		return fmt.Sprintf(`%d`, v)
	case "real":
		return fmt.Sprintf(`%g`, v)
	case "boolean":
		return fmt.Sprintf(`%t`, v)
	case "string", "uuid":
		return fmt.Sprintf(`"%s"`, v)
	}
	return ""
}

func enumAliasSuffix(v any, t string) string {
	switch t {
	case "integer":
		return fmt.Sprintf(`%d`, v)
	case "real":
		return strings.Replace(fmt.Sprintf(`%g`, v), ".", "_", 1)
	case "boolean":
		return fmt.Sprintf(`%t`, v)
	case "string":
		return FieldName(fmt.Sprintf(`%s`, v))
	case "uuid":
		return strings.ReplaceAll(fmt.Sprintf(`%s`, v), "-", "_")
	}
	return ""
}

func floatEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

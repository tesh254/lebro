// Package jsonschema implements lebro's schema boundary with JSON Schema
// Draft 2020-12. The concrete engine is contained in this adapter package so
// applications can replace it without changing lebro.Tool.
package jsonschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"github.com/tesh254/lebro"
)

const schemaResource = "urn:lebro:json-schema"

// Compiler compiles JSON Schema Draft 2020-12 documents. External references
// are intentionally not loaded; local references and standard metaschemas are
// supported without network or filesystem access.
type Compiler struct{}

var _ lebro.SchemaCompiler = Compiler{}

// NewCompiler returns a JSON Schema Draft 2020-12 compiler.
func NewCompiler() Compiler {
	return Compiler{}
}

// Compile parses, checks, and compiles a JSON Schema document.
func (Compiler) Compile(raw json.RawMessage) (lebro.CompiledSchema, error) {
	if len(raw) == 0 {
		return nil, &lebro.SchemaError{Message: "schema must not be empty"}
	}

	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, &lebro.SchemaError{Message: "schema must be valid JSON"}
	}
	if err := validateDialect(document); err != nil {
		return nil, err
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	resourceErr := compiler.AddResource(schemaResource, document)
	schema, compileErr := compiler.Compile(schemaResource)
	if err := errors.Join(resourceErr, compileErr); err != nil {
		return nil, normalizeSchemaError(err)
	}
	return compiledSchema{schema: schema}, nil
}

type compiledSchema struct {
	schema *jsonschema.Schema
}

var _ lebro.CompiledSchema = compiledSchema{}

func (s compiledSchema) Validate(raw json.RawMessage) *lebro.ValidationError {
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return &lebro.ValidationError{Issues: []lebro.ValidationIssue{{
			Keyword: "json",
			Message: "value must be valid JSON",
		}}}
	}

	if err := s.schema.Validate(value); err != nil {
		validationErr := err.(*jsonschema.ValidationError)
		return &lebro.ValidationError{Issues: validationIssues(validationErr, value)}
	}
	return nil
}

func validateDialect(document any) error {
	object, ok := document.(map[string]any)
	if !ok {
		return nil
	}
	dialect, ok := object["$schema"]
	if !ok {
		return nil
	}
	dialectString, ok := dialect.(string)
	if !ok {
		return &lebro.SchemaError{Path: "/$schema", Message: "$schema must be a string"}
	}
	if strings.TrimSuffix(dialectString, "#") != lebro.JSONSchemaDraft202012 {
		return &lebro.SchemaError{Path: "/$schema", Message: "unsupported dialect " + strconvQuote(dialectString)}
	}
	return nil
}

func normalizeSchemaError(err error) error {
	var schemaValidationErr *jsonschema.SchemaValidationError
	if errors.As(err, &schemaValidationErr) {
		var validationErr *jsonschema.ValidationError
		if errors.As(schemaValidationErr.Err, &validationErr) {
			issues := validationIssues(validationErr, nil)
			if len(issues) != 0 {
				return &lebro.SchemaError{Path: issues[0].Path, Message: issues[0].Message}
			}
		}
		return &lebro.SchemaError{Message: "schema does not satisfy the Draft 2020-12 metaschema"}
	}

	var loadErr *jsonschema.LoadURLError
	if errors.As(err, &loadErr) {
		return &lebro.SchemaError{Message: "schema contains an unresolved external reference"}
	}
	return &lebro.SchemaError{Message: "schema could not be compiled"}
}

func validationIssues(validationErr *jsonschema.ValidationError, instance any) []lebro.ValidationIssue {
	issues := make([]lebro.ValidationIssue, 0, 1)
	collectValidationIssues(validationErr, instance, &issues)
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Path != issues[j].Path {
			return issues[i].Path < issues[j].Path
		}
		if issues[i].Keyword != issues[j].Keyword {
			return issues[i].Keyword < issues[j].Keyword
		}
		return issues[i].Message < issues[j].Message
	})
	return issues
}

func collectValidationIssues(validationErr *jsonschema.ValidationError, instance any, issues *[]lebro.ValidationIssue) {
	instanceLocation := validationErr.InstanceLocation
	if errorKind, ok := validationErr.ErrorKind.(*kind.PropertyNames); ok {
		path := appendPath(instanceLocation, errorKind.Property)
		if len(validationErr.InstanceLocation) == 0 {
			if located, found := findPropertyPath(instance, errorKind.Property); found {
				path = located
			}
		}
		*issues = append(*issues, lebro.ValidationIssue{
			Path:    jsonPointer(path),
			Keyword: "propertyNames",
			Message: "property name is not allowed",
		})
		return
	}
	if len(validationErr.Causes) != 0 {
		for _, cause := range validationErr.Causes {
			collectValidationIssues(cause, instance, issues)
		}
		return
	}

	keyword := ""
	keywordPath := validationErr.ErrorKind.KeywordPath()
	if len(keywordPath) != 0 {
		keyword = keywordPath[0]
	}

	switch errorKind := validationErr.ErrorKind.(type) {
	case *kind.Required:
		properties := append([]string(nil), errorKind.Missing...)
		sort.Strings(properties)
		for _, property := range properties {
			*issues = append(*issues, lebro.ValidationIssue{
				Path:    jsonPointer(appendPath(instanceLocation, property)),
				Keyword: keyword,
				Message: "required property is missing",
			})
		}
	case *kind.DependentRequired:
		properties := append([]string(nil), errorKind.Missing...)
		sort.Strings(properties)
		for _, property := range properties {
			*issues = append(*issues, lebro.ValidationIssue{
				Path:    jsonPointer(appendPath(instanceLocation, property)),
				Keyword: keyword,
				Message: "property is required when " + strconvQuote(errorKind.Prop) + " is present",
			})
		}
	case *kind.AdditionalProperties:
		properties := append([]string(nil), errorKind.Properties...)
		sort.Strings(properties)
		for _, property := range properties {
			*issues = append(*issues, lebro.ValidationIssue{
				Path:    jsonPointer(appendPath(instanceLocation, property)),
				Keyword: keyword,
				Message: "additional property is not allowed",
			})
		}
	default:
		output := validationErr.BasicOutput()
		*issues = append(*issues, lebro.ValidationIssue{
			Path:    jsonPointer(instanceLocation),
			Keyword: keyword,
			Message: output.Error.String(),
		})
	}
}

func findPropertyPath(value any, property string) ([]string, bool) {
	switch value := value.(type) {
	case map[string]any:
		if _, ok := value[property]; ok {
			return []string{property}, true
		}
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if path, found := findPropertyPath(value[key], property); found {
				return prependPath(key, path), true
			}
		}
	case []any:
		for index, item := range value {
			if path, found := findPropertyPath(item, property); found {
				return prependPath(strconv.Itoa(index), path), true
			}
		}
	}
	return nil, false
}

func prependPath(token string, path []string) []string {
	prepended := make([]string, 1, len(path)+1)
	prepended[0] = token
	return append(prepended, path...)
}

func appendPath(path []string, token string) []string {
	appended := make([]string, len(path), len(path)+1)
	copy(appended, path)
	return append(appended, token)
}

func jsonPointer(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	escaped := make([]string, len(tokens))
	for i, token := range tokens {
		token = strings.ReplaceAll(token, "~", "~0")
		escaped[i] = strings.ReplaceAll(token, "/", "~1")
	}
	return "/" + strings.Join(escaped, "/")
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

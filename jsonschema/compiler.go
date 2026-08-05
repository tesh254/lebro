// Package jsonschema implements lebro's schema boundary with JSON Schema
// Draft 2020-12. The concrete engine is contained in this adapter package so
// applications can replace it without changing lebro.Tool.
package jsonschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
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

// Compile concurrently parses, checks, and compiles a JSON Schema document.
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
	collector := validationIssueCollector{
		instance:        instance,
		propertyPaths:   map[string][][]string{},
		propertyOffsets: map[string]int{},
	}
	collector.collect(validationErr, nil, &issues)
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

type validationIssueCollector struct {
	instance        any
	propertyPaths   map[string][][]string
	propertyOffsets map[string]int
}

func (c *validationIssueCollector) collect(validationErr *jsonschema.ValidationError, parentLocation []string, issues *[]lebro.ValidationIssue) {
	instanceLocation := validationErr.InstanceLocation
	if len(instanceLocation) == 0 && len(parentLocation) != 0 {
		instanceLocation = parentLocation
	}
	if errorKind, ok := validationErr.ErrorKind.(*kind.PropertyNames); ok {
		path := appendPath(instanceLocation, errorKind.Property)
		if len(instanceLocation) == 0 {
			path = c.nextPropertyPath(validationErr.SchemaURL, errorKind.Property, path)
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
			c.collect(cause, instanceLocation, issues)
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

func (c *validationIssueCollector) nextPropertyPath(schemaURL, property string, fallback []string) []string {
	key := schemaURL + "\x00" + property
	paths, ok := c.propertyPaths[key]
	if !ok {
		paths = findPropertyPaths(c.instance, schemaURL, property)
		c.propertyPaths[key] = paths
	}
	offset := c.propertyOffsets[key]
	if offset >= len(paths) {
		return fallback
	}
	c.propertyOffsets[key] = offset + 1
	return paths[offset]
}

type instanceCandidate struct {
	value any
	path  []string
}

func findPropertyPaths(instance any, schemaURL, property string) [][]string {
	candidates := []instanceCandidate{{value: instance}}
	tokens := schemaLocationTokens(schemaURL)
	for index := 0; index < len(tokens); index++ {
		switch tokens[index] {
		case "properties":
			index++
			propertyName := tokens[index]
			next := make([]instanceCandidate, 0, len(candidates))
			for _, candidate := range candidates {
				object, ok := candidate.value.(map[string]any)
				if !ok {
					continue
				}
				value, ok := object[propertyName]
				if !ok {
					continue
				}
				next = append(next, instanceCandidate{value: value, path: appendPath(candidate.path, propertyName)})
			}
			candidates = next
		case "items":
			next := make([]instanceCandidate, 0, len(candidates))
			for _, candidate := range candidates {
				items, ok := candidate.value.([]any)
				if !ok {
					continue
				}
				for itemIndex, item := range items {
					next = append(next, instanceCandidate{value: item, path: appendPath(candidate.path, strconv.Itoa(itemIndex))})
				}
			}
			candidates = next
		}
	}

	paths := make([][]string, 0, len(candidates))
	for _, candidate := range candidates {
		object, ok := candidate.value.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := object[property]; ok {
			paths = append(paths, appendPath(candidate.path, property))
		}
	}
	return paths
}

func schemaLocationTokens(schemaURL string) []string {
	_, fragment, found := strings.Cut(schemaURL, "#")
	if !found || fragment == "" {
		return nil
	}
	fragment, _ = url.PathUnescape(fragment)
	parts := strings.Split(strings.TrimPrefix(fragment, "/"), "/")
	for index, part := range parts {
		part = strings.ReplaceAll(part, "~1", "/")
		parts[index] = strings.ReplaceAll(part, "~0", "~")
	}
	return parts
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

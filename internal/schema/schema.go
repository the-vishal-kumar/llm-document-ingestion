// Package schema validates extracted documents against a JSON Schema.
//
// It implements the subset of JSON Schema (draft 2020-12 keywords) that the
// extraction schema uses: type (single or union), properties, required,
// additionalProperties:false, items, enum, pattern, minLength, maxLength,
// minimum, maximum, exclusiveMinimum, minItems and maxItems. Any other keyword
// in a schema is rejected at compile time, so the schema can never silently
// claim a constraint the validator does not enforce.
package schema

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

//go:embed po.schema.json
var poSchemaJSON []byte

// PORaw returns the raw purchase-order schema, for embedding in LLM prompts.
func PORaw() []byte { return slices.Clone(poSchemaJSON) }

// PO is the compiled purchase-order schema.
var PO = MustCompile(poSchemaJSON)

// Schema is a compiled schema node.
type Schema struct {
	Types            []string
	Properties       map[string]*Schema
	Required         []string
	NoAdditional     bool
	Items            *Schema
	Enum             []any
	Pattern          *regexp.Regexp
	MinLength        *int
	MaxLength        *int
	Minimum          *float64
	Maximum          *float64
	ExclusiveMinimum *float64
	MinItems         *int
	MaxItems         *int
}

var supported = map[string]bool{
	"$schema": true, "title": true, "description": true,
	"type": true, "properties": true, "required": true, "additionalProperties": true,
	"items": true, "enum": true, "pattern": true, "minLength": true, "maxLength": true,
	"minimum": true, "maximum": true, "exclusiveMinimum": true, "minItems": true, "maxItems": true,
}

// MustCompile compiles raw or panics. Use it for embedded schemas only.
func MustCompile(raw []byte) *Schema {
	s, err := Compile(raw)
	if err != nil {
		panic(err)
	}
	return s
}

// Compile parses a JSON Schema document.
func Compile(raw []byte) (*Schema, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return compile(m, "#")
}

func compile(m map[string]any, at string) (*Schema, error) {
	s := &Schema{}
	for k, v := range m {
		if !supported[k] {
			return nil, fmt.Errorf("schema %s: unsupported keyword %q", at, k)
		}
		var err error
		switch k {
		case "type":
			switch t := v.(type) {
			case string:
				s.Types = []string{t}
			case []any:
				for _, x := range t {
					str, ok := x.(string)
					if !ok {
						return nil, fmt.Errorf("schema %s: bad type", at)
					}
					s.Types = append(s.Types, str)
				}
			default:
				return nil, fmt.Errorf("schema %s: bad type", at)
			}
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("schema %s: properties must be an object", at)
			}
			s.Properties = map[string]*Schema{}
			for name, sub := range props {
				sm, ok := sub.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("schema %s/%s: not an object", at, name)
				}
				if s.Properties[name], err = compile(sm, at+"/properties/"+name); err != nil {
					return nil, err
				}
			}
		case "required":
			arr, _ := v.([]any)
			for _, x := range arr {
				str, _ := x.(string)
				s.Required = append(s.Required, str)
			}
		case "additionalProperties":
			b, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("schema %s: only boolean additionalProperties is supported", at)
			}
			s.NoAdditional = !b
		case "items":
			sm, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("schema %s: items must be an object", at)
			}
			if s.Items, err = compile(sm, at+"/items"); err != nil {
				return nil, err
			}
		case "enum":
			s.Enum, _ = v.([]any)
		case "pattern":
			str, _ := v.(string)
			if s.Pattern, err = regexp.Compile(str); err != nil {
				return nil, fmt.Errorf("schema %s: %w", at, err)
			}
		case "minLength":
			s.MinLength = intPtr(v)
		case "maxLength":
			s.MaxLength = intPtr(v)
		case "minItems":
			s.MinItems = intPtr(v)
		case "maxItems":
			s.MaxItems = intPtr(v)
		case "minimum":
			s.Minimum = floatPtr(v)
		case "maximum":
			s.Maximum = floatPtr(v)
		case "exclusiveMinimum":
			s.ExclusiveMinimum = floatPtr(v)
		}
	}
	return s, nil
}

func intPtr(v any) *int {
	f, _ := v.(float64)
	i := int(f)
	return &i
}

func floatPtr(v any) *float64 {
	f, _ := v.(float64)
	return &f
}

// Error lists every violation found, each prefixed with a JSON pointer.
type Error struct{ Violations []string }

func (e *Error) Error() string {
	return "schema validation failed: " + strings.Join(e.Violations, "; ")
}

// ValidateJSON decodes raw and validates it.
func (s *Schema) ValidateJSON(raw []byte) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return &Error{Violations: []string{"#: not valid JSON"}}
	}
	return s.Validate(v)
}

// Validate checks a value produced by encoding/json (map[string]any, []any,
// float64, string, bool, nil).
func (s *Schema) Validate(v any) error {
	var out []string
	s.validate(v, "#", &out)
	if len(out) > 0 {
		return &Error{Violations: out}
	}
	return nil
}

func typeOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if x == math.Trunc(x) {
			return "integer"
		}
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	}
	return "unknown"
}

func (s *Schema) validate(v any, at string, out *[]string) {
	if len(s.Types) > 0 {
		t := typeOf(v)
		ok := slices.Contains(s.Types, t) || (t == "integer" && slices.Contains(s.Types, "number"))
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: expected %s, got %s", at, strings.Join(s.Types, "|"), t))
			return
		}
	}
	if s.Enum != nil && !slices.ContainsFunc(s.Enum, func(e any) bool { return e == v }) {
		*out = append(*out, at+": value not in enum")
	}
	switch x := v.(type) {
	case string:
		n := utf8.RuneCountInString(x)
		if s.MinLength != nil && n < *s.MinLength {
			*out = append(*out, at+": string too short")
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			*out = append(*out, at+": string too long")
		}
		if s.Pattern != nil && !s.Pattern.MatchString(x) {
			*out = append(*out, at+": does not match pattern")
		}
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			*out = append(*out, at+": not a finite number")
		}
		if s.Minimum != nil && x < *s.Minimum {
			*out = append(*out, fmt.Sprintf("%s: below minimum %v", at, *s.Minimum))
		}
		if s.Maximum != nil && x > *s.Maximum {
			*out = append(*out, fmt.Sprintf("%s: above maximum %v", at, *s.Maximum))
		}
		if s.ExclusiveMinimum != nil && x <= *s.ExclusiveMinimum {
			*out = append(*out, fmt.Sprintf("%s: must be > %v", at, *s.ExclusiveMinimum))
		}
	case []any:
		if s.MinItems != nil && len(x) < *s.MinItems {
			*out = append(*out, at+": too few items")
		}
		if s.MaxItems != nil && len(x) > *s.MaxItems {
			*out = append(*out, at+": too many items")
		}
		if s.Items != nil {
			for i, e := range x {
				s.Items.validate(e, fmt.Sprintf("%s/%d", at, i), out)
			}
		}
	case map[string]any:
		for _, r := range s.Required {
			if _, ok := x[r]; !ok {
				*out = append(*out, fmt.Sprintf("%s: missing required property %q", at, r))
			}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub, ok := s.Properties[k]
			if !ok {
				if s.NoAdditional {
					*out = append(*out, fmt.Sprintf("%s: unexpected property %q", at, k))
				}
				continue
			}
			sub.validate(x[k], at+"/"+k, out)
		}
	}
}

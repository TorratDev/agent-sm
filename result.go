package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

type resultSchema struct {
	Type                 string                  `json:"type"`
	Properties           map[string]resultSchema `json:"properties"`
	Required             []string                `json:"required"`
	Enum                 []string                `json:"enum"`
	Items                *resultSchema           `json:"items"`
	AdditionalProperties *bool                   `json:"additionalProperties"`
}

// validateResult covers the object/array/scalar/enum subset used by the step table.
// Unknown schema features fail closed rather than pretending to enforce them.
func validateResult(raw []byte, schema string, out any) error {
	var s resultSchema
	if err := decodeStrict([]byte(schema), &s); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := uniqueJSON(d)
	if err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("result must contain exactly one JSON value")
	}
	if err := s.check(v); err != nil {
		return err
	}
	return decodeStrict(raw, out)
}

// Decode tokens explicitly so duplicate result keys cannot hide an earlier value.
func uniqueJSON(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		m := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			k, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("invalid object key")
			}
			if _, found := m[k]; found {
				return nil, fmt.Errorf("duplicate result field %q", k)
			}
			value, err := uniqueJSON(d)
			if err != nil {
				return nil, err
			}
			m[k] = value
		}
		if end, err := d.Token(); err != nil || end != json.Delim('}') {
			return nil, fmt.Errorf("unterminated object")
		}
		return m, nil
	case json.Delim('['):
		a := []any{}
		for d.More() {
			value, err := uniqueJSON(d)
			if err != nil {
				return nil, err
			}
			a = append(a, value)
		}
		if end, err := d.Token(); err != nil || end != json.Delim(']') {
			return nil, fmt.Errorf("unterminated array")
		}
		return a, nil
	}
	return t, nil
}

func (s resultSchema) check(v any) error {
	if v == nil {
		return fmt.Errorf("null result value")
	}
	if len(s.Enum) > 0 {
		for _, option := range s.Enum {
			if v == option {
				return nil
			}
		}
		return fmt.Errorf("invalid enum value")
	}
	switch s.Type {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("expected object")
		}
		for _, k := range s.Required {
			if _, ok := m[k]; !ok {
				return fmt.Errorf("missing result field %q", k)
			}
		}
		for k, value := range m {
			prop, ok := s.Properties[k]
			if !ok {
				return fmt.Errorf("unknown result field %q", k)
			}
			if err := prop.check(value); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok || s.Items == nil {
			return fmt.Errorf("expected array with item schema")
		}
		for _, item := range a {
			if err := s.Items.check(item); err != nil {
				return err
			}
		}
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected string")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected boolean")
		}
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return fmt.Errorf("expected integer")
		}
		if _, err := n.Int64(); err != nil {
			return fmt.Errorf("expected bounded integer")
		}
	default:
		return fmt.Errorf("unsupported result schema type %q", s.Type)
	}
	return nil
}

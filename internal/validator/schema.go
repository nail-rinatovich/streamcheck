package validator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

type Column struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Required bool     `json:"required,omitempty"`
	Min      *float64 `json:"min,omitempty"`
	Max      *float64 `json:"max,omitempty"`
	Pattern  string   `json:"pattern,omitempty"`
	pattern  *regexp.Regexp
}
type Schema struct {
	Columns    []Column `json:"columns"`
	AllowExtra bool     `json:"allow_extra"`
}

func ParseSchema(data []byte) (Schema, error) {
	var s Schema
	if len(data) > 32768 {
		return s, errors.New("schema exceeds 32 KiB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return s, fmt.Errorf("schema: %w", err)
	}
	if d.Decode(new(any)) != io.EOF {
		return s, errors.New("schema must contain exactly one object")
	}
	if len(s.Columns) == 0 || len(s.Columns) > 128 {
		return s, errors.New("schema needs 1..128 columns")
	}
	names := map[string]bool{}
	for i := range s.Columns {
		c := &s.Columns[i]
		if c.Name == "" || len(c.Name) > 128 || c.Name[0] == '$' || names[c.Name] {
			return s, errors.New("column names must be unique, nonempty, <=128 bytes and not start with $")
		}
		names[c.Name] = true
		switch c.Type {
		case "string", "integer", "number", "boolean", "date":
		default:
			return s, fmt.Errorf("unsupported type for %s", c.Name)
		}
		if (c.Min != nil || c.Max != nil) && c.Type != "number" && c.Type != "integer" {
			return s, errors.New("min/max require a numeric column")
		}
		if c.Min != nil && c.Max != nil && *c.Min > *c.Max {
			return s, errors.New("min must be <= max")
		}
		if c.Pattern != "" {
			if c.Type != "string" {
				return s, errors.New("pattern requires string")
			}
			var err error
			c.pattern, err = regexp.Compile(c.Pattern)
			if err != nil {
				return s, fmt.Errorf("pattern: %w", err)
			}
		}
	}
	return s, nil
}

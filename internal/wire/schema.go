package wire

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// The result_schema subset: only these keys are accepted, recursively (§5.3).
const (
	keyType       = "type"
	keyProperties = "properties"
	keyRequired   = "required"
	keyItems      = "items"
)

// schemaNode is the parsed form of one result_schema node.
type schemaNode struct {
	typ        string
	properties map[string]*schemaNode
	required   []string
	items      *schemaNode
}

// ValidateSchema checks that schema stays within the accepted result_schema
// subset (§5.3): the only keys accepted are type, properties, required, and
// items - recursively for nested schemas - and the only type values accepted
// are object, array, string, number, integer, and boolean. An empty object
// schema is valid and constrains nothing.
func ValidateSchema(schema json.RawMessage) error {
	_, err := parseSchemaNode(schema, "")
	return err
}

// ValidateSchemaValue checks that schema is within the subset and that value
// conforms to it: type conformance, required properties present, and items
// conformity (§5.3, §5.4).
func ValidateSchemaValue(schema, value json.RawMessage) error {
	node, err := parseSchemaNode(schema, "")
	if err != nil {
		return err
	}
	if !json.Valid(value) {
		return fmt.Errorf("value: not valid JSON")
	}
	return node.checkValue(value)
}

func parseSchemaNode(raw json.RawMessage, path string) (*schemaNode, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s: empty schema", schemaWhere(path))
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%s: not valid JSON", schemaWhere(path))
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("%s: must be a JSON object", schemaWhere(path))
	}
	n := &schemaNode{}
	for key, val := range obj {
		switch key {
		case keyType:
			var typ string
			if err := json.Unmarshal(val, &typ); err != nil {
				return nil, fmt.Errorf("%s: type must be a string", schemaWhere(path))
			}
			if !knownSchemaType(typ) {
				return nil, fmt.Errorf("%s: unknown type %q", schemaWhere(path), typ)
			}
			n.typ = typ
		case keyProperties:
			var props map[string]json.RawMessage
			if err := json.Unmarshal(val, &props); err != nil || props == nil {
				return nil, fmt.Errorf("%sproperties: must be a JSON object", schemaWhere(path))
			}
			n.properties = make(map[string]*schemaNode, len(props))
			for name, sub := range props {
				node, err := parseSchemaNode(sub, childSchemaPath(path, keyProperties+"."+name))
				if err != nil {
					return nil, err
				}
				n.properties[name] = node
			}
		case keyRequired:
			if err := json.Unmarshal(val, &n.required); err != nil {
				return nil, fmt.Errorf("%srequired: must be an array of strings", schemaWhere(path))
			}
		case keyItems:
			node, err := parseSchemaNode(val, childSchemaPath(path, keyItems))
			if err != nil {
				return nil, err
			}
			n.items = node
		default:
			return nil, fmt.Errorf("%s: key %q is outside the result_schema subset", schemaWhere(path), key)
		}
	}
	return n, nil
}

func knownSchemaType(typ string) bool {
	switch typ {
	case "object", "array", "string", "number", "integer", "boolean":
		return true
	default:
		return false
	}
}

func (n *schemaNode) checkValue(raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("value: %w", err)
	}
	return n.check(v, "")
}

func (n *schemaNode) check(v any, path string) error {
	if n.typ != "" && !conformsToType(v, n.typ) {
		return fmt.Errorf("%s: expected %s", valueWhere(path), n.typ)
	}
	if len(n.required) > 0 || len(n.properties) > 0 {
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: expected an object", valueWhere(path))
		}
		for _, name := range n.required {
			if _, ok := obj[name]; !ok {
				return fmt.Errorf("%s: missing required property %q", valueWhere(path), name)
			}
		}
		names := make([]string, 0, len(n.properties))
		for name := range n.properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if field, ok := obj[name]; ok {
				if err := n.properties[name].check(field, childValuePath(path, name)); err != nil {
					return err
				}
			}
		}
	}
	if n.items != nil {
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: expected an array", valueWhere(path))
		}
		for i, item := range arr {
			if err := n.items.check(item, itemValuePath(path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func conformsToType(v any, typ string) bool {
	switch typ {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	case "boolean":
		_, ok := v.(bool)
		return ok
	default:
		return false
	}
}

func schemaWhere(path string) string {
	if path == "" {
		return "schema"
	}
	return "schema." + path
}

func childSchemaPath(path, seg string) string {
	if path == "" {
		return seg
	}
	return path + "." + seg
}

func valueWhere(path string) string {
	if path == "" {
		return "value"
	}
	return "value." + path
}

func childValuePath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func itemValuePath(path string, i int) string {
	if path == "" {
		return fmt.Sprintf("[%d]", i)
	}
	return fmt.Sprintf("%s[%d]", path, i)
}

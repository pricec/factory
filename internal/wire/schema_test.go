package wire

import (
	"encoding/json"
	"testing"
)

func TestValidateSchema(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		schema  json.RawMessage
		wantErr bool
	}{
		{"ok empty schema", raw(`{}`), false},
		{"ok type object", raw(`{"type":"object"}`), false},
		{"ok type array", raw(`{"type":"array"}`), false},
		{"ok type string", raw(`{"type":"string"}`), false},
		{"ok type number", raw(`{"type":"number"}`), false},
		{"ok type integer", raw(`{"type":"integer"}`), false},
		{"ok type boolean", raw(`{"type":"boolean"}`), false},
		{"ok required without type", raw(`{"required":["a","b"]}`), false},
		{"ok properties without type", raw(`{"properties":{"a":{"type":"string"}}}`), false},
		{"ok object with properties and required", raw(`{
			"type":"object",
			"required":["a"],
			"properties":{"a":{"type":"string"},"b":{"type":"number"}}
		}`), false},
		{"ok array of objects", raw(`{
			"type":"array",
			"items":{"type":"object","required":["x"],"properties":{"x":{"type":"integer"}}}
		}`), false},
		{"ok nested arrays", raw(`{
			"type":"object",
			"properties":{"matrix":{"type":"array","items":{"type":"array","items":{"type":"number"}}}}
		}`), false},

		{"reject enum", raw(`{"enum":["a","b"]}`), true},
		{"reject minimum", raw(`{"minimum":1}`), true},
		{"reject pattern", raw(`{"type":"string","pattern":"^x$"}`), true},
		{"reject extra key beside type", raw(`{"type":"object","minProperties":1}`), true},
		{"reject description", raw(`{"description":"a result"}`), true},
		{"reject $schema", raw(`{"$schema":"https://example.com"}`), true},
		{"reject additionalProperties", raw(`{"additionalProperties":false}`), true},
		{"reject extra key nested in properties", raw(`{
			"type":"object",
			"properties":{"a":{"type":"string","maxLength":4}}
		}`), true},
		{"reject extra key nested in items", raw(`{"type":"array","items":{"type":"string","minLength":1}}`), true},
		{"reject extra key deep in nested arrays", raw(`{
			"type":"object",
			"properties":{"a":{"type":"array","items":{"type":"object","properties":{"b":{"format":"date"}}}}}
		}`), true},
		{"reject unknown type null", raw(`{"type":"null"}`), true},
		{"reject unknown type any", raw(`{"type":"any"}`), true},
		{"reject empty type value", raw(`{"type":""}`), true},
		{"reject type as array", raw(`{"type":["string","number"]}`), true},
		{"reject type as number", raw(`{"type":5}`), true},
		{"reject type as boolean", raw(`{"type":true}`), true},
		{"reject required as string", raw(`{"required":"a"}`), true},
		{"reject required with non-string entries", raw(`{"required":["a",1]}`), true},
		{"reject properties as array", raw(`{"properties":["a"]}`), true},
		{"reject properties as null", raw(`{"properties":null}`), true},
		{"reject items as string", raw(`{"items":"string"}`), true},
		{"reject non-object schema number", raw(`5`), true},
		{"reject non-object schema array", raw(`[{"type":"string"}]`), true},
		{"reject null schema", raw(`null`), true},
		{"reject invalid JSON", raw(`{nope`), true},
		{"reject empty raw", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSchema(tc.schema)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateSchema(%s) = %v, wantErr %v", tc.schema, err, tc.wantErr)
			}
		})
	}
}

func TestValidateSchemaValue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		schema  json.RawMessage
		value   json.RawMessage
		wantErr bool
	}{
		{"ok empty schema accepts object", raw(`{}`), raw(`{"a":[1,{"b":null}]}`), false},
		{"ok empty schema accepts null", raw(`{}`), raw(`null`), false},
		{"ok empty schema accepts string", raw(`{}`), raw(`"anything"`), false},

		{"ok object matches", raw(`{"type":"object"}`), raw(`{"a":1}`), false},
		{"reject array against object", raw(`{"type":"object"}`), raw(`[1]`), true},
		{"reject null against object", raw(`{"type":"object"}`), raw(`null`), true},

		{"ok array matches", raw(`{"type":"array"}`), raw(`[1,"two"]`), false},
		{"reject object against array", raw(`{"type":"array"}`), raw(`{"a":1}`), true},

		{"ok string matches", raw(`{"type":"string"}`), raw(`""`), false},
		{"reject number against string", raw(`{"type":"string"}`), raw(`5`), true},

		{"ok number matches integer literal", raw(`{"type":"number"}`), raw(`5`), false},
		{"ok number matches float", raw(`{"type":"number"}`), raw(`5.5`), false},
		{"reject string against number", raw(`{"type":"number"}`), raw(`"5"`), true},
		{"reject boolean against number", raw(`{"type":"number"}`), raw(`true`), true},

		{"ok integer matches", raw(`{"type":"integer"}`), raw(`7`), false},
		{"ok integer matches whole float", raw(`{"type":"integer"}`), raw(`7.0`), false},
		{"reject fraction against integer", raw(`{"type":"integer"}`), raw(`7.5`), true},

		{"ok boolean matches", raw(`{"type":"boolean"}`), raw(`true`), false},
		{"reject zero against boolean", raw(`{"type":"boolean"}`), raw(`0`), true},

		{"ok required present", raw(`{"required":["a"]}`), raw(`{"a":1,"b":2}`), false},
		{"reject required missing", raw(`{"required":["a"]}`), raw(`{"b":1}`), true},
		{"reject required against non-object", raw(`{"required":["a"]}`), raw(`"str"`), true},

		{"ok property type conforms", raw(`{"properties":{"a":{"type":"string"}}}`), raw(`{"a":"x","b":1}`), false},
		{"ok property absent and not required", raw(`{"properties":{"a":{"type":"string"}}}`), raw(`{"b":1}`), false},
		{"reject property type mismatch", raw(`{"properties":{"a":{"type":"string"}}}`), raw(`{"a":1}`), true},

		{"ok object full check", raw(`{
			"type":"object",
			"required":["answer"],
			"properties":{"answer":{"type":"integer"}}
		}`), raw(`{"answer":42}`), false},
		{"reject object missing required", raw(`{
			"type":"object",
			"required":["answer"],
			"properties":{"answer":{"type":"integer"}}
		}`), raw(`{"other":1}`), true},
		{"reject object wrong property type", raw(`{
			"type":"object",
			"required":["answer"],
			"properties":{"answer":{"type":"integer"}}
		}`), raw(`{"answer":"42"}`), true},

		{"ok items conform", raw(`{"type":"array","items":{"type":"string"}}`), raw(`["a","b"]`), false},
		{"ok empty array conforms", raw(`{"type":"array","items":{"type":"string"}}`), raw(`[]`), false},
		{"reject one item mismatch", raw(`{"type":"array","items":{"type":"string"}}`), raw(`["a",1]`), true},
		{"reject non-array against items", raw(`{"items":{"type":"string"}}`), raw(`"a"`), true},

		{"ok nested array of objects", raw(`{
			"type":"array",
			"items":{"type":"object","required":["x"],"properties":{"x":{"type":"number"}}}
		}`), raw(`[{"x":1},{"x":2.5}]`), false},
		{"reject nested object missing required", raw(`{
			"type":"array",
			"items":{"type":"object","required":["x"],"properties":{"x":{"type":"number"}}}
		}`), raw(`[{"x":1},{"y":2}]`), true},

		{"reject invalid value JSON", raw(`{}`), raw(`{nope`), true},
		{"reject invalid schema", raw(`{"enum":[1]}`), raw(`1`), true},
		{"reject nil value", raw(`{"type":"object"}`), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSchemaValue(tc.schema, tc.value)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateSchemaValue(%s, %s) = %v, wantErr %v", tc.schema, tc.value, err, tc.wantErr)
			}
		})
	}
}

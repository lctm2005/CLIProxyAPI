package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeTraeCLIToolParametersSchemaLocations(t *testing.T) {
	for _, keyword := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies"} {
		t.Run(keyword, func(t *testing.T) {
			schema := `{"` + keyword + `":{"encrypted":{"type":"string","encrypted":true}}}`
			got := gjson.Parse(NormalizeTraeCLIToolParameters(schema))
			if got.Get(keyword+".encrypted.encrypted").Exists() || got.Get(keyword+".encrypted.type").String() != "string" {
				t.Fatalf("schema map lost a property or retained the annotation: %s", got.Raw)
			}
		})
	}
	for _, keyword := range []string{"additionalProperties", "unevaluatedProperties", "propertyNames", "additionalItems", "unevaluatedItems", "contains", "not", "if", "then", "else", "items", "contentSchema"} {
		t.Run(keyword, func(t *testing.T) {
			schema := `{"` + keyword + `":{"type":"string","encrypted":true}}`
			got := gjson.Parse(NormalizeTraeCLIToolParameters(schema))
			if got.Get(keyword+".encrypted").Exists() || got.Get(keyword+".type").String() != "string" {
				t.Fatalf("subschema lost its type or retained the annotation: %s", got.Raw)
			}
		})
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems", "items"} {
		t.Run(keyword+"/array", func(t *testing.T) {
			schema := `{"` + keyword + `":[{"type":"string","encrypted":true},false]}`
			got := gjson.Parse(NormalizeTraeCLIToolParameters(schema))
			if got.Get(keyword+".0.encrypted").Exists() || got.Get(keyword+".0.type").String() != "string" || got.Get(keyword+".1").Raw != "false" {
				t.Fatalf("schema array changed unrelated constraints: %s", got.Raw)
			}
		})
	}
}

func TestNormalizeTraeCLIToolParametersPreservesLiteralData(t *testing.T) {
	schema := `{"encrypted":true,"properties":{"escaped.name":{"encrypted":true,"type":"string"}},"enum":[{"encrypted":true}],"dependencies":{"field":["encrypted"]},"default":{"encrypted":true,"contentSchema":{"encrypted":true}},"x-data":{"encrypted":true},"minimum":9007199254740993}`
	got := gjson.Parse(NormalizeTraeCLIToolParameters(schema))
	if got.Get("encrypted").Exists() || got.Get(`properties.escaped\.name.encrypted`).Exists() {
		t.Fatalf("schema annotations survived: %s", got.Raw)
	}
	for path, want := range map[string]string{
		`properties.escaped\.name.type`:   "string",
		"enum.0.encrypted":                "true",
		"dependencies.field.0":            "encrypted",
		"default.encrypted":               "true",
		"default.contentSchema.encrypted": "true",
		"x-data.encrypted":                "true",
		"minimum":                         "9007199254740993",
	} {
		if value := got.Get(path); !value.Exists() || value.String() != want {
			t.Errorf("%s = %s, want %s", path, value.Raw, want)
		}
	}
	for _, unchanged := range []string{"true", "false", "null", "invalid-json", `{"type":"object","properties":{"encrypted":{"type":"boolean"}}}`} {
		if result := NormalizeTraeCLIToolParameters(unchanged); result != unchanged {
			t.Errorf("schema changed: %s -> %s", unchanged, result)
		}
	}
}

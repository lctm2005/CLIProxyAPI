package helps

import (
	"strings"

	"github.com/tidwall/gjson"
)

// NormalizeTraeCLIToolParameters removes Codex's encrypted schema annotation
// from tools forwarded through TRAE raw-chat. With this annotation, the GPT
// upstream returns tool names but no argument deltas; the plain JSON tool
// protocol used here cannot carry encrypted arguments.
//
// Only schema positions are visited. Property names and literal values in
// default, const, enum, and examples are user data and must remain intact.
func NormalizeTraeCLIToolParameters(schema string) string {
	if !gjson.Valid(schema) {
		return schema
	}
	return rewriteTraeCLIToolSchema(gjson.Parse(schema), false)
}

func rewriteTraeCLIToolSchema(node gjson.Result, schemaMap bool) string {
	if !node.IsObject() {
		return node.Raw
	}
	var out strings.Builder
	out.WriteByte('{')
	first, changed := true, false
	node.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if !schemaMap && name == "encrypted" {
			changed = true
			return true
		}
		raw := value.Raw
		if schemaMap {
			raw = rewriteTraeCLIToolSchema(value, false)
		} else {
			switch name {
			case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies":
				raw = rewriteTraeCLIToolSchema(value, true)
			case "additionalProperties", "unevaluatedProperties", "propertyNames", "contentSchema",
				"additionalItems", "unevaluatedItems", "contains", "not", "if", "then", "else":
				raw = rewriteTraeCLIToolSchema(value, false)
			case "items":
				if value.IsArray() {
					raw = rewriteTraeCLIToolSchemaArray(value)
				} else {
					raw = rewriteTraeCLIToolSchema(value, false)
				}
			case "allOf", "anyOf", "oneOf", "prefixItems":
				raw = rewriteTraeCLIToolSchemaArray(value)
			}
		}
		changed = changed || raw != value.Raw
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.WriteString(key.Raw)
		out.WriteByte(':')
		out.WriteString(raw)
		return true
	})
	out.WriteByte('}')
	if !changed {
		return node.Raw
	}
	return out.String()
}

func rewriteTraeCLIToolSchemaArray(node gjson.Result) string {
	if !node.IsArray() {
		return node.Raw
	}
	var out strings.Builder
	out.WriteByte('[')
	first, changed := true, false
	node.ForEach(func(_, value gjson.Result) bool {
		raw := rewriteTraeCLIToolSchema(value, false)
		changed = changed || raw != value.Raw
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.WriteString(raw)
		return true
	})
	out.WriteByte(']')
	if !changed {
		return node.Raw
	}
	return out.String()
}

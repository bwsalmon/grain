package gemini

import (
	"google.golang.org/genai"

	"github.com/bwsalmon/grain/v2/pkg/mcp"
)

// toFunctionDeclarations translates an MCP tools/list response into the
// function declarations the Gemini API expects -- the only bridge between
// the two: everything else about a tool (name, description, argument
// shape) is defined once in v2/mcp and carried through unchanged.
func toFunctionDeclarations(tools []mcp.ToolInfo) []*genai.FunctionDeclaration {
	decls := make([]*genai.FunctionDeclaration, 0, len(tools))
	for _, t := range tools {
		decls = append(decls, &genai.FunctionDeclaration{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  jsonSchemaToGenaiSchema(t.InputSchema),
		})
	}
	return decls
}

// jsonSchemaToGenaiSchema converts the plain JSON Schema every v2/mcp tool
// declares into genai's own Schema type. It only needs to understand the
// subset those tools actually use -- type, description, properties, items,
// required -- not JSON Schema in general.
func jsonSchemaToGenaiSchema(schema map[string]any) *genai.Schema {
	if schema == nil {
		return nil
	}
	out := &genai.Schema{}
	if t, ok := schema["type"].(string); ok {
		out.Type = jsonTypeToGenaiType(t)
	}
	if d, ok := schema["description"].(string); ok {
		out.Description = d
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		out.Properties = make(map[string]*genai.Schema, len(props))
		for name, raw := range props {
			if propSchema, ok := raw.(map[string]any); ok {
				out.Properties[name] = jsonSchemaToGenaiSchema(propSchema)
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		out.Items = jsonSchemaToGenaiSchema(items)
	}
	switch req := schema["required"].(type) {
	case []string:
		out.Required = req
	case []any:
		for _, r := range req {
			if s, ok := r.(string); ok {
				out.Required = append(out.Required, s)
			}
		}
	}
	return out
}

func jsonTypeToGenaiType(t string) genai.Type {
	switch t {
	case "object":
		return genai.TypeObject
	case "string":
		return genai.TypeString
	case "number":
		return genai.TypeNumber
	case "integer":
		return genai.TypeInteger
	case "boolean":
		return genai.TypeBoolean
	case "array":
		return genai.TypeArray
	default:
		return ""
	}
}

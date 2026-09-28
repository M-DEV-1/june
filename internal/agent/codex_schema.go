package agent

import (
	"strings"

	"google.golang.org/genai"
)

// codexTool is one function tool as the Responses API declares it; strict is sent explicitly because the API marks it required and defaulting it on would reject a schema that does not name every property as required.
type codexTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

// codexTools converts the GenAI function declarations the agent already registers into the shape the Responses API expects.
func codexTools(decls []*genai.FunctionDeclaration) []codexTool {
	out := make([]codexTool, 0, len(decls))
	for _, d := range decls {
		out = append(out, codexTool{Type: "function", Name: d.Name, Description: d.Description, Parameters: jsonSchema(d.Parameters)})
	}
	return out
}

// jsonSchema adapts a GenAI schema tree to the standard JSON Schema dictionary OpenAI expects.
func jsonSchema(s *genai.Schema) map[string]any {
	if s == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	node := jsonSchemaNode(s)
	if node["type"] == nil {
		node["type"] = "object"
	}
	// A tool that takes no arguments still declares an empty properties map, the shape OpenAI documents for a parameterless function and the one this path sent before the schema walk was factored out.
	if node["properties"] == nil {
		node["properties"] = map[string]any{}
	}
	return node
}

// jsonSchemaNode translates one node of the schema tree, recurring on OBJECT properties and ARRAY items.
func jsonSchemaNode(p *genai.Schema) map[string]any {
	if p == nil {
		return nil
	}
	node := map[string]any{}
	if p.Type != "" {
		node["type"] = strings.ToLower(string(p.Type))
	} else {
		node["type"] = "string"
	}
	if p.Description != "" {
		node["description"] = p.Description
	}
	if len(p.Enum) > 0 {
		node["enum"] = p.Enum
	}
	switch p.Type {
	case genai.TypeArray:
		if p.Items != nil {
			node["items"] = jsonSchemaNode(p.Items)
		}
	case genai.TypeObject:
		if len(p.Properties) > 0 {
			props := map[string]any{}
			for name, child := range p.Properties {
				props[name] = jsonSchemaNode(child)
			}
			node["properties"] = props
		}
		if len(p.Required) > 0 {
			node["required"] = p.Required
		}
	}
	return node
}

package config

import (
	"github.com/invopop/jsonschema"
)

// JSONSchema describes InstructionsRef as either a file path string or
// {prompt: <key>}, matching its custom UnmarshalYAML.
func (InstructionsRef) JSONSchema() *jsonschema.Schema {
	props := jsonschema.NewProperties()
	props.Set("prompt", &jsonschema.Schema{
		Type:        "string",
		Description: "Name of an entry in the top-level prompts: registry.",
	})
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string", Description: "Relative or absolute path to an instructions file."},
			{
				Type:                 "object",
				Properties:           props,
				Required:             []string{"prompt"},
				AdditionalProperties: jsonschema.FalseSchema,
			},
		},
	}
}

// JSONSchema describes PromptString as either a literal string or
// {prompt: <key>}, matching its custom UnmarshalYAML.
func (PromptString) JSONSchema() *jsonschema.Schema {
	props := jsonschema.NewProperties()
	props.Set("prompt", &jsonschema.Schema{
		Type:        "string",
		Description: "Name of an entry in the top-level prompts: registry.",
	})
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string", Description: "Literal text."},
			{
				Type:                 "object",
				Properties:           props,
				Required:             []string{"prompt"},
				AdditionalProperties: jsonschema.FalseSchema,
			},
		},
	}
}

// JSONSchema describes MemoryAgentConfig as either a memory profile name
// ("conversational" or a profiles.memory key) or an inline MemoryProfile object.
func (MemoryAgentConfig) JSONSchema() *jsonschema.Schema {
	r := &jsonschema.Reflector{
		FieldNameTag:              "yaml",
		DoNotReference:            true,
		AllowAdditionalProperties: false,
	}
	inline := r.Reflect(&MemoryProfile{})
	inline.Version = ""
	inline.ID = ""
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string", Description: "Name of a profiles.memory entry (or \"conversational\")."},
			inline,
		},
	}
}

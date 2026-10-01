package spec

import _ "embed"

// SchemaJSON is the JSON Schema (draft 2020-12) of the seed format, with
// descriptions written for whoever authors seeds, people or LLMs. Validate
// checks what a schema cannot: references between states, events,
// patterns and vocabularies, plus lint warnings.
//
//go:embed schema.json
var SchemaJSON []byte

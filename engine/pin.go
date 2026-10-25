// Package enginepin carries engine/engine.json into the binaries built from this repository, so
// adad's expected engine hash comes from its own build, never from a file beside the engine.
package enginepin

import _ "embed"

// JSON is engine/engine.json as it was when adad was built; internal/engine parses it.
//
//go:embed engine.json
var JSON []byte

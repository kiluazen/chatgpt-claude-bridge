// Package models holds the Codex catalog entries for the models the router
// serves outside OpenAI: one JSON file per model, in a directory named for the
// upstream that serves it (openrouter or bridge). The file's slug is the model
// name Codex sends and the upstream receives. Add a model by adding a file.
package models

import "embed"

//go:embed */*.json
var FS embed.FS

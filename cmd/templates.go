package cmd

import "embed"

// The scaffolding templates, compiled into the binary.
//
// Embedded rather than fetched because templates are free and always will be:
// there is nothing to gate, so there is no reason for a serving API. This
// makes `koolbase create` work offline and ties template versions to CLI
// releases, which already ship through get.koolbase.com.
//
// all: is required — without it, go:embed silently skips files and
// directories beginning with _ or ., and a Flutter project has .gitignore,
// .metadata, and analysis_options among its scaffolding.
//
//go:embed all:templates
var templatesFS embed.FS

// flutterSDKConstraint is the koolbase_flutter version written into a
// generated pubspec.yaml. Bump it when the SDK ships surface the templates
// depend on — the widgets landed in 10.3.0.
const flutterSDKConstraint = "^10.3.0"

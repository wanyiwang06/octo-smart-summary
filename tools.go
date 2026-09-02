//go:build tools

// Package tools records build-time-only dependencies so `go mod tidy` keeps them.
//
// gen_fence_delims.go derives the fence guard's delimiter and tag-name alphabets from
// Unicode properties, but it carries `//go:build ignore` (it is a `go:generate`
// program, not part of the package build). That build tag hides its imports from
// `go mod tidy`, so golang.org/x/text was recorded as `// indirect` — held in place
// only by whatever else happened to require it transitively.
//
// That is a latent break in a security-relevant path: if the transitive requirement
// goes away, `go mod tidy` drops x/text, the generator stops compiling, and the CI
// drift gate fails on a module error rather than on actual drift. Worse, the guarded
// alphabet is a function of the Unicode data version this module pins, so the
// dependency deserves to be explicit and reviewed at bump time rather than inherited.
//
// This file is never compiled into the binary — no build ever sets the `tools` tag.
// It exists purely so the import graph `go mod tidy` sees matches the one the
// repository actually depends on.
package tools

import (
	_ "golang.org/x/text/unicode/norm"
	_ "golang.org/x/text/unicode/runenames"
)

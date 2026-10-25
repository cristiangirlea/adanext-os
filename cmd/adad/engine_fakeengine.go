//go:build fakeengine

package main

// testEngineFlag lets the end-to-end test build (scripts/test-e2e.sh, -tags=fakeengine) run its
// fake engine: there -engine-sha256 names the expected hash. No release build sets the tag.
const testEngineFlag = true

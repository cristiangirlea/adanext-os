//go:build !fakeengine

package main

// testEngineFlag is false in every build but the fakeengine test build: only the embedded pin
// names the engine's hash.
const testEngineFlag = false

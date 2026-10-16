//go:build fakeengine && !windows

package main

import "errors"

func job(string) error { return errors.New("job observation is Windows-only") }

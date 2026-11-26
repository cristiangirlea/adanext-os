//go:build fakeengine

package source

import (
	"os"
	"path/filepath"
)

func init() {
	// The browser fixture sets this to the coordinator's private artifacts directory.
	if top := os.Getenv("ADANEXT_TEST_IMPORT_PERMISSION_TOP"); filepath.IsAbs(top) {
		importPermissionTop = func(string) string { return top }
	}
}

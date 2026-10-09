package interop

import (
	"os"
	"path/filepath"
	"testing"
)

// skipInterop skips the test because a prerequisite of the interop tests is missing: node, or a package under
// interop/js/node_modules. When the CI environment variable is set, it fails the test instead. CI installs every
// prerequisite (interop.yml runs npm ci in interop/js), so a missing one there is a broken workflow, and a skip
// would let the test pass without running.
func skipInterop(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf("interop prerequisite missing, and CI is set so the test fails instead of skipping: "+format, args...)
	}
	t.Skipf(format, args...)
}

// requireJSModule returns once the package name (for example "hyperdht" or "@hyperswarm/secret-stream") is installed
// under interop/js/node_modules. Otherwise it skips the test through skipInterop.
func requireJSModule(t *testing.T, name string) {
	t.Helper()
	pkg := filepath.Join("js", "node_modules", filepath.FromSlash(name), "package.json")
	if _, err := os.Stat(pkg); err != nil {
		skipInterop(t, "interop: interop/js has no %s; run npm ci in interop/js", name)
	}
}

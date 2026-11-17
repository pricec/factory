package foremanapp

import (
	"os"
	"testing"

	"github.com/pricec/factory/internal/ocusage"
)

// TestMain stubs the worker runs' opencode usage reader: no test may exec
// a real opencode for database discovery (tests whose spawner path needs
// usage install their own reader via BuildHarnessConfig).
func TestMain(m *testing.M) {
	opencodeUsageReader = func(string, int64) ([]ocusage.Session, error) { return nil, nil }
	os.Exit(m.Run())
}

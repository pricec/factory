package jobs

import (
	"os"
	"strings"
	"testing"
)

// docs/merging.md is executed by merge jobs for any repo, not only
// pricec/factory. Guard the factory-only assumptions that broke a
// speech-service merge.
func TestMergingDocNoFactoryOnlyAssumptions(t *testing.T) {
	b, err := os.ReadFile("../../docs/merging.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	for _, bad := range []string{"go run ./cmd/", "origin/master..", "--base master"} {
		if strings.Contains(doc, bad) {
			t.Errorf("docs/merging.md contains factory-only %q", bad)
		}
	}
	for _, want := range []string{
		`go build -o "$EV/cigate" ./cmd/cigate`,
		`"$EV/cigate" -repo "$REPO" -branch "$BRANCH" -sha "$FULL"`,
		`[ "$REPO" != pricec/factory ] || sudo -n true`,
		`BASE=$(`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/merging.md missing %q", want)
		}
	}
}

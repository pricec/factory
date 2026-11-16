package foremanapp_test

import (
	"testing"

	"github.com/pricec/factory/internal/claudecli"
	"github.com/pricec/factory/internal/foremanapp"
	"github.com/pricec/factory/internal/models"
)

// TestBrainModelPinWiresRunner boots a real paired foreman: the operator's
// env pin (FACTORY_FOREMAN_BRAIN_MODEL, carried as Config.BrainModel) reaches
// the brain runner unchanged, so a claude-* pin routes sessions to the
// claude runner. The default stays the GLM pin - remote foremen must not be
// pinned to claude until their binaries carry the claude runner.
func TestBrainModelPinWiresRunner(t *testing.T) {
	fac := newFactory(t)

	pinned, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	fe := newForeman(t, fac, pinned, func(c *foremanapp.Config) {
		c.BrainModel = "claude-opus-5-5"
	})
	if got := fe.a.Brain().Model; got != "claude-opus-5-5" {
		t.Errorf("brain model = %q, want the env pin", got)
	}
	if !claudecli.IsModel(fe.a.Brain().Model) {
		t.Errorf("env-pinned brain model %q does not route to the claude runner", fe.a.Brain().Model)
	}

	def, _, err := fac.a.Auth().IssuePairingToken(fac.ctx)
	if err != nil {
		t.Fatalf("IssuePairingToken: %v", err)
	}
	fd := newForeman(t, fac, def)
	if got := fd.a.Brain().Model; got != models.ForemanBrainDefault {
		t.Errorf("default brain model = %q, want the GLM pin %q", got, models.ForemanBrainDefault)
	}
	if claudecli.IsModel(fd.a.Brain().Model) {
		t.Errorf("default brain model routes to the claude runner; the GLM default must stay on opencode")
	}
}

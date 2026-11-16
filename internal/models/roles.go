package models

// Role is one panel-managed model role (spec §6.5, 2026-11-15
// amendment): the settings UI and GET/POST /v1/settings address roles by
// name; Default is the pinned fallback when neither a stored setting nor
// an env override applies.
type Role struct {
	Name    string
	Default string
}

// Roles is every managed role in pinned display order: the factory
// planner, the foreman brain, and the three worker tiers (spec §6.5).
var Roles = []Role{
	{"planner", PlannerDefault},
	{"foreman_brain", ForemanBrainDefault},
	{"worker_flash", WorkerFlash},
	{"worker_default", WorkerStandard},
	{"worker_hardest", WorkerHardest},
}

// RoleDefault returns the pinned default model for name (spec §6.5).
func RoleDefault(name string) (string, bool) {
	for _, r := range Roles {
		if r.Name == name {
			return r.Default, true
		}
	}
	return "", false
}

// RoleEnvVar is the env override for name (spec §6.5: every role is
// env-overridable); "" when name is not a role.
func RoleEnvVar(name string) string {
	switch name {
	case "planner":
		return "FACTORY_PLANNER_MODEL"
	case "foreman_brain":
		return "FACTORY_FOREMAN_BRAIN_MODEL"
	case "worker_flash":
		return "FACTORY_WORKER_FLASH_MODEL"
	case "worker_default":
		return "FACTORY_WORKER_MODEL"
	case "worker_hardest":
		return "FACTORY_WORKER_HARDEST_MODEL"
	default:
		return ""
	}
}

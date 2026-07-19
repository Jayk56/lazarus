package store

var maintenanceTransitions = map[string]map[string]bool{
	"new":      {"captured": true, "cancelled": true, "failed": true},
	"captured": {"stopping": true, "cancelled": true, "failed": true},
	"stopping": {"stopped": true, "failed": true},
	"stopped":  {"waiting": true, "failed": true},
	"waiting":  {"starting": true, "failed": true},
	"starting": {"completed": true, "failed": true},
	"failed":   {"new": true, "captured": true, "stopping": true, "stopped": true, "waiting": true, "starting": true},
}

var targetTransitions = map[string]map[string]bool{
	"captured": {"stopping": true, "skipped": true},
	"stopping": {"stopped": true, "failed": true, "skipped": true},
	"stopped":  {"starting": true, "skipped": true},
	"starting": {"healthy": true, "failed": true, "skipped": true},
	"failed":   {"stopping": true, "starting": true, "skipped": true},
}

func ValidMaintenanceState(state string) bool {
	if state == "completed" || state == "cancelled" {
		return true
	}
	_, ok := maintenanceTransitions[state]
	return ok
}

func ValidMaintenanceTransition(from, to string) bool { return maintenanceTransitions[from][to] }

func IsMaintenanceTerminal(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled"
}

func ValidTargetState(state string) bool {
	if state == "healthy" || state == "skipped" {
		return true
	}
	_, ok := targetTransitions[state]
	return ok
}

func ValidTargetTransition(from, to string) bool { return targetTransitions[from][to] }

func targetCompletionAllowed(original, current string) bool {
	if current == "skipped" {
		return true
	}
	switch original {
	case "running":
		return current == "healthy"
	case "stopped":
		return current == "stopped"
	default:
		return false
	}
}

func targetPhaseAllowed(maintenanceState, targetState string) bool {
	switch targetState {
	case "stopping", "stopped":
		return maintenanceState == "stopping"
	case "starting", "healthy":
		return maintenanceState == "starting"
	case "failed":
		return maintenanceState == "stopping" || maintenanceState == "starting"
	default:
		return false
	}
}

package service

import (
	"cps/internal/model"
	"testing"
)

func TestLifecycleTransitionTable(t *testing.T) {
	valid := [][2]model.State{
		{model.StateCreating, model.StateRunning},
		{model.StateRunning, model.StatePaused},
		{model.StatePaused, model.StateRunning},
		{model.StateRunning, model.StateHibernating},
		{model.StateHibernating, model.StateHibernated},
		{model.StateHibernated, model.StateStarting},
		{model.StateStopped, model.StateStarting},
		{model.StateStopped, model.StateRunning}, // reconciler repairs stale persisted state
		{model.StateFailed, model.StateRunning},  // reconciler repairs stale persisted state
		{model.StateStopping, model.StateStopped},
		{model.StateDecommissioning, model.StateDecommissioned},
	}
	for _, pair := range valid {
		if !allowedTransition(pair[0], pair[1]) {
			t.Errorf("expected transition %s -> %s to be allowed", pair[0], pair[1])
		}
	}
	invalid := [][2]model.State{
		{model.StateRunning, model.StateStarting},
		{model.StateDecommissioned, model.StateRunning},
		{model.StateHibernated, model.StatePaused},
		{model.StateCreating, model.StateStopped},
	}
	for _, pair := range invalid {
		if allowedTransition(pair[0], pair[1]) {
			t.Errorf("expected transition %s -> %s to be rejected", pair[0], pair[1])
		}
	}
}

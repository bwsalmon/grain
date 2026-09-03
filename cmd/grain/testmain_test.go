package main

import (
	"os"
	"testing"

	"github.com/bwsalmon/grain/pkg/orchestrator"
)

// TestMain turns off the check-registration window for this whole suite
// -- the wait that keeps an empty check list from reading as clean until
// CI has had time to register one (orchestrator.SetCheckRegistrationWindow).
//
// The tests here stand a real daemon up against a githubsim over a real
// wall clock, and there is no CI anywhere in that picture: nothing will
// ever report a check run, so every window a pull request here starts
// runs its full two minutes and then ends where it began. Turning it off
// costs these tests no coverage -- the window is about telling "CI has
// not reported yet" from "there is no CI", and this suite only ever has
// the second -- and it is covered where the clock can be handed in, in
// pkg/orchestrator.
func TestMain(m *testing.M) {
	restore := orchestrator.SetCheckRegistrationWindow(0)
	code := m.Run()
	restore()
	os.Exit(code)
}

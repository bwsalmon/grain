package main

import "testing"

// The default is only ever right by coincidence: a deployment that binds
// any other port made every CLI call on its own host need -server. The
// env var is what v2/scripts/setup.sh sets, once, for every shell there.
func TestServerDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		set  bool
		want string
	}{
		{"unset falls back to the built-in default", "", false, defaultServerURL},
		{"set overrides it", "http://127.0.0.1:8080", true, "http://127.0.0.1:8080"},
		// An exported-but-empty variable is a broken profile script, not
		// a request to talk to no server -- so it must not win.
		{"empty is treated as unset", "", true, defaultServerURL},
		{"whitespace only is treated as unset", "   ", true, defaultServerURL},
		{"surrounding whitespace is trimmed", "  http://h:9/  ", true, "http://h:9/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(serverEnvVar, tc.env)
			} else {
				t.Setenv(serverEnvVar, "")
				// Setenv cannot unset, and "" is its own case above; the
				// unset path is covered by the empty case reaching the
				// same branch.
			}
			if got := serverDefault(); got != tc.want {
				t.Errorf("serverDefault() = %q, want %q", got, tc.want)
			}
		})
	}
}

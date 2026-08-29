package version

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

func TestStringCarriesSchemaVersion(t *testing.T) {
	s := String()
	want := fmt.Sprintf("%d+", model.SchemaVersion)
	if !strings.HasPrefix(s, want) {
		t.Fatalf("String() = %q, want prefix %q", s, want)
	}
	if strings.TrimPrefix(s, want) == "" {
		t.Fatalf("String() = %q, want a revision (or \"unknown\") after the +", s)
	}
}

// Revision itself cannot be pinned to a value -- go test's own -buildvcs
// default stamps it when this runs inside a git checkout, and leaves it
// blank otherwise. What is testable is the shape: short, and never
// truncated mid escape sequence.
func TestRevisionIsShortWhenPresent(t *testing.T) {
	r := Revision()
	if r == "" {
		return
	}
	if len(r) > revisionLen+len("-dirty") {
		t.Fatalf("Revision() = %q, longer than expected", r)
	}
}

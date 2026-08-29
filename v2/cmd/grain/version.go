// version.go implements `grain version`: prints pkg/version.String() --
// the store schema version this build expects plus the git commit it
// was built from (bwsalmon/agents#397) -- and exits. Unlike
// `grain schema-version` (schemaversion.go), which prints only the bare
// number setup.sh compares against a marker on disk, this is for a
// human (or an operator's own tooling) checking exactly what build is
// running.
package main

import (
	"flag"
	"fmt"

	"github.com/bwsalmon/grain/v2/pkg/version"
)

func versionCmd(args []string) {
	fs := flag.NewFlagSet("grain version", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return
	}
	fmt.Println(version.String())
}

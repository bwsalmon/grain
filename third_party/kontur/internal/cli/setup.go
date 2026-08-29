package cli

import (
	"context"
	"flag"
	"io"

	staticconfig "github.com/bwsalmon/kontur/deploy/static-kubelet"
	"github.com/bwsalmon/kontur/internal/setup"
)

func runSetup(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("konturctl setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	kubeletVersion := fs.String("kubelet-version", "", "kubelet release to install (default: install.sh's own default, v1.31.0)")
	staticPodPath := fs.String("static-pod-path", "", "directory kubelet watches for static pod manifests (default: /etc/kubernetes/manifests)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	return setup.Install(ctx, staticconfig.FS, setup.Options{
		KubeletVersion: *kubeletVersion,
		StaticPodPath:  *staticPodPath,
	}, stdout, stderr)
}

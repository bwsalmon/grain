// Package staticconfig embeds install.sh and the config files it installs
// (containerd-config.toml, kubelet-config.yaml, kubelet.service,
// cni/10-kontur.conflist), so `konturctl setup` can carry the whole static
// kubelet setup around inside a single binary instead of requiring a
// checkout of this repo on the target node. See README.md for what each
// file does; this package changes nothing about their content, it only
// makes them reachable from Go code elsewhere in the module.
package staticconfig

import "embed"

//go:embed install.sh containerd-config.toml kubelet-config.yaml kubelet.service cni/10-kontur.conflist
var FS embed.FS

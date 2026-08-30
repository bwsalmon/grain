packer {
  required_plugins {
    qemu = {
      version = ">= 1.1.0"
      source  = "github.com/hashicorp/qemu"
    }
  }
}

# See README.md in this directory for what this template builds and why.

variable "operator_ssh_public_key" {
  type        = string
  description = "The public half of the operator SSH keypair (grain/pkg/kontur's SSHRunner uses the private half). Baked into the debian user's authorized_keys at build time -- see README.md, 'Why the key is baked in, not injected'. Never the private key; this variable's value is not a secret."
}

variable "sandbox_setup_script" {
  type        = string
  default     = ""
  description = "Contents (not a path) of an optional shell script run against the guest near the end of provision.sh, after the built-in provisioning and before the operator-key/cloud-init finalization -- see provision.sh's own comment on that section. Empty (the default) runs nothing extra. May itself be a secret-free thing only -- like provision.sh's own rule, nothing here should bake a credential into the image."
}

variable "base_image_url" {
  type        = string
  default     = "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"
  description = "Same Debian 12 generic-cloud qcow2 URL terraform/gcp/variables.tf's debian_image_url and docs/runbook.md's first-time setup both use for v1's own sandbox base image."
}

variable "base_image_checksum" {
  type        = string
  default     = "file:https://cloud.debian.org/images/cloud/bookworm/latest/SHA512SUMS"
  description = "Debian publishes a signed checksum manifest alongside the image; Packer's \"file:<url>\" form downloads it and looks up base_image_url's own entry rather than pinning one hash here that silently goes stale."
}

variable "image_name" {
  type        = string
  default     = "kontur-guest"
  description = "Base name for the output qcow2 (build.sh appends a git-sha/date suffix before publishing -- see README.md)."
}

variable "disk_size" {
  type    = string
  default = "16G"
}

variable "memory" {
  type    = number
  default = 2048
}

variable "cpus" {
  type    = number
  default = 2
}

variable "output_directory" {
  type    = string
  default = "output"
}

# The following two are populated by build.sh, not hand-supplied: an
# ephemeral NoCloud seed (a temporary keypair + user-data/meta-data) that
# gives Packer itself SSH access to run provisioner.sh against the stock
# base image during the build. Neither survives the build -- provision.sh's
# last step overwrites the debian user's authorized_keys with
# operator_ssh_public_key alone, and build.sh deletes the seed directory and
# ephemeral key afterward. See README.md, "How the build reaches the VM at
# all".
variable "packer_ssh_private_key_file" {
  type = string
}

variable "seed_dir" {
  type = string
}

source "qemu" "kontur-guest" {
  iso_url          = var.base_image_url
  iso_checksum     = var.base_image_checksum
  disk_image       = true
  use_backing_file = false

  output_directory = var.output_directory
  vm_name          = "${var.image_name}.qcow2"
  format           = "qcow2"
  disk_size        = var.disk_size
  disk_interface   = "virtio"
  net_device       = "virtio-net"

  accelerator = "kvm"
  headless    = true
  cpus        = var.cpus
  memory      = var.memory

  # The NoCloud seed above is delivered the same way any qemu/libvirt
  # NoCloud datasource is: a small ISO labelled "cidata" attached as a
  # second CD-ROM. cloud-init on the base image finds it on first boot with
  # no further configuration.
  cd_label = "cidata"
  cd_files = ["${var.seed_dir}/user-data", "${var.seed_dir}/meta-data"]

  communicator          = "ssh"
  ssh_username           = "debian"
  ssh_private_key_file   = var.packer_ssh_private_key_file
  ssh_timeout            = "15m"
  shutdown_command       = "sudo shutdown -h now"
  shutdown_timeout       = "5m"
}

build {
  sources = ["source.qemu.kontur-guest"]

  provisioner "shell" {
    execute_command = "{{ .Vars }} sudo -E sh -c '{{ .Path }}'"
    environment_vars = [
      "OPERATOR_SSH_PUBLIC_KEY=${var.operator_ssh_public_key}",
      "SANDBOX_SETUP_SCRIPT=${var.sandbox_setup_script}",
    ]
    script            = "${path.root}/provision.sh"
    expect_disconnect = true
  }
}

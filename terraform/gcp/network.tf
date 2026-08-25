# One VPC, one subnet, one firewall rule. The host exposes exactly one
# inbound port -- 22 -- and by default only to Google's IAP range, which is
# the "one externally reachable port" property docs/design.md asks for.

locals {
  network_name    = var.create_network ? google_compute_network.this[0].name : var.network_name
  subnetwork_name = var.create_network ? google_compute_subnetwork.this[0].name : var.subnetwork_name
  host_tag        = "${var.name_prefix}-host"
}

resource "google_compute_network" "this" {
  count                   = var.create_network ? 1 : 0
  name                    = "${var.name_prefix}-net"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "this" {
  count                    = var.create_network ? 1 : 0
  name                     = "${var.name_prefix}-subnet"
  region                   = var.region
  network                  = google_compute_network.this[0].id
  ip_cidr_range            = var.subnet_cidr
  private_ip_google_access = true
}

resource "google_compute_firewall" "ssh" {
  count         = var.create_network ? 1 : 0
  name          = "${var.name_prefix}-allow-ssh"
  network       = google_compute_network.this[0].name
  direction     = "INGRESS"
  source_ranges = var.ssh_source_ranges
  target_tags   = [local.host_tag]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

# Egress is open: the host fetches Debian packages, the base image, grain
# itself, and talks to the GitHub and Anthropic APIs. Sandbox egress is a
# separate question, enforced inside the host by `grain host egress`.

resource "google_compute_router" "nat" {
  count   = var.create_network && var.enable_cloud_nat ? 1 : 0
  name    = "${var.name_prefix}-router"
  region  = var.region
  network = google_compute_network.this[0].id
}

resource "google_compute_router_nat" "nat" {
  count                              = var.create_network && var.enable_cloud_nat ? 1 : 0
  name                               = "${var.name_prefix}-nat"
  region                             = var.region
  router                             = google_compute_router.nat[0].name
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"

  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }
}

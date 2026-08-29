# One VPC, one subnet, two firewall rules. Unlike terraform/gcp/network.tf's
# v1 host, this one is not reachable on any port from the open internet at
# all: SSH is IAP-tunnel-only (same as v1), and the UI is reachable only
# from Google's own load-balancing/health-check ranges, since the actual
# public entry point is the IAP-protected HTTPS load balancer in iap.tf,
# not the instance itself.

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

# 130.211.0.0/22 and 35.191.0.0/16 are Google's own health-check and
# load-balancer-to-backend source ranges for a global external HTTPS
# load balancer (iap.tf's google_compute_backend_service) -- not the
# public internet, which is exactly what lets ui_port bind 0.0.0.0
# safely (variables.tf's own comment on ui_port). No source range here
# is ever widened by an operator the way ssh_source_ranges might be:
# the load balancer, gated by IAP, is the only intended path to this
# port.
resource "google_compute_firewall" "lb_to_ui" {
  count         = var.create_network ? 1 : 0
  name          = "${var.name_prefix}-allow-lb-to-ui"
  network       = google_compute_network.this[0].name
  direction     = "INGRESS"
  source_ranges = ["130.211.0.0/22", "35.191.0.0/16"]
  target_tags   = [local.host_tag]

  allow {
    protocol = "tcp"
    ports    = [tostring(var.ui_port)]
  }
}

# Egress is open: the host fetches Debian packages, Docker images, the
# grain source, and talks to the GitHub and Gemini APIs.

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

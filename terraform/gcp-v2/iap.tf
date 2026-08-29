# Exposes the staging UI at dns.tf's local.dns_name over HTTPS, protected
# by Identity-Aware Proxy -- bwsalmon/agents#394's own "protected by iap."
#
# IAP for a web UI (as opposed to the IAP *TCP* tunnel ssh_source_ranges
# already gives SSH, in network.tf) is a property of a global external
# Application Load Balancer's backend service, not of the instance
# itself: Google's edge terminates TLS, requires a Google sign-in and an
# roles/iap.httpsResourceAccessor grant before a request ever reaches the
# backend, and only then forwards it, over plain HTTP, to the instance's
# *internal* IP -- which is why google_compute_instance.host needs no
# external IP of its own (network.tf's assign_external_ip) and why
# ui_port only has to be reachable from Google's own load-balancer
# ranges (network.tf's lb_to_ui firewall rule), not the internet.
#
#   browser ──HTTPS, Google sign-in──▶ IAP (on the backend service)
#                                           │ authorized (iap_members)
#                                           ▼
#                                   plain HTTP, internal IP
#                                           │
#                                           ▼
#                              google_compute_instance.host:ui_port

resource "google_compute_health_check" "ui" {
  name = "${var.name_prefix}-ui-health"

  http_health_check {
    port         = var.ui_port
    request_path = "/"
  }
}

# Unmanaged: one fixed instance, not a template a group would scale --
# this deployment is one VM, on purpose (variables.tf's own machine_type
# comment on why v2 needs no fleet).
resource "google_compute_instance_group" "host" {
  name      = "${var.name_prefix}-ig"
  zone      = var.zone
  instances = [google_compute_instance.host.self_link]

  named_port {
    name = "http"
    port = var.ui_port
  }
}

resource "google_compute_backend_service" "ui" {
  name                  = "${var.name_prefix}-ui-backend"
  protocol              = "HTTP"
  port_name             = "http"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  health_checks         = [google_compute_health_check.ui.id]

  backend {
    group = google_compute_instance_group.host.self_link
  }

  iap {
    enabled              = true
    oauth2_client_id     = local.iap_client_id
    oauth2_client_secret = local.iap_client_secret
  }

  depends_on = [google_project_service.iap]

  lifecycle {
    precondition {
      condition     = local.iap_client_id != "" && local.iap_client_secret != ""
      error_message = "No IAP OAuth client available: set create_iap_brand = true (with iap_brand_support_email), or create_iap_brand = false with iap_client_id/iap_client_secret both set to an existing client."
    }
  }
}

resource "google_compute_url_map" "ui" {
  name            = "${var.name_prefix}-ui-map"
  default_service = google_compute_backend_service.ui.id
}

resource "google_compute_managed_ssl_certificate" "ui" {
  name = "${var.name_prefix}-ui-cert"

  managed {
    domains = [local.dns_name]
  }
}

resource "google_compute_target_https_proxy" "ui" {
  name             = "${var.name_prefix}-ui-proxy"
  url_map          = google_compute_url_map.ui.id
  ssl_certificates = [google_compute_managed_ssl_certificate.ui.id]
}

resource "google_compute_global_forwarding_rule" "ui" {
  name                  = "${var.name_prefix}-ui-fr"
  ip_address            = google_compute_global_address.lb.address
  ip_protocol           = "TCP"
  port_range            = "443"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  target                = google_compute_target_https_proxy.ui.id
}

# --- the OAuth brand/client IAP itself needs ---------------------------
#
# Off by default -- see variables.tf's create_iap_brand for why: a
# project may have at most one brand, ever, and (as of this writing) the
# provider itself warns this resource no longer functions as intended
# for a genuinely new brand, following the IAP OAuth Admin API's own
# deprecation. Left in place for a project where it still works, or
# where GCP's guidance has moved back onto a Terraform-managed path by
# the time you are reading this -- check current GCP documentation
# rather than trusting this comment's own age.

resource "google_iap_brand" "this" {
  count             = var.create_iap_brand ? 1 : 0
  support_email     = var.iap_brand_support_email
  application_title = var.iap_brand_application_title
  project           = var.project_id

  depends_on = [google_project_service.iap]

  lifecycle {
    precondition {
      condition     = var.iap_brand_support_email != ""
      error_message = "iap_brand_support_email is required when create_iap_brand is true."
    }
  }
}

resource "google_iap_client" "this" {
  count        = var.create_iap_brand ? 1 : 0
  brand        = google_iap_brand.this[0].name
  display_name = "${var.name_prefix} UI"
}

locals {
  iap_client_id     = var.create_iap_brand ? google_iap_client.this[0].client_id : var.iap_client_id
  iap_client_secret = var.create_iap_brand ? google_iap_client.this[0].secret : var.iap_client_secret
}

# Who may actually reach the UI once they are through Google's own sign-in
# -- see variables.tf's iap_members. Scoped to this one backend service,
# not the whole project, so a broader roles/iap.httpsResourceAccessor
# grant elsewhere (another IAP-protected app in the same project) does
# not also open this one.
resource "google_iap_web_backend_service_iam_member" "members" {
  for_each            = toset(var.iap_members)
  project             = var.project_id
  web_backend_service = google_compute_backend_service.ui.name
  role                = "roles/iap.httpsResourceAccessor"
  member              = each.value
}

resource "google_project_service" "iap" {
  project            = var.project_id
  service            = "iap.googleapis.com"
  disable_on_destroy = false
}

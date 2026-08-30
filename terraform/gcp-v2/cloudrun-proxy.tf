# A Cloud Run based alternative to exposing google_compute_instance.host's
# own instance group as the load balancer's backend (instance.tf, iap.tf)
# -- see variables.tf's use_cloudrun_iap_proxy for why a deployment might
# want this instead. Nothing here is created unless that flag is on.
#
#   iap.tf's load balancer ──Serverless NEG──▶ this Cloud Run service
#                                                   │ socat, blind TCP forward
#                                                   ▼
#                               Serverless VPC Access connector
#                                                   │
#                                                   ▼
#                         google_compute_instance.host's internal IP:ui_port
#
# IAP itself does not move: it still lives on
# google_compute_backend_service.ui in iap.tf, checking the same
# iap_members grant, regardless of what backs that backend service.
#
# The Cloud Run service's own default run.app URL is deliberately not a
# second way in: ingress below is INTERNAL_LOAD_BALANCER-only, so the
# load balancer's Serverless NEG is the only path a request can take to
# reach this container. A Google-managed load balancer talking to a
# serverless NEG backend needs no google_cloud_run_v2_service_iam_member
# of its own either -- that integration authenticates the load balancer
# to Cloud Run automatically, unlike a request against the public
# run.app URL, which is exactly what INTERNAL_LOAD_BALANCER ingress
# forecloses.

resource "google_project_service" "run" {
  count              = var.use_cloudrun_iap_proxy ? 1 : 0
  project            = var.project_id
  service            = "run.googleapis.com"
  disable_on_destroy = false
}

resource "google_project_service" "vpcaccess" {
  count              = var.use_cloudrun_iap_proxy ? 1 : 0
  project            = var.project_id
  service            = "vpcaccess.googleapis.com"
  disable_on_destroy = false
}

resource "google_vpc_access_connector" "proxy" {
  count         = var.use_cloudrun_iap_proxy && var.create_network ? 1 : 0
  name          = "${var.name_prefix}-connector"
  region        = var.region
  network       = google_compute_network.this[0].name
  ip_cidr_range = var.cloudrun_connector_cidr

  depends_on = [google_project_service.vpcaccess]
}

locals {
  # Empty when use_cloudrun_iap_proxy is off, the same "guard on the
  # flag, not on this being non-empty" convention dns.tf's own dns_name
  # local follows.
  cloudrun_connector_id = !var.use_cloudrun_iap_proxy ? "" : (
    var.create_network ? google_vpc_access_connector.proxy[0].id : "projects/${var.project_id}/locations/${var.region}/connectors/${var.cloudrun_connector_name}"
  )
}

resource "google_cloud_run_v2_service" "proxy" {
  count    = var.use_cloudrun_iap_proxy ? 1 : 0
  name     = "${var.name_prefix}-iap-proxy"
  location = var.region
  labels   = var.labels

  # Blocks the service's own default run.app URL from the internet --
  # the only path in is through the load balancer's Serverless NEG
  # (below), where IAP already sits. Without this, the run.app URL would
  # be a second, un-IAP'd way to reach the VM.
  ingress = "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"

  template {
    containers {
      image = var.cloudrun_proxy_image

      # Blind TCP forward, port 8080 (Cloud Run's own default container
      # port, so no need to also set a ports block) to the VM's internal
      # IP -- see this file's header for why the proxy does not need to
      # understand HTTP at all.
      command = ["/bin/sh", "-c"]
      args = [
        "exec socat -d TCP-LISTEN:8080,fork,reuseaddr TCP:${google_compute_instance.host.network_interface[0].network_ip}:${var.ui_port}"
      ]

      resources {
        limits = {
          cpu    = "1"
          memory = "256Mi"
        }
      }
    }

    vpc_access {
      connector = local.cloudrun_connector_id
      egress    = "PRIVATE_RANGES_ONLY"
    }

    scaling {
      min_instance_count = var.cloudrun_proxy_min_instances
      max_instance_count = var.cloudrun_proxy_max_instances
    }
  }

  depends_on = [google_project_service.run, google_project_service.vpcaccess]
}

resource "google_compute_region_network_endpoint_group" "cloudrun_proxy" {
  count                 = var.use_cloudrun_iap_proxy ? 1 : 0
  name                  = "${var.name_prefix}-cloudrun-neg"
  region                = var.region
  network_endpoint_type = "SERVERLESS"

  cloud_run {
    service = google_cloud_run_v2_service.proxy[0].name
  }
}

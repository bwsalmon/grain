output "instance_name" {
  value       = google_compute_instance.host.name
  description = "Host VM name, for gcloud commands."
}

output "zone" {
  value       = var.zone
  description = "Zone the host runs in."
}

output "project_id" {
  value       = var.project_id
  description = "Convenience for scripts that read the project from state rather than re-parsing tfvars."
}

# The three below are null when expose_ui_publicly is off: there is no
# load balancer, so there is no address, no name and no URL. Read them
# null-safe (`terraform output -raw url 2>/dev/null || true`) -- -raw
# errors on a null rather than printing nothing.
output "url" {
  value       = var.expose_ui_publicly ? "https://${local.dns_name}/" : null
  description = "The staging environment's fixed URL -- reachable once signed in as one of iap_members. Null when expose_ui_publicly is off; use tunnel_command instead."
}

output "dns_name" {
  value       = var.expose_ui_publicly ? local.dns_name : null
  description = "The fixed DNS name itself, with no scheme -- what the managed SSL certificate is issued for. Null when expose_ui_publicly is off."
}

output "load_balancer_ip" {
  value       = var.expose_ui_publicly ? google_compute_global_address.lb[0].address : null
  description = "The reserved static IP the DNS name resolves to. Stable across a redeploy of the host VM. Null when expose_ui_publicly is off."
}

output "tunnel_command" {
  value       = "gcloud compute start-iap-tunnel ${google_compute_instance.host.name} ${var.ui_port} --local-host-port=localhost:${var.ui_port} --zone ${var.zone} --project ${var.project_id}"
  description = "Forward the UI to localhost over IAP's TCP tunnel, then open http://localhost:<ui_port>. The whole access path when expose_ui_publicly is off, and a way in past a broken load balancer when it is on."
}

output "host_service_account" {
  value       = google_service_account.host.email
  description = "The identity the host VM runs as."
}

output "agent_service_account" {
  value       = local.agent_account_needed ? google_service_account.agent[0].email : null
  description = "The narrow identity pkg/capability/gcpkey mints per-task keys for, when configured."
}

output "minter_service_account" {
  value       = local.agent_account_needed ? google_service_account.minter[0].email : null
  description = "The identity push-secrets.sh mints a key for, to seed the host with -- see that script."
}

output "ssh_command" {
  value       = "gcloud compute ssh ${google_compute_instance.host.name} --zone ${var.zone} --project ${var.project_id} --tunnel-through-iap"
  description = "How to get a shell on the host despite it having no external IP and no open SSH port."
}

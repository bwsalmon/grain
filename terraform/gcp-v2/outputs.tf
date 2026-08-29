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

output "url" {
  value       = "https://${local.dns_name}/"
  description = "The staging environment's fixed URL -- reachable once signed in as one of iap_members."
}

output "dns_name" {
  value       = local.dns_name
  description = "The fixed DNS name itself, with no scheme -- what the managed SSL certificate is issued for."
}

output "load_balancer_ip" {
  value       = google_compute_global_address.lb.address
  description = "The reserved static IP the DNS name resolves to. Stable across a redeploy of the host VM."
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

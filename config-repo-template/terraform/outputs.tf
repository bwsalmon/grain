output "instance_name" {
  value       = google_compute_instance.host.name
  description = "Host VM name, for gcloud commands."
}

output "zone" {
  value       = var.zone
  description = "Zone the host runs in."
}

output "external_ip" {
  value       = var.assign_external_ip ? google_compute_instance.host.network_interface[0].access_config[0].nat_ip : null
  description = "Public address, when one was requested."
}

output "host_service_account" {
  value       = google_service_account.host.email
  description = "The identity the host runs as. Grant it more by editing vm_service_account_roles."
}

output "agent_service_account" {
  value       = local.agent_account_needed ? google_service_account.agent[0].email : null
  description = "The narrow identity sandboxed agents are meant to impersonate, when configured."
}

output "ssh_command" {
  value       = "gcloud compute ssh ${google_compute_instance.host.name} --zone ${var.zone} --project ${var.project_id} --tunnel-through-iap"
  description = "How to get a shell on the host with the default IAP-only firewall."
}

output "project_id" {
  value       = var.project_id
  description = "Convenience for CI, which reads the project from state rather than re-parsing tfvars."
}

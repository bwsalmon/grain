terraform {
  required_version = ">= 1.6.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.8"
    }
  }

  # Deliberately empty: CI fills it in with
  #   terraform init -backend-config=../config/backend.hcl
  # so the bucket name lives in the repo as configuration, not here.
  backend "gcs" {}
}

provider "google" {
  project = var.project_id
  region  = var.region
  zone    = var.zone
}

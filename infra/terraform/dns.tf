# DNS-as-code: a Cloud DNS managed zone for the platform domain + A records for every public
# host, pointing at the reserved ingress IP. After the first apply, delegate the domain at the
# parent DNS (e.g. fmind.ai): create NS records for var.domain using `terraform output
# dns_name_servers`. Toggle off (var.manage_dns=false) when DNS lives elsewhere.
resource "google_dns_managed_zone" "platform" {
  count = var.manage_dns ? 1 : 0

  name        = var.dns_zone_name
  dns_name    = "${var.domain}."
  description = "Fgentic platform hosts (managed by Terraform)"
}

locals {
  # Apex (well-known delegation) + every public host the Gateway serves.
  dns_hosts = var.manage_dns ? toset(["", "chat.", "matrix.", "auth.", "id.", "grafana."]) : toset([])
}

resource "google_dns_record_set" "platform" {
  for_each = local.dns_hosts

  managed_zone = google_dns_managed_zone.platform[0].name
  name         = "${each.value}${google_dns_managed_zone.platform[0].dns_name}"
  type         = "A"
  ttl          = 300
  rrdatas      = [google_compute_address.ingress_ip.address]
}

output "dns_name_servers" {
  value       = var.manage_dns ? google_dns_managed_zone.platform[0].name_servers : []
  description = "Delegate var.domain at the parent DNS with NS records pointing here"
}

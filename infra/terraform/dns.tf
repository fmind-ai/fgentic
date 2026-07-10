# Optional DNS-as-code: A records for the platform hosts pointing at the reserved ingress IP.
# Requires an existing Cloud DNS managed zone for the apex domain (var.dns_zone_name); leave
# var.manage_dns = false when DNS lives elsewhere (registrar, Cloudflare, ...).
locals {
  dns_hosts = var.manage_dns ? toset(["", "chat.", "matrix.", "auth."]) : toset([])
}

data "google_dns_managed_zone" "platform" {
  count = var.manage_dns ? 1 : 0
  name  = var.dns_zone_name
}

resource "google_dns_record_set" "platform" {
  for_each = local.dns_hosts

  managed_zone = data.google_dns_managed_zone.platform[0].name
  name         = "${each.value}${data.google_dns_managed_zone.platform[0].dns_name}"
  type         = "A"
  ttl          = 300
  rrdatas      = [google_compute_address.ingress_ip.address]
}

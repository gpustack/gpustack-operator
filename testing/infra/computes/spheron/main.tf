resource "spheron_instance" "this" {
  request_key = var.request_key
  request_json = jsonencode(merge({
    provider        = var.instance_type.provider
    offerId         = var.instance_type.offer_id
    gpuType         = var.instance_type.gpu_type
    gpuCount        = var.instance_type.gpu_count
    region          = var.instance_type.region
    operatingSystem = var.instance_type.operating_system
    instanceType    = var.instance_type.preemptible ? "SPOT" : "DEDICATED"
    ssh_public_key  = trimspace(file(pathexpand(var.ssh_public_key)))
    name            = var.name_prefix
    }, var.team_id == null ? {} : { teamId = var.team_id },
    var.cloud_init == null ? {} : { cloudInit = merge(
      { for k, v in var.cloud_init : k => v if k != "writeFiles" && v != null },
      var.cloud_init.writeFiles == null ? {} : {
        writeFiles = [for file in var.cloud_init.writeFiles : {
          for k, v in file : k => v if v != null
        }]
      },
    ) },
    length(var.volume_ids) == 0 ? {} : { volumeIds = var.volume_ids },
    var.max_hourly_rate == null ? {} : { maxHourlyRate = var.max_hourly_rate },
  ))
}

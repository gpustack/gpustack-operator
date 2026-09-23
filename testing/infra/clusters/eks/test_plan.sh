#!/usr/bin/env bash
set -euo pipefail

# This regression check needs AWS credentials and an initialized cluster state.
# An empty state cannot reveal an unintended replacement in the legacy plan.
if ! terraform state list | grep -q '^module\.eks\.'; then
  echo 'test_plan.sh requires an initialized EKS Terraform state' >&2
  exit 1
fi

plan_file="$(mktemp)"
trap 'rm -f "$plan_file"' EXIT

terraform plan -out="$plan_file" \
  -var='region=us-east-1' \
  -var='gpu_instance_types={}' \
  -var='cpu_instance_types={topology-a={instance_types=["t3a.medium"],node_count=2,availability_zone_index=0},topology-b={instance_types=["t3a.medium"],node_count=2,availability_zone_index=1}}' \
  -var='efa_enabled=false' \
  -var='switch_kube_context=false' >/dev/null

terraform show -json "$plan_file" | jq -e '
  [.resource_changes[] | select(.type == "aws_eks_node_group") | .change.after] as $groups |
  ($groups | length == 2) and
  ($groups | all(.scaling_config[0] | .desired_size == 2 and .min_size == 2 and .max_size == 2)) and
  (.planned_values.outputs.selected_zones.value | .["topology-a"] != .["topology-b"]) and
  ([.resource_changes[] | select(.address == "aws_eks_pod_identity_association.topograph_aws[0]" or .address == "aws_iam_role.topograph_aws[0]" or .address == "aws_iam_role_policy.topograph_aws[0]")] | length == 0)
'

legacy_plan_file="$(mktemp)"
trap 'rm -f "$plan_file" "$legacy_plan_file"' EXIT
terraform plan -out="$legacy_plan_file" -var='switch_kube_context=false' >/dev/null
terraform show -json "$legacy_plan_file" | jq -e '
  ([.resource_changes[].change.actions | select(index("delete") and index("create"))] | length == 0) and
  ([.resource_changes[] | select(.address == "aws_eks_pod_identity_association.topograph_aws[0]" or .address == "aws_iam_role.topograph_aws[0]" or .address == "aws_iam_role_policy.topograph_aws[0]")] | length == 0)
'

identity_plan_file="$(mktemp)"
trap 'rm -f "$plan_file" "$legacy_plan_file" "$identity_plan_file"' EXIT
terraform plan -out="$identity_plan_file" -var='topograph_aws_pod_identity_enabled=true' -var='switch_kube_context=false' >/dev/null
terraform show -json "$identity_plan_file" | jq -e '
  ([.resource_changes[] | select(.address == "aws_eks_pod_identity_association.topograph_aws[0]") | .change.after] | length == 1) and
  ([.resource_changes[] | select(.address == "aws_eks_pod_identity_association.topograph_aws[0]") | .change.after | .namespace == "gpustack-system" and .service_account == "gpustack-operator-topograph"] | all) and
  ([.resource_changes[] | select(.type == "aws_launch_template") | .change.after.metadata_options[0] | .http_endpoint == "enabled" and .http_put_response_hop_limit == 2 and .http_tokens == "required"] | length > 0 and all) and
  ([.resource_changes[] | select(.address == "aws_iam_role_policy.topograph_aws[0]") | .change.after.policy | fromjson | .Statement[].Action] == ["ec2:DescribeInstanceTopology"])
'

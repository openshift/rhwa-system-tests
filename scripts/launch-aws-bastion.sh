#!/usr/bin/env bash
# Launch an external SSH bastion for the node-operation helpers in this suite.
set -euo pipefail
umask 077

usage() {
    cat <<'USAGE'
Usage: launch-aws-bastion.sh --ssh-key FILE [options]
       launch-aws-bastion.sh --cleanup STATE_DIR

Uses the current oc context and AWS CLI credentials; AWS_PROFILE is optional.
Discovers the AWS region, VPC and node security groups from the cluster.
Selects a subnet with an active IPv4 default route to an internet gateway,
or validates --subnet-id.

Options:
  --ssh-key FILE       Cluster SSH private key, or public key with ssh-agent.
  --subnet-id ID       Public subnet in the cluster VPC (optional).
  --instance-type TYPE  EC2 instance type (default: t3.micro).
  --cleanup STATE_DIR  Delete this run's instance, SSH rules, group and key pair.
  -h, --help           Show this help.

Creates a billable Amazon Linux 2023 instance and public IPv4 address, accepting
SSH from any IPv4 address (0.0.0.0/0) with key authentication. Adds TCP/22
rules to one security group per node, allowing access only from the new bastion
group. With the matching key loaded in ssh-agent, uses only the public key
(FILE.pub, or FILE if already public) and does not copy the private key.
Without a matching agent identity, an unencrypted private key is required.
Writes local state under /tmp/far-bastion.*, including env.sh for the tests.
If setup fails, resources remain recorded there for inspection and cleanup.
Requires AWS CLI v2, oc, jq, ssh, ssh-add, ssh-keygen, and Bash 3.2+.
USAGE
}

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
log() { printf '%s\n' "$*" >&2; }
aws_cli() { aws --region "$region" --no-cli-pager --no-cli-auto-prompt "$@"; }
save_state() {
    jq "$@" "$state_dir/resources.json" > "$state_dir/resources.next.json"
    mv "$state_dir/resources.next.json" "$state_dir/resources.json"
}
check_owner() {
    jq -e --arg token "$token" \
        'any(.Tags[]?; .Key == "rhwa-bastion-run" and .Value == $token)' \
        >/dev/null <<< "$1" || die "Resource does not belong to bastion run $token"
}

cleanup() {
    [[ -f "$state_dir/resources.json" ]] || die "No resources.json in $state_dir"
    jq -e '.format == "rhwa-aws-bastion-v1"' "$state_dir/resources.json" >/dev/null ||
        die "Not a bastion state directory"
    region=$(jq -er '.region' "$state_dir/resources.json")
    token=$(jq -er '.token' "$state_dir/resources.json")
    account=$(aws_cli sts get-caller-identity --query Account --output text)
    [[ $account == "$(jq -r '.account' "$state_dir/resources.json")" ]] ||
        die "AWS account differs from the account that launched this bastion"

    # Revoke only the individually recorded and tagged rules, preserving other rules.
    while IFS=$'\t' read -r group_id rule_id; do
        [[ -n $rule_id ]] || continue
        rule=$(aws_cli ec2 describe-security-group-rules \
            --filters "Name=group-id,Values=$group_id" --output json |
            jq -c --arg id "$rule_id" '.SecurityGroupRules[] | select(.SecurityGroupRuleId == $id)')
        if [[ -n $rule ]]; then
            check_owner "$rule"
            aws_cli ec2 revoke-security-group-ingress --group-id "$group_id" \
                --security-group-rule-ids "$rule_id" >/dev/null
        fi
        save_state --arg id "$rule_id" '.rules |= map(select(.id != $id))'
    done < <(jq -r '.rules[] | [.group, .id] | @tsv' "$state_dir/resources.json")

    instance_id=$(jq -r '.instance_id' "$state_dir/resources.json")
    if [[ -n $instance_id ]]; then
        instance=$(aws_cli ec2 describe-instances \
            --filters "Name=instance-id,Values=$instance_id" --output json |
            jq -c '.Reservations[].Instances[]')
        if [[ -n $instance ]]; then
            check_owner "$instance"
            if [[ $(jq -r '.State.Name' <<< "$instance") != terminated ]]; then
                log "Terminating $instance_id"
                aws_cli ec2 terminate-instances --instance-ids "$instance_id" >/dev/null
                aws_cli ec2 wait instance-terminated --instance-ids "$instance_id"
            fi
        fi
        save_state '.instance_id = ""'
    fi

    group_id=$(jq -r '.security_group_id' "$state_dir/resources.json")
    if [[ -n $group_id ]]; then
        group=$(aws_cli ec2 describe-security-groups \
            --filters "Name=group-id,Values=$group_id" --output json | jq -c '.SecurityGroups[]')
        if [[ -n $group ]]; then
            check_owner "$group"
            aws_cli ec2 delete-security-group --group-id "$group_id"
        fi
        save_state '.security_group_id = ""'
    fi
    key_name=$(jq -r '.key_name' "$state_dir/resources.json")
    if [[ -n $key_name ]]; then
        key_pair=$(aws_cli ec2 describe-key-pairs \
            --filters "Name=key-name,Values=$key_name" --output json | jq -c '.KeyPairs[]')
        if [[ -n $key_pair ]]; then
            check_owner "$key_pair"
            aws_cli ec2 delete-key-pair --key-name "$key_name"
        fi
        save_state '.key_name = ""'
    fi
    log "Cloud resources removed. Local state and the SSH identity file remain in $state_dir."
}

ssh_key=
subnet_id=
instance_type=t3.micro
cleanup_dir=
while [[ $# -gt 0 ]]; do
    case "$1" in
        --ssh-key|--subnet-id|--instance-type|--cleanup)
            [[ $# -ge 2 && -n $2 ]] || die "$1 requires a value"
            case "$1" in
                --ssh-key) ssh_key=$2 ;;
                --subnet-id) subnet_id=$2 ;;
                --instance-type) instance_type=$2 ;;
                --cleanup) cleanup_dir=$2 ;;
            esac
            shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) die "Unknown argument: $1 (see --help)" ;;
    esac
done
for dependency in aws jq; do
    command -v "$dependency" >/dev/null || die "$dependency is required"
done
if [[ -n $cleanup_dir ]]; then
    state_dir=$(cd "$cleanup_dir" && pwd -P)
    cleanup
    exit 0
fi
[[ -n $ssh_key && -r $ssh_key && -f $ssh_key ]] || die "Provide --ssh-key FILE"
for dependency in oc ssh ssh-add ssh-keygen; do
    command -v "$dependency" >/dev/null || die "$dependency is required"
done

state_dir=$(mktemp -d /tmp/far-bastion.XXXXXX)
token=${state_dir##*/}
script_path=$(cd "$(dirname "$0")" && pwd -P)/$(basename "$0")
trap 'result=$?; if [[ $result -ne 0 ]]; then
    printf "Setup failed. State: %s\n" "$state_dir" >&2
    if [[ -f "$state_dir/resources.json" ]]; then
        printf "Cleanup: bash %q --cleanup %q\n" "$script_path" "$state_dir" >&2
    fi
fi' EXIT

# Prefer the public key so passphrase-protected private keys stay with the agent.
mkdir "$state_dir/profile" "$state_dir/shared"
public_source=$ssh_key
if [[ -r $ssh_key.pub ]]; then
    public_source=$ssh_key.pub
fi
read -r key_type _ < "$public_source"
case "$key_type" in
    ssh-rsa|ssh-ed25519) install -m 600 "$public_source" "$state_dir/public-key.pub" ;;
    *)
        ssh-keygen -y -P '' -f "$ssh_key" > "$state_dir/public-key.pub" 2> "$state_dir/key.stderr" ||
            die "For an encrypted key, provide its .pub file and load the key with ssh-add first" ;;
esac
key_fingerprint=$(ssh-keygen -lf "$state_dir/public-key.pub" | awk '{print $2}')
use_agent=false
if [[ -n ${SSH_AUTH_SOCK:-} ]] && ssh-add -T "$state_dir/public-key.pub" >/dev/null 2>&1; then
    use_agent=true
    # The test helper passes this file to ssh -i, which also accepts public keys
    # backed by ssh-agent. Its legacy filename does not determine its contents.
    install -m 600 "$state_dir/public-key.pub" "$state_dir/profile/ssh-privatekey"
    log "Using SSH agent identity $key_fingerprint; storing only the public key"
else
    ssh-keygen -y -P '' -f "$ssh_key" > "$state_dir/derived-public-key.pub" 2> "$state_dir/key.stderr" ||
        die "Load the matching key with ssh-add and keep SSH_AUTH_SOCK available to the script and tests"
    [[ $(ssh-keygen -lf "$state_dir/derived-public-key.pub" | awk '{print $2}') == "$key_fingerprint" ]] ||
        die "The public key does not match --ssh-key"
    install -m 600 "$ssh_key" "$state_dir/profile/ssh-privatekey"
    log "Using an unencrypted private key; a local 0600 copy is stored in $state_dir/profile"
fi

oc get infrastructure cluster -o json > "$state_dir/infrastructure.json"
region=$(jq -er '.status.platformStatus | select(.type == "AWS") | .aws.region | select(length > 0)' \
    "$state_dir/infrastructure.json") || die "Current cluster must have AWS platform status"
account=$(aws_cli sts get-caller-identity --query Account --output text)
oc get nodes -o json > "$state_dir/nodes.json"
jq -e '.items | length > 0 and all(.[];
    ((.spec.providerID // "") | test("^aws:///[^/]+/i-[0-9a-f]+$")) and
    ([.status.addresses[] | select(.type == "InternalIP")][0].address |
        test("^([0-9]{1,3}\\.){3}[0-9]{1,3}$")))' "$state_dir/nodes.json" >/dev/null ||
    die "Every cluster node must have an AWS providerID and IPv4 InternalIP"
read -r -a node_ids <<< "$(jq -r '[.items[].spec.providerID | split("/")[-1]] | join(" ")' \
    "$state_dir/nodes.json")"
aws_cli ec2 describe-instances --instance-ids "${node_ids[@]}" --output json > "$state_dir/instances.json"
jq -e --argjson count "${#node_ids[@]}" '
    [.Reservations[].Instances[]] | length == $count and
    all(.[]; .State.Name == "running") and ([.[].VpcId] | unique | length == 1)' \
    "$state_dir/instances.json" >/dev/null || die "Cluster instances must be running in one VPC"
vpc_id=$(jq -r '.Reservations[0].Instances[0].VpcId' "$state_dir/instances.json")
aws_cli ec2 describe-subnets --filters "Name=vpc-id,Values=$vpc_id" --output json > "$state_dir/subnets.json"
aws_cli ec2 describe-route-tables --filters "Name=vpc-id,Values=$vpc_id" --output json > "$state_dir/routes.json"
jq --slurpfile routes "$state_dir/routes.json" '[
    .Subnets[] | select(.State == "available" and .AvailableIpAddressCount > 0) | . as $subnet |
    ($routes[0].RouteTables |
        map(select(any(.Associations[]?; .SubnetId == $subnet.SubnetId)))) as $explicit |
    (if ($explicit | length) > 0 then $explicit else
        $routes[0].RouteTables | map(select(any(.Associations[]?; .Main == true))) end) |
    select(any(.[].Routes[]?; .DestinationCidrBlock == "0.0.0.0/0" and .State == "active" and
        ((.GatewayId // "") | startswith("igw-")))) | $subnet.SubnetId
] | sort' "$state_dir/subnets.json" > "$state_dir/public-subnets.json"
if [[ -n $subnet_id ]]; then
    jq -e --arg id "$subnet_id" 'index($id) != null' "$state_dir/public-subnets.json" >/dev/null ||
        die "Subnet $subnet_id is not an available public subnet in $vpc_id"
else
    subnet_id=$(jq -r '.[0] // empty' "$state_dir/public-subnets.json")
    [[ -n $subnet_id ]] || die "No public subnet with an active internet-gateway default route in $vpc_id"
fi
architecture=$(aws_cli ec2 describe-instance-types --instance-types "$instance_type" \
    --query 'InstanceTypes[0].ProcessorInfo.SupportedArchitectures[0]' --output text)
[[ $architecture == x86_64 || $architecture == arm64 ]] || die "Unsupported architecture: $architecture"
ami_id=$(aws_cli ssm get-parameter \
    --name "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-$architecture" \
    --query Parameter.Value --output text)
root_device=$(aws_cli ec2 describe-images --image-ids "$ami_id" \
    --query 'Images[0].RootDeviceName' --output text)
jq -n --arg region "$region" --arg account "$account" --arg token "$token" \
    '{format:"rhwa-aws-bastion-v1", region:$region, account:$account, token:$token,
      instance_id:"", security_group_id:"", key_name:"", rules:[]}' > "$state_dir/resources.json"
log "Cluster: $(oc whoami --show-server)"
log "AWS account: $account; region: $region; VPC: $vpc_id; subnet: $subnet_id"
log "Launching $instance_type with $ami_id; SSH source: 0.0.0.0/0; state: $state_dir"

# All created resources and rules carry this run's tag for cleanup ownership checks.
key_name=$token
aws_cli ec2 import-key-pair --key-name "$key_name" \
    --public-key-material "fileb://$state_dir/public-key.pub" \
    --tag-specifications "ResourceType=key-pair,Tags=[{Key=rhwa-bastion-run,Value=$token}]" >/dev/null
save_state --arg name "$key_name" '.key_name = $name'
group_id=$(aws_cli ec2 create-security-group --group-name "$token" \
    --description "SSH bastion for RHWA tests" --vpc-id "$vpc_id" \
    --tag-specifications "ResourceType=security-group,Tags=[{Key=rhwa-bastion-run,Value=$token}]" \
    --query GroupId --output text)
save_state --arg id "$group_id" '.security_group_id = $id'
aws_cli ec2 authorize-security-group-ingress --group-id "$group_id" \
    --protocol tcp --port 22 --cidr 0.0.0.0/0 >/dev/null
while IFS= read -r node_group; do
    rule_id=$(aws_cli ec2 authorize-security-group-ingress --group-id "$node_group" \
        --protocol tcp --port 22 --source-group "$group_id" \
        --tag-specifications "ResourceType=security-group-rule,Tags=[{Key=rhwa-bastion-run,Value=$token}]" \
        --query 'SecurityGroupRules[0].SecurityGroupRuleId' --output text)
    save_state --arg group "$node_group" --arg id "$rule_id" '.rules += [{group:$group, id:$id}]'
done < <(jq -r '[.Reservations[].Instances[].SecurityGroups[0].GroupId] | unique[]' "$state_dir/instances.json")

# Explicitly allow the TCP forwarding used by ssh -W. No private key is sent here.
cat > "$state_dir/user-data.sh" <<'USERDATA'
#!/bin/bash
set -eu
printf 'AllowTcpForwarding local\nPasswordAuthentication no\n' > /etc/ssh/sshd_config.d/00-rhwa-bastion.conf
sshd -t
systemctl reload sshd
USERDATA
block_devices=$(jq -nc --arg device "$root_device" \
    '[{DeviceName:$device,Ebs:{Encrypted:true,DeleteOnTermination:true,VolumeType:"gp3"}}]')
instance_id=$(aws_cli ec2 run-instances --image-id "$ami_id" --instance-type "$instance_type" \
    --count 1 --subnet-id "$subnet_id" --associate-public-ip-address \
    --security-group-ids "$group_id" --key-name "$key_name" --client-token "$token" \
    --metadata-options HttpTokens=required,HttpEndpoint=enabled \
    --block-device-mappings "$block_devices" --user-data "file://$state_dir/user-data.sh" \
    --tag-specifications \
        "ResourceType=instance,Tags=[{Key=Name,Value=$token},{Key=rhwa-bastion-run,Value=$token}]" \
        "ResourceType=volume,Tags=[{Key=rhwa-bastion-run,Value=$token}]" \
    --query 'Instances[0].InstanceId' --output text)
save_state --arg id "$instance_id" '.instance_id = $id'
log "Waiting for $instance_id to run"
aws_cli ec2 wait instance-running --instance-ids "$instance_id"
public_ip=$(aws_cli ec2 describe-instances --instance-ids "$instance_id" \
    --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
[[ $public_ip =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "Bastion has no public IPv4 address"

ssh_options=(-i "$state_dir/profile/ssh-privatekey" -o BatchMode=yes -o IdentitiesOnly=yes
    -o ConnectTimeout=5 -o ConnectionAttempts=1 -o StrictHostKeyChecking=accept-new
    -o "UserKnownHostsFile=$state_dir/known_hosts")
log "Waiting for SSH at ec2-user@$public_ip"
ssh_ready=false
for ((attempt=1; attempt<=36; attempt++)); do
    if ssh "${ssh_options[@]}" "ec2-user@$public_ip" true > "$state_dir/ssh.stdout" 2> "$state_dir/ssh.stderr"; then
        ssh_ready=true
        break
    fi
    sleep 5
done
if [[ $ssh_ready != true ]]; then
    cat "$state_dir/ssh.stderr" >&2
    die "Cannot SSH to bastion; check network reachability, subnet ACLs and the cluster key"
fi
proxy_command="ssh -i $state_dir/profile/ssh-privatekey -o BatchMode=yes -o IdentitiesOnly=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=$state_dir/known_hosts -W %h:%p ec2-user@$public_ip"
while IFS=$'\t' read -r node_name node_ip; do
    log "Checking core@$node_ip ($node_name) through bastion"
    load_state=$(ssh "${ssh_options[@]}" -o "ProxyCommand=$proxy_command" "core@$node_ip" \
        'sudo -n systemctl show kubelet --property=LoadState --value') ||
        die "Cannot reach $node_name with passwordless sudo; check key, node SSH and subnet ACLs"
    [[ $load_state == loaded ]] || die "kubelet unit is not loaded on $node_name"
done < <(jq -r '.items[] | [.metadata.name,
    ([.status.addresses[] | select(.type == "InternalIP")][0].address)] | @tsv' "$state_dir/nodes.json")

printf '%s\n' "$public_ip" > "$state_dir/shared/bastion_public_address"
printf '%s\n' ec2-user > "$state_dir/shared/bastion_ssh_user"
{
    printf 'export CLUSTER_PROFILE_DIR=%q\n' "$state_dir/profile"
    printf 'export SHARED_DIR=%q\n' "$state_dir/shared"
    if [[ $use_agent == true ]]; then
        printf 'export SSH_AUTH_SOCK=%q\n' "$SSH_AUTH_SOCK"
    fi
} > "$state_dir/env.sh"
log "Bastion $instance_id is ready; SSH and passwordless sudo verified on every cluster node."
printf 'source %q\n' "$state_dir/env.sh"
printf '# Cleanup when finished:\nbash %q --cleanup %q\n' "$script_path" "$state_dir"

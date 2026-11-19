#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
    echo "Usage: $0 /path/to/cluster-private-key"
    cat <<'EOF'
Load settings with: bastion_env=$(make -s setup-local-bastion SSH_KEY=/path/to/key) && eval "$bastion_env"
EOF
}

fail() {
    echo "ERROR: $*" >&2
    exit 1
}

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
    usage
    exit 0
fi
[[ $# == 1 && -n $1 ]] || { usage >&2; exit 2; }
[[ -f $1 && -r $1 ]] || fail "The SSH private key must be a readable file."
for tool in oc ssh ssh-keygen jq; do
    command -v "$tool" >/dev/null || fail "$tool is required."
done

script_dir=$(cd "$(dirname "$0")" && pwd)
state_dir="/tmp/medik8s-local-bastion-$(id -u)"
namespace=test-ssh-bastion
manager=system-tests-local-bastion
oc_bin=$(command -v oc)
ssh_bin=$(command -v ssh)
context=$("$oc_bin" config current-context) || fail "Log in with oc login first."
oc_call() {
    "$oc_bin" --context="$context" --request-timeout=15s "$@"
}
oc_call whoami >/dev/null || fail "Log in to an existing OpenShift cluster with oc login first."
cluster_id=$(oc_call get namespace kube-system -o jsonpath='{.metadata.uid}')
[[ -n $cluster_id ]] || fail "Cannot identify the current cluster."
nodes=$(oc_call get nodes -o json)
jq -e 'any(.items[];
    (.metadata.labels | has("node-role.kubernetes.io/master")) and
    .metadata.labels["kubernetes.io/os"] == "linux" and
    any(.status.conditions[]; .type == "Ready" and .status == "True"))' \
    <<< "$nodes" >/dev/null || fail "A Ready Linux control-plane node is required."
for resource in "namespace/$namespace" "scc/$manager"; do
    existing_manager=$(oc_call get "$resource" --ignore-not-found \
        -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}')
    if [[ -z $existing_manager ]]; then
        existing_resource=$(oc_call get "$resource" --ignore-not-found -o name)
        [[ -z $existing_resource ]] || fail "$resource already exists and is not managed by this script."
    else
        [[ $existing_manager == "$manager" ]] || fail "$resource belongs to another setup."
    fi
done

[[ ! -L $state_dir ]] || fail "$state_dir must not be a symlink."
mkdir -p "$state_dir"
[[ -O $state_dir ]] || fail "$state_dir must belong to the current user."
chmod 700 "$state_dir"
if [[ -f $state_dir/cluster-id ]]; then
    [[ $(cat "$state_dir/cluster-id") == "$cluster_id" ]] || \
        fail "The local state in $state_dir belongs to another cluster."
fi
mkdir "$state_dir/setup.lock" 2>/dev/null || fail "Another setup is running, or $state_dir/setup.lock is stale."
started_pid=
cleanup() {
    status=$?
    if [[ $status != 0 && -n $started_pid ]]; then
        kill "$started_pid" 2>/dev/null || true
        wait "$started_pid" 2>/dev/null || true
        rm -f "$state_dir/port-forward.pid"
    fi
    rm -f "$state_dir/cluster-profile/ssh-privatekey.new" "$state_dir/authorized_keys.new" "$state_dir/kubeconfig.new"
    rmdir "$state_dir/setup.lock"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "$state_dir/cluster-profile" "$state_dir/shared" "$state_dir/bin"
key="$state_dir/cluster-profile/ssh-privatekey"
cp "$1" "$key.new"
chmod 600 "$key.new"
ssh-keygen -y -P '' -f "$key.new" > "$state_dir/authorized_keys.new" || \
    fail "Use a valid, unencrypted cluster SSH private key."
mv "$key.new" "$key"
mv "$state_dir/authorized_keys.new" "$state_dir/authorized_keys"
printf '%s\n' "$cluster_id" > "$state_dir/cluster-id"
oc_call config view --raw --flatten --minify > "$state_dir/kubeconfig.new"
mv "$state_dir/kubeconfig.new" "$state_dir/kubeconfig"
export KUBECONFIG="$state_dir/kubeconfig"

if [[ $ssh_bin == "$state_dir/bin/ssh" ]]; then
    ssh_bin=$(cat "$state_dir/ssh-binary")
fi
[[ -x $ssh_bin && $ssh_bin != "$state_dir/bin/ssh" ]] || fail "Cannot find the original SSH executable."
printf '%s\n' "$ssh_bin" > "$state_dir/ssh-binary"
if [[ ! -f $state_dir/ssh_host_ed25519_key ]]; then
    ssh-keygen -q -t ed25519 -N '' -C '' -f "$state_dir/ssh_host_ed25519_key"
fi
ssh-keygen -y -P '' -f "$state_dir/ssh_host_ed25519_key" > "$state_dir/ssh_host_ed25519_key.pub"
printf '[127.0.0.1]:2222 %s\n' "$(cat "$state_dir/ssh_host_ed25519_key.pub")" > "$state_dir/known_hosts"
cat > "$state_dir/sshd_config" <<'EOF'
Port 2222
HostKey /etc/bastion/ssh_host_ed25519_key
AuthorizedKeysFile /home/core/.ssh/authorized_keys
AllowUsers core
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
UsePAM yes
AllowTcpForwarding local
AllowAgentForwarding no
X11Forwarding no
PermitTTY no
PidFile /tmp/sshd.pid
EOF

echo "Configuring the local bastion for context $context." >&2
oc_call apply -f "$script_dir/manifests/local-bastion.yaml" >&2
oc_call -n "$namespace" create secret generic ssh-bastion-config \
    --from-file="authorized_keys=$state_dir/authorized_keys" \
    --from-file="ssh_host_ed25519_key=$state_dir/ssh_host_ed25519_key" \
    --from-file="sshd_config=$state_dir/sshd_config" \
    --dry-run=client -o yaml | oc_call apply -f - >&2
revision=$(cat "$state_dir/authorized_keys" "$state_dir/ssh_host_ed25519_key.pub" \
    "$state_dir/sshd_config" | cksum | awk '{print $1}')
oc_call -n "$namespace" patch deployment ssh-bastion --type=merge -p \
    "$(jq -nc --arg revision "$revision" \
        '{spec:{template:{metadata:{annotations:{"system-tests.medik8s.io/config-revision":$revision}}}}}')" >&2
"$oc_bin" --context="$context" --request-timeout=190s -n "$namespace" \
    rollout status deployment/ssh-bastion --timeout=180s >&2

ssh_options=(-F /dev/null -i "$key" -o BatchMode=yes -o IdentitiesOnly=yes
    -o IdentityAgent=none -o ConnectTimeout=5 -o ConnectionAttempts=1
    -o ServerAliveInterval=5 -o ServerAliveCountMax=1
    -o StrictHostKeyChecking=yes -o "UserKnownHostsFile=$state_dir/known_hosts")
: > "$state_dir/ssh-check.log"
bastion_ready() {
    "$ssh_bin" "${ssh_options[@]}" -p 2222 core@127.0.0.1 true \
        > /dev/null 2> "$state_dir/ssh-check.log"
}
forward_is_ours() {
    [[ $1 =~ ^[0-9]+$ ]] || return 1
    process_command=$(ps -ww -p "$1" -o command= 2>/dev/null) || return 1
    [[ $process_command == "$oc_bin --context=$context --namespace=$namespace port-forward "* ||
        $process_command == "/bin/bash $script_dir/local-bastion-forward.sh $oc_bin --context=$context --namespace=$namespace port-forward "* ]]
}
forward_pid=
if [[ -f $state_dir/port-forward.pid ]]; then
    forward_pid=$(cat "$state_dir/port-forward.pid")
fi
if forward_is_ours "$forward_pid" &&
    [[ $process_command == "/bin/bash $script_dir/local-bastion-forward.sh "* ]] && bastion_ready; then
    echo "Reusing port-forward process $forward_pid." >&2
else
    if forward_is_ours "$forward_pid"; then
        kill "$forward_pid"
        for ((attempt=0; attempt<5; attempt++)); do
            forward_is_ours "$forward_pid" || break
            sleep 1
        done
        if forward_is_ours "$forward_pid"; then
            fail "Port-forward process $forward_pid did not stop."
        fi
    fi
    nohup /bin/bash "$script_dir/local-bastion-forward.sh" \
        "$oc_bin" --context="$context" --namespace="$namespace" port-forward \
        --address=127.0.0.1 deployment/ssh-bastion 2222:2222 \
        > "$state_dir/port-forward.log" 2>&1 < /dev/null &
    started_pid=$!
    printf '%s\n' "$started_pid" > "$state_dir/port-forward.pid"
    ready=false
    for ((attempt=0; attempt<12; attempt++)); do
        kill -0 "$started_pid" 2>/dev/null || break
        if grep -q 'Forwarding from 127.0.0.1:2222' "$state_dir/port-forward.log" && bastion_ready && \
            kill -0 "$started_pid" 2>/dev/null; then
            ready=true
            break
        fi
        sleep 1
    done
    if [[ $ready != true ]]; then
        cat "$state_dir/port-forward.log" "$state_dir/ssh-check.log" >&2
        fail "The SSH port forward did not become ready on 127.0.0.1:2222."
    fi
fi

proxy_command=$(printf '%q ' "$ssh_bin" "${ssh_options[@]}" -p 2222 -W '%h:%p' core@127.0.0.1)
node_addresses=$(jq -r '.items[] |
    select(.metadata.labels["kubernetes.io/os"] == "linux") |
    select(any(.status.conditions[]; .type == "Ready" and .status == "True")) |
    [.metadata.name, ([.status.addresses[] | select(.type == "InternalIP") | .address][0] // "")] | @tsv' \
    <<< "$nodes")
while IFS=$'\t' read -r node_name node_ip; do
    [[ -n $node_ip ]] || fail "Node $node_name has no InternalIP."
    echo "Checking SSH and sudo access to $node_name." >&2
    "$ssh_bin" -F /dev/null -i "$key" -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityAgent=none \
        -o ConnectTimeout=10 -o ConnectionAttempts=1 -o ServerAliveInterval=5 -o ServerAliveCountMax=1 \
        -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
        -o "ProxyCommand=$proxy_command" "core@$node_ip" 'sudo -n true' >&2 || \
        fail "The supplied key cannot reach $node_name through the bastion."
done <<< "$node_addresses"

# The existing test helper has no bastion-port setting. An SSH alias supplies it.
cat > "$state_dir/ssh-config" <<EOF
Host medik8s-local-bastion
    HostName 127.0.0.1
    Port 2222
    User core
    BatchMode yes
    IdentitiesOnly yes
    IdentityAgent none
    ProxyCommand none
    ProxyJump none
    IdentityFile $key
    UserKnownHostsFile $state_dir/known_hosts
    StrictHostKeyChecking yes
Host *
    Include ~/.ssh/config
    Include /etc/ssh/ssh_config
EOF
{
    echo '#!/usr/bin/env bash'
    printf 'exec %q -F %q "$@"\n' "$ssh_bin" "$state_dir/ssh-config"
} > "$state_dir/bin/ssh"
chmod 700 "$state_dir/bin/ssh"
printf '%s\n' medik8s-local-bastion > "$state_dir/shared/bastion_public_address"
printf '%s\n' core > "$state_dir/shared/bastion_ssh_user"
{
    printf 'export KUBECONFIG=%q\n' "$state_dir/kubeconfig"
    printf 'export CLUSTER_PROFILE_DIR=%q\n' "$state_dir/cluster-profile"
    printf 'export SHARED_DIR=%q\n' "$state_dir/shared"
    # The generated file expands PATH when the caller sources it.
    # shellcheck disable=SC2016
    printf 'case ":$PATH:" in\n    *":%s:"*) ;;\n    *) export PATH="%s:$PATH" ;;\nesac\n' \
        "$state_dir/bin" "$state_dir/bin"
} > "$state_dir/env.sh"
echo "Bastion ready. Port-forward log: $state_dir/port-forward.log" >&2
printf '. %q\n' "$state_dir/env.sh"

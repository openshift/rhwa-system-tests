# AWS bastion for remediation tests

[`scripts/launch-aws-bastion.sh`](../scripts/launch-aws-bastion.sh) launches an
external EC2 SSH bastion for the suite's kubelet stop/start helpers. The bastion
stays available while cluster nodes reboot or stop reporting Ready. It does not
install FAR or NHC, create fencing credentials, or run destructive tests.

Use the same kubeconfig and AWS CLI credentials as the cluster. The script
requires AWS CLI v2, `oc`, `jq`, OpenSSH, and Bash 3.2 or newer. The cluster must
report the AWS platform, and its nodes must have AWS provider IDs and IPv4
InternalIP addresses. Use the **SSH key used during cluster installation**,
not the AWS API access key. The same SSH identity authenticates to both the
bastion and the nodes.

The AWS principal must be able to read the cluster's EC2 networking, retrieve
the Amazon Linux AMI parameter from SSM, and create/tag/delete EC2 instances,
key pairs, security groups and security-group rules. Credentials limited to
FAR's EC2 fencing operations are insufficient to provision the bastion.

```bash
export KUBECONFIG=/path/to/cluster/kubeconfig
ssh-add /path/to/cluster-private-key
bash scripts/launch-aws-bastion.sh \
  --ssh-key /path/to/cluster-private-key
```

Passphrase-protected keys work through your running SSH agent. `ssh-add` prompts
for the passphrase when loading the key; the script never asks for it. The
script reads the companion `/path/to/cluster-private-key.pub` file, verifies
that the agent can sign with that identity, and stores **only the public key**
for the SSH connections and test helper. You can also pass the public key
directly with `--ssh-key /path/to/cluster-private-key.pub`. The private key is
not copied in agent mode. Without a matching loaded agent identity, the script
continues to support an unencrypted private key using a local `0600` copy.

`AWS_PROFILE` is **optional**. It selects a named collection of AWS CLI settings
and credentials, commonly stored in `~/.aws/config` and `~/.aws/credentials`.
It is useful when switching between AWS accounts or roles. Leave it unset if
your existing credentials already work: the script uses the normal AWS CLI
credential lookup, including exported credentials, the default profile, or an
available workload/instance role. It does not read AWS credentials from the
cluster's kubeconfig. You can check the active account and identity without
changing anything:

```bash
aws sts get-caller-identity
```

See [AWS configuration and credential documentation](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-files.html).

If the tests run inside a container, it must also receive the generated state
directory and, in agent mode, the agent socket at the path in `SSH_AUTH_SOCK`.
Keep that agent running and the matching key loaded throughout the tests.
Both SSH connections originate on the runner, so agent forwarding to the
bastion is not needed.

The script discovers the region from `Infrastructure/cluster` and the VPC and
security groups from the cluster's EC2 instances. It chooses a subnet with an
active `0.0.0.0/0` route to an internet gateway, honoring both explicit and main
route-table associations. To choose a particular public subnet in that VPC,
add `--subnet-id subnet-...`. If none exists, provision a public subnet first;
the script does not change VPC routes, internet gateways or network ACLs.

It launches the current Amazon Linux 2023 AMI from AWS's public SSM parameter
with `t3.micro` by default (`--instance-type` overrides this), encrypted root
storage, a public IPv4 address, and required IMDSv2. EC2, storage and public IPv4
charges apply. It imports only the public SSH key, creates a bastion security
group allowing inbound TCP/22 from any IPv4 address (`0.0.0.0/0`), and adds TCP/22
access from that group to one attached security group per cluster node. The new
bastion group retains AWS's default outbound allowance. Existing node rules
remain in place. Restrictive network ACLs can still block either SSH hop.

Before reporting success it verifies SSH to the bastion and then SSH plus
passwordless `sudo` to every cluster node, using a read-only kubelet unit query.
No kubelet is stopped. Source the printed file **in the shell that launches the
tests**:

```bash
source /tmp/far-bastion.XXXXXX/env.sh  # use the actual path printed by the script
```

This sets `CLUSTER_PROFILE_DIR` and `SHARED_DIR` to generated directories holding
`ssh-privatekey`, `bastion_public_address`, and `bastion_ssh_user`. In agent mode,
the legacy `ssh-privatekey` filename contains the **public** key; OpenSSH uses
it to select the matching private identity in the agent. The existing test
helper copies that public file before passing it to `ssh -i`. The environment
file also exports `SSH_AUTH_SOCK` in agent mode. The SSH helper will report
`findSSHBastion: using external bastion ... (user: ec2-user)`. These
exports replace any existing values in that shell; use a separate shell if a CI
workflow already uses those directories for other artifacts.

The output also gives the cleanup command:

```bash
bash scripts/launch-aws-bastion.sh --cleanup /tmp/far-bastion.XXXXXX
```

Cleanup checks the AWS account and each resource's ownership tag, revokes the
recorded node SSH rules, terminates the instance (deleting its root volume), and
deletes the bastion security group and imported EC2 key pair. It preserves the
cluster's other resources and rules. Local state, including the SSH identity
file (public in agent mode, private otherwise), remains in the printed
directory for you to remove after use. Keep that
directory until cloud cleanup has finished. Failed setup retains its recorded
resources and prints the cleanup command; cleanup can be retried.

AWS references: [launching with a public IP](https://docs.aws.amazon.com/cli/latest/reference/ec2/run-instances.html),
[security-group rules](https://docs.aws.amazon.com/cli/latest/reference/ec2/authorize-security-group-ingress.html),
[Amazon Linux AMI parameters](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/finding-an-ami-parameter-store.html),
and [OpenSSH public identities backed by ssh-agent](https://man.openbsd.org/ssh_config#IdentityFile).

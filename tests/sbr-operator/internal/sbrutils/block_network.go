package sbrutils

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// BlockFaultKind selects the externally observable transport fault.
type BlockFaultKind string

// Supported faults preserve the prepared SSH management path.
const (
	BlockAPILoss            BlockFaultKind = "api-loss"
	BlockStorageLoss        BlockFaultKind = "storage-loss"
	BlockStorageTimeout     BlockFaultKind = "storage-timeout"
	BlockSingletonIsolation BlockFaultKind = "singleton-isolation"
	BlockGroupPartition     BlockFaultKind = "group-partition"
	BlockStorageLatency     BlockFaultKind = "storage-latency"
)

// BlockNetwork contains discovered destinations and current SBR network namespaces.
type BlockNetwork struct {
	API, Storage      []BlockEndpoint
	Peers, Management []string
	NamespacePIDs     []int
}

// BlockFaultEvidence records fault dispatch, confirmation, removal, and persistent ordering.
type BlockFaultEvidence struct {
	DispatchedAt, ActiveAt          time.Time
	RestoreDispatchedAt, RestoredAt time.Time
	Chain, Unit                     string
	PersistentOrderingConfirmed     bool
}

// BlockNetworkFault owns native network rules and optional boot persistence.
type BlockNetworkFault struct {
	host         BlockHost
	network      BlockNetwork
	kind         BlockFaultKind
	persistent   bool
	prepared     bool
	evidence     BlockFaultEvidence
	device       string
	families     []string
	reachableAPI map[int][]BlockEndpoint
}

// NewBlockNetworkFault validates discovered destinations without changing the host.
func NewBlockNetworkFault(host BlockHost, network BlockNetwork, kind BlockFaultKind,
	owner string, persistent bool,
) (*BlockNetworkFault, error) {
	if host == nil || !validBlockOwner(owner) {
		return nil, fmt.Errorf("network fault requires a host and a valid owner")
	}

	switch kind {
	case BlockAPILoss, BlockStorageLoss, BlockStorageTimeout,
		BlockSingletonIsolation, BlockGroupPartition, BlockStorageLatency:
	default:
		return nil, fmt.Errorf("unsupported network fault %q", kind)
	}

	if len(network.API) == 0 || len(network.Storage) == 0 {
		return nil, &BlockPrerequisiteError{Reason: "discover API and advertised storage endpoints before preparing a fault"}
	}

	if (kind == BlockSingletonIsolation || kind == BlockGroupPartition) &&
		(len(network.Peers) == 0 || len(network.Management) == 0) {
		return nil, &BlockPrerequisiteError{Reason: "cluster partition requires cluster and management addresses"}
	}

	if persistent && kind == BlockStorageLatency {
		return nil, fmt.Errorf("the latency case does not support a persistent fault")
	}

	for _, endpoint := range append(append([]BlockEndpoint{}, network.API...), network.Storage...) {
		address, err := netip.ParseAddr(endpoint.Address)
		if err != nil || address.IsUnspecified() || address.Zone() != "" || endpoint.Port < 1 || endpoint.Port > 65535 {
			return nil, fmt.Errorf("invalid network endpoint %v", endpoint)
		}
	}

	for _, address := range append(append([]string{}, network.Peers...), network.Management...) {
		if _, err := netip.ParseAddr(address); err != nil {
			if _, prefixErr := netip.ParsePrefix(address); prefixErr != nil {
				return nil, fmt.Errorf("invalid partition address %q", address)
			}
		}
	}

	sum := sha256.Sum256([]byte(owner + ":" + string(kind)))
	suffix := hex.EncodeToString(sum[:6])
	fault := &BlockNetworkFault{
		host: host, network: network, kind: kind, persistent: persistent,
		evidence: BlockFaultEvidence{Chain: "SBRT_" + suffix, Unit: "sbr-test-" + suffix + ".service"},
	}
	fault.SetNamespacePIDs(network.NamespacePIDs)
	for _, tool := range []string{"iptables", "ip6tables"} {
		if fault.familyNeeded(tool) {
			fault.families = append(fault.families, tool)
		}
	}

	return fault, nil
}

// SetNamespacePIDs updates live namespaces after Pod replacement or reboot.
func (fault *BlockNetworkFault) SetNamespacePIDs(pids []int) {
	fault.network.NamespacePIDs = []int{1}
	for _, pid := range pids {
		if pid > 1 {
			fault.network.NamespacePIDs = append(fault.network.NamespacePIDs, pid)
		}
	}
}

// Evidence returns timestamps and identifiers for public test reports.
func (fault *BlockNetworkFault) Evidence() BlockFaultEvidence {
	return fault.evidence
}

// Kind identifies the transport behavior that this fault changes.
func (fault *BlockNetworkFault) Kind() BlockFaultKind {
	return fault.kind
}

// Prepare checks native commands, rule ownership, and baseline API access.
func (fault *BlockNetworkFault) Prepare(ctx context.Context) error {
	script := "set -eu\ncommand -v bash >/dev/null\ncommand -v timeout >/dev/null\ncommand -v nsenter >/dev/null\n"
	if fault.persistent {
		script += "command -v systemctl >/dev/null\ntest ! -e /etc/systemd/system/" + fault.evidence.Unit + "\n" +
			"test ! -e /var/lib/" + fault.evidence.Unit + ".sh\n"
	}

	if fault.kind == BlockStorageLatency {
		script += "command -v ip >/dev/null\ncommand -v tc >/dev/null\n"
	} else {
		for _, tool := range fault.families {
			script += "command -v " + tool + " >/dev/null\nrules=$(" + tool + " -w 5 -S)\n" +
				"if printf '%s\\n' \"$rules\" | awk '$1 == \"-N\" && $2 == \"" + fault.evidence.Chain +
				"\" { found=1 } END { exit !found }'; then exit 1; fi\n"
		}
	}

	if _, err := fault.host.Run(ctx, blockHostCall, "sudo sh -c "+blockShellQuote(script)); err != nil {
		return &BlockPrerequisiteError{Reason: fmt.Sprintf("native network fault prerequisites are unavailable: %v", err)}
	}

	if fault.kind == BlockStorageLatency {
		if err := fault.prepareLatency(ctx); err != nil {
			return err
		}
	}

	for _, pid := range fault.network.NamespacePIDs {
		for _, endpoint := range fault.network.API {
			if err := fault.probeTransport(ctx, pid, endpoint, true); err != nil {
				continue
			}

			if fault.reachableAPI == nil {
				fault.reachableAPI = map[int][]BlockEndpoint{}
			}

			fault.reachableAPI[pid] = append(fault.reachableAPI[pid], endpoint)
		}

		if len(fault.reachableAPI[pid]) == 0 {
			return &BlockPrerequisiteError{Reason: fmt.Sprintf("network namespace %d cannot reach any API endpoint", pid)}
		}
	}

	fault.prepared = true

	return nil
}

// Activate installs the owned fault and records dispatch before the host call.
func (fault *BlockNetworkFault) Activate(ctx context.Context) error {
	if !fault.prepared {
		return fmt.Errorf("prepare the network fault before activation")
	}

	fault.evidence.DispatchedAt = time.Now()
	fault.evidence.ActiveAt = time.Time{}
	fault.evidence.RestoreDispatchedAt = time.Time{}
	fault.evidence.RestoredAt = time.Time{}
	script := fault.applyScript()
	if fault.kind == BlockStorageLatency {
		script = fault.latencyApplyScript()
	}

	if fault.persistent {
		unit := "[Unit]\nDescription=SBR test network rules\nDefaultDependencies=no\nAfter=local-fs.target\n" +
			"Before=crio.service kubelet.service\n[Service]\nType=oneshot\nRemainAfterExit=yes\n" +
			"ExecStart=/bin/sh /var/lib/" + fault.evidence.Unit + ".sh\n[Install]\nWantedBy=multi-user.target\n"
		script = "set -eu\nprintf '%s' " + blockShellQuote("#!/bin/sh\n"+script) +
			" > /var/lib/" + fault.evidence.Unit + ".sh\n" +
			"printf '%s' " + blockShellQuote(unit) + " > /etc/systemd/system/" + fault.evidence.Unit + "\n" +
			"systemctl daemon-reload\nsystemctl enable --now " + fault.evidence.Unit + "\n" +
			"systemctl is-active --quiet " + fault.evidence.Unit + "\n" +
			"ordering=$(systemctl show " + fault.evidence.Unit + " --property Before --value)\n" +
			"case \" $ordering \" in *' kubelet.service '*) ;; *) exit 1 ;; esac\n" +
			"case \" $ordering \" in *' crio.service '*) ;; *) exit 1 ;; esac\n"
	}

	if _, err := fault.host.Run(ctx, blockHostCall, "sudo sh -c "+blockShellQuote(script)); err != nil {
		return fmt.Errorf("activate %s: %w", fault.kind, err)
	}

	fault.evidence.PersistentOrderingConfirmed = fault.persistent

	return nil
}

// Confirm checks installed rules, affected transport, and matching packet counters.
func (fault *BlockNetworkFault) Confirm(ctx context.Context) error {
	if !fault.prepared {
		return fmt.Errorf("prepare the network fault before confirmation")
	}

	if fault.kind == BlockStorageLatency {
		return fault.confirmLatency(ctx)
	}

	if _, err := fault.host.Run(ctx, blockHostCall, "sudo sh -c "+blockShellQuote(fault.checkRulesScript())); err != nil {
		return fmt.Errorf("check installed %s rules: %w", fault.kind, err)
	}

	if fault.kind != BlockStorageLoss && fault.kind != BlockStorageTimeout {
		apiAvailable := fault.kind != BlockAPILoss && fault.kind != BlockSingletonIsolation
		for _, pid := range fault.network.NamespacePIDs {
			for _, endpoint := range fault.network.API {
				if err := fault.probeTransport(ctx, pid, endpoint, apiAvailable); err != nil {
					return err
				}
			}
		}
	}

	if fault.kind == BlockGroupPartition || fault.kind == BlockSingletonIsolation {
		for _, address := range fault.network.Peers {
			if strings.Contains(address, "/") {
				continue
			}

			if err := fault.probeTransport(ctx, 1, BlockEndpoint{Address: address, Port: 10250}, false); err != nil {
				return err
			}
		}
	}

	script := "set -eu\ncount=0\n"
	for _, tool := range fault.families {
		script += "rules=$(" + tool + " -w 5 -L " + fault.evidence.Chain + " -n -v -x)\n" +
			"matched=$(printf '%s\\n' \"$rules\" | awk '" +
			"$3 == \"REJECT\" || $3 == \"DROP\" { count += $1 } END { print count+0 }')\n" +
			"count=$((count + matched))\n"
	}

	script += "test \"$count\" -gt 0\n"
	if _, err := fault.host.Run(ctx, blockHostCall, "sudo sh -c "+blockShellQuote(script)); err != nil {
		return fmt.Errorf("%s has no confirmed matching rejected or dropped traffic: %w", fault.kind, err)
	}

	if fault.evidence.ActiveAt.IsZero() {
		fault.evidence.ActiveAt = time.Now()
	}

	return nil
}

// Restore removes only owned rules and checks restored API transport when applicable.
func (fault *BlockNetworkFault) Restore(ctx context.Context) error {
	if !fault.prepared || fault.evidence.DispatchedAt.IsZero() || !fault.evidence.RestoredAt.IsZero() {
		return nil
	}

	fault.evidence.RestoreDispatchedAt = time.Now()
	var serviceErr error
	if fault.persistent {
		script := "set -eu\nunits=$(systemctl list-unit-files --no-legend --full " + fault.evidence.Unit + ")\n" +
			"if printf '%s\\n' \"$units\" | awk '$1 == \"" + fault.evidence.Unit +
			"\" { found=1 } END { exit !found }'; then\nsystemctl disable --now " + fault.evidence.Unit + "\nfi\n"
		if _, err := fault.host.Run(ctx, blockHostCall, "sudo sh -c "+blockShellQuote(script)); err != nil {
			serviceErr = fmt.Errorf("disable persistent %s rules: %w", fault.kind, err)
		}
	}

	script := fault.removeScript()
	if fault.kind == BlockStorageLatency {
		script = fault.latencyRemoveScript()
	}

	if _, err := fault.host.Run(ctx, blockHostCall, "sudo sh -c "+blockShellQuote(script)); err != nil {
		return errors.Join(serviceErr, fmt.Errorf("remove and check %s rules: %w", fault.kind, err))
	}

	if serviceErr != nil {
		return serviceErr
	}

	if fault.persistent {
		script = "set -eu\nrm -f -- /etc/systemd/system/" + fault.evidence.Unit +
			" /var/lib/" + fault.evidence.Unit + ".sh\nsystemctl daemon-reload\n" +
			"test ! -e /etc/systemd/system/" + fault.evidence.Unit + "\n" +
			"test ! -e /var/lib/" + fault.evidence.Unit + ".sh\n"
		if _, err := fault.host.Run(ctx, blockHostCall, "sudo sh -c "+blockShellQuote(script)); err != nil {
			return fmt.Errorf("remove persistent fault service: %w", err)
		}
	}

	if fault.kind != BlockStorageLoss && fault.kind != BlockStorageTimeout {
		for _, pid := range fault.network.NamespacePIDs {
			for _, endpoint := range fault.availableEndpoints(pid) {
				if err := fault.probeTransport(ctx, pid, endpoint, true); err != nil {
					return fmt.Errorf("check restored API transport: %w", err)
				}
			}
		}
	}

	fault.evidence.RestoredAt = time.Now()

	return nil
}

// ProbeAPI checks discovered API destinations from the host and current namespaces.
func (fault *BlockNetworkFault) ProbeAPI(ctx context.Context) error {
	for _, pid := range fault.network.NamespacePIDs {
		for _, endpoint := range fault.network.API {
			if err := fault.probeTransport(ctx, pid, endpoint, true); err != nil {
				return err
			}
		}
	}

	return nil
}

func (fault *BlockNetworkFault) availableEndpoints(pid int) []BlockEndpoint {
	if len(fault.reachableAPI[pid]) > 0 {
		return fault.reachableAPI[pid]
	}

	return fault.reachableAPI[1]
}

func (fault *BlockNetworkFault) probeTransport(
	ctx context.Context, pid int, endpoint BlockEndpoint, available bool,
) error {
	command := "exec 3<>/dev/tcp/" + endpoint.Address + "/" + strconv.Itoa(endpoint.Port)
	script := "sudo nsenter --target " + strconv.Itoa(pid) + " --net -- timeout 3 bash -c " + blockShellQuote(command)
	if !available {
		// A missing namespace or command must not satisfy a failed-transport assertion.
		script = "sudo sh -c " + blockShellQuote("set -eu\ntest -e /proc/"+strconv.Itoa(pid)+"/ns/net\n"+
			"set +e\n"+strings.TrimPrefix(script, "sudo ")+"\nresult=$?\nset -e\n"+
			"test \"$result\" -eq 1 -o \"$result\" -eq 124\n")
	}

	if _, err := fault.host.Run(ctx, blockHostCall, script); err != nil {
		return fmt.Errorf("transport to %s:%d in network namespace %d must have available=%t: %w",
			endpoint.Address, endpoint.Port, pid, available, err)
	}

	return nil
}

func (fault *BlockNetworkFault) familyNeeded(tool string) bool {
	for _, endpoint := range append(append([]BlockEndpoint{}, fault.network.API...), fault.network.Storage...) {
		if blockFamily(endpoint.Address) == tool {
			return true
		}
	}

	for _, address := range fault.network.Peers {
		if blockFamily(address) == tool {
			return true
		}
	}

	return false
}

func blockFamily(address string) string {
	if strings.Contains(address, ":") {
		return "ip6tables"
	}

	return "iptables"
}

func (fault *BlockNetworkFault) rules(tool string) []string {
	var rules []string
	for _, address := range fault.network.Management {
		if blockFamily(address) == tool {
			rules = append(rules, "-p tcp -d "+address+" --sport 22 -j RETURN", "-p tcp -s "+address+" --dport 22 -j RETURN")
		}
	}

	addEndpoints := func(endpoints []BlockEndpoint, target string) {
		for _, endpoint := range endpoints {
			if blockFamily(endpoint.Address) == tool {
				rules = append(rules, fmt.Sprintf("-p tcp -d %s --dport %d -j %s", endpoint.Address, endpoint.Port, target))
				if target == "RETURN" {
					rules = append(rules, fmt.Sprintf("-p tcp -s %s --sport %d -j RETURN", endpoint.Address, endpoint.Port))
				}
			}
		}
	}
	if fault.kind == BlockSingletonIsolation || fault.kind == BlockGroupPartition {
		addEndpoints(fault.network.Storage, "RETURN")
		if fault.kind == BlockGroupPartition {
			addEndpoints(fault.network.API, "RETURN")
		}

		for _, address := range fault.network.Peers {
			if blockFamily(address) == tool {
				rules = append(rules, "-d "+address+" -j REJECT", "-s "+address+" -j REJECT")
			}
		}
	}

	switch fault.kind {
	case BlockAPILoss, BlockSingletonIsolation:
		addEndpoints(fault.network.API, "REJECT")
	case BlockStorageLoss:
		addEndpoints(fault.network.Storage, "REJECT")
	case BlockStorageTimeout:
		addEndpoints(fault.network.Storage, "DROP")
	case BlockGroupPartition, BlockStorageLatency:
	}

	return rules
}

func (fault *BlockNetworkFault) applyScript() string {
	script := "set -eu\n"
	for _, tool := range fault.families {
		// A complete successful listing distinguishes absence from a query error.
		script += "rules=$(" + tool + " -w 5 -S)\n" +
			"if printf '%s\\n' \"$rules\" | awk '$1 == \"-N\" && $2 == \"" + fault.evidence.Chain +
			"\" { found=1 } END { exit !found }'; then\n" +
			tool + " -w 5 -F " + fault.evidence.Chain + "\nelse\n" + tool + " -w 5 -N " + fault.evidence.Chain + "\nfi\n"
		for _, rule := range fault.rules(tool) {
			script += tool + " -w 5 -A " + fault.evidence.Chain + " " + rule + "\n"
		}

		for _, hook := range []string{"OUTPUT", "FORWARD", "INPUT"} {
			script += "rules=$(" + tool + " -w 5 -S " + hook + ")\n" +
				"if ! printf '%s\\n' \"$rules\" | awk '$1 == \"-A\" && $2 == \"" + hook +
				"\" && $3 == \"-j\" && $4 == \"" + fault.evidence.Chain + "\" { found=1 } END { exit !found }'; then\n" +
				tool + " -w 5 -I " + hook + " 1 -j " + fault.evidence.Chain + "\nfi\n"
		}
	}

	return script
}

func (fault *BlockNetworkFault) checkRulesScript() string {
	script := "set -eu\n"
	for _, tool := range fault.families {
		for _, rule := range fault.rules(tool) {
			script += tool + " -w 5 -C " + fault.evidence.Chain + " " + rule + "\n"
		}

		for _, hook := range []string{"OUTPUT", "FORWARD", "INPUT"} {
			script += tool + " -w 5 -C " + hook + " -j " + fault.evidence.Chain + "\n"
		}
	}

	if fault.persistent {
		script += "systemctl is-enabled --quiet " + fault.evidence.Unit + "\n" +
			"systemctl is-active --quiet " + fault.evidence.Unit + "\n"
	}

	return script
}

func (fault *BlockNetworkFault) removeScript() string {
	script := "set -eu\n"
	for _, tool := range fault.families {
		for _, hook := range []string{"OUTPUT", "FORWARD", "INPUT"} {
			script += "while :; do\nrules=$(" + tool + " -w 5 -S " + hook + ")\n" +
				"if ! printf '%s\\n' \"$rules\" | awk '$1 == \"-A\" && $2 == \"" + hook +
				"\" && $3 == \"-j\" && $4 == \"" + fault.evidence.Chain + "\" { found=1 } END { exit !found }'; then break; fi\n" +
				tool + " -w 5 -D " + hook + " -j " + fault.evidence.Chain + "\ndone\n"
		}

		script += "rules=$(" + tool + " -w 5 -S)\n" +
			"if printf '%s\\n' \"$rules\" | awk '$1 == \"-N\" && $2 == \"" + fault.evidence.Chain +
			"\" { found=1 } END { exit !found }'; then\n" + tool + " -w 5 -F " + fault.evidence.Chain + "\n" +
			tool + " -w 5 -X " + fault.evidence.Chain + "\nfi\n" +
			"rules=$(" + tool + " -w 5 -S)\n" +
			"if printf '%s\\n' \"$rules\" | awk '$2 == \"" + fault.evidence.Chain + "\" || $NF == \"" +
			fault.evidence.Chain + "\" { found=1 } END { exit !found }'; then exit 1; fi\n"
	}

	return script
}

func (fault *BlockNetworkFault) prepareLatency(ctx context.Context) error {
	for _, endpoint := range fault.network.Storage {
		output, err := fault.host.Run(ctx, blockHostCall, "ip -j route get "+endpoint.Address)
		if err != nil {
			return &BlockPrerequisiteError{Reason: fmt.Sprintf("storage route discovery failed: %v", err)}
		}

		var routes []struct {
			Device string `json:"dev"`
		}
		if err := json.Unmarshal([]byte(output), &routes); err != nil || len(routes) != 1 || routes[0].Device == "" {
			return &BlockPrerequisiteError{Reason: "storage route must identify one network interface"}
		}

		if strings.ContainsAny(routes[0].Device, " \n\r'\"/;$`\\") {
			return fmt.Errorf("invalid storage route interface %q", routes[0].Device)
		}

		if fault.device != "" && fault.device != routes[0].Device {
			return &BlockPrerequisiteError{Reason: "storage latency requires all backend endpoints to use one network interface"}
		}

		fault.device = routes[0].Device
	}

	output, err := fault.host.Run(ctx, blockHostCall, "sudo tc -j qdisc show dev "+fault.device)
	if err != nil {
		return &BlockPrerequisiteError{Reason: fmt.Sprintf("read original traffic-control objects: %v", err)}
	}

	var qdiscs []struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(output), &qdiscs); err != nil {
		return fmt.Errorf("decode original traffic-control objects: %w", err)
	}

	for _, qdisc := range qdiscs {
		if qdisc.Kind != "noqueue" {
			return &BlockPrerequisiteError{Reason: "existing traffic-control objects conflict with the latency class"}
		}
	}

	return nil
}

func (fault *BlockNetworkFault) latencyApplyScript() string {
	script := "set -eu\ntc qdisc add dev " + fault.device +
		" root handle 7990: prio bands 3 priomap 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n" +
		"tc qdisc add dev " + fault.device + " parent 7990:3 handle 7991: netem delay 20s\n"
	for endpointIndex, endpoint := range fault.network.Storage {
		protocol := "ip"
		if blockFamily(endpoint.Address) == "ip6tables" {
			protocol = "ipv6"
		}

		script += fmt.Sprintf("tc filter add dev %s protocol %s parent 7990: prio %d "+
			"flower dst_ip %s ip_proto tcp dst_port %d flowid 7990:3\n",
			fault.device, protocol, endpointIndex+1, endpoint.Address, endpoint.Port)
	}

	return script
}

func (fault *BlockNetworkFault) confirmLatency(ctx context.Context) error {
	output, err := fault.host.Run(ctx, blockHostCall, "sudo tc -j -s qdisc show dev "+fault.device)
	if err != nil {
		return fmt.Errorf("read owned storage latency counters: %w", err)
	}

	var qdiscs []struct {
		Kind    string `json:"kind"`
		Handle  string `json:"handle"`
		Packets uint64 `json:"packets"`
		Options struct {
			Delay struct {
				Seconds float64 `json:"delay"`
			} `json:"delay"`
		} `json:"options"`
	}
	if err := json.Unmarshal([]byte(output), &qdiscs); err != nil {
		return fmt.Errorf("decode owned storage latency counters: %w", err)
	}

	matched := false
	for _, qdisc := range qdiscs {
		if qdisc.Kind == "netem" && qdisc.Handle == "7991:" && qdisc.Packets > 0 && qdisc.Options.Delay.Seconds == 20 {
			matched = true
		}
	}

	if !matched {
		return fmt.Errorf("the owned 20s storage-latency class must contain matching traffic")
	}

	output, err = fault.host.Run(ctx, blockHostCall, "sudo tc -j filter show dev "+fault.device+" parent 7990:")
	if err != nil {
		return fmt.Errorf("read current storage-latency filters: %w", err)
	}

	var filters []struct {
		Kind    string `json:"kind"`
		Options struct {
			ClassID string `json:"classid"`
			Keys    struct {
				Address  string `json:"dst_ip"`
				Protocol string `json:"ip_proto"`
				Port     int    `json:"dst_port"`
			} `json:"keys"`
		} `json:"options"`
	}
	if err := json.Unmarshal([]byte(output), &filters); err != nil {
		return fmt.Errorf("decode current storage-latency filters: %w", err)
	}

	for _, endpoint := range fault.network.Storage {
		found := false
		for _, filter := range filters {
			if filter.Kind == "flower" && filter.Options.ClassID == "7990:3" &&
				filter.Options.Keys.Address == endpoint.Address && filter.Options.Keys.Port == endpoint.Port &&
				filter.Options.Keys.Protocol == "tcp" {
				found = true
			}
		}

		if !found {
			return fmt.Errorf("the storage-latency filter for %s:%d is absent", endpoint.Address, endpoint.Port)
		}
	}

	for _, pid := range fault.network.NamespacePIDs {
		for _, endpoint := range fault.network.API {
			if err := fault.probeTransport(ctx, pid, endpoint, true); err != nil {
				return err
			}
		}
	}

	if fault.evidence.ActiveAt.IsZero() {
		fault.evidence.ActiveAt = time.Now()
	}

	return nil
}

func (fault *BlockNetworkFault) latencyRemoveScript() string {
	return "set -eu\nobjects=$(tc qdisc show dev " + fault.device + ")\n" +
		"if printf '%s\\n' \"$objects\" | awk '$2 == \"prio\" && $3 == \"7990:\" { found=1 } END { exit !found }'; then\n" +
		"tc qdisc del dev " + fault.device + " root handle 7990:\nfi\n" +
		"objects=$(tc qdisc show dev " + fault.device + ")\n" +
		"if printf '%s\\n' \"$objects\" | awk '$3 == \"7990:\" || $3 == \"7991:\" " +
		"{ found=1 } END { exit !found }'; then exit 1; fi\n"
}

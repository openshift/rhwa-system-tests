//nolint:godox,lll // Keep the required TODO assertions and approved step strings in their original form.
package tests

type bpnPlan struct {
	id    string
	title string
	steps []string
}

var bpnPlans = []bpnPlan{
	{
		// TODO(RHWA-1839-01): Check an existing application during the interval. This needs an application-specific health endpoint and must not affect this test's result.
		id:    "RHWA-1839-01",
		title: "Healthy storage and API — matrix row 1",
		steps: []string{
			"Complete the baseline and observer-readiness procedure.",
			"Record SBRStorageUnhealthy=False and the boot ID of each selected Node.",
			"Observe all three Nodes for NO_REBOOT without injecting a fault.",
			"Read each actual host boot ID again.",
			"Delete the test resources through common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-02): Repeat during an actual API server outage. This needs an independent control-plane recovery procedure and independent API-event evidence.
		id:    "RHWA-1839-02",
		title: "API unavailable to all selected SBR nodes — matrix row 2",
		steps: []string{
			"Complete the baseline and confirm that all observers are ready.",
			"Prepare API-loss rules for all three selected Nodes.",
			"Activate those rules. Confirm API transport failure from each affected namespace.",
			"Start the 300s interval after the last Node confirms the fault.",
			"Sample actual host boot IDs throughout the interval.",
			"Remove all API rules. Observe restored access and healthy storage conditions.",
			"Continue boot observations for POST_RESTORE before common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-03A): Test NHC threshold behavior separately. This needs a defined condition-publication schedule and must not claim that thresholds prevent every reboot.
		id:    "RHWA-1839-03A",
		title: "Broad storage failure without an authorized request — matrix row 3",
		steps: []string{
			"Complete the baseline and observer-readiness procedure.",
			"Confirm the test NHC contains pauseRequests: [sbr-block-test].",
			"Reject storage traffic on all three workers.",
			"Confirm each fault. Require SBRStorageUnhealthy=True on each Node within HEALTH_CHANGE.",
			"Keep all faults active for NO_REBOOT after the last activation.",
			"Remove the faults. Require False storage conditions.",
			"Observe POST_RESTORE before common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-03B): Remove one peer during the request and check completion through the remaining peer. This needs a separate controlled peer-stop fixture.
		id:    "RHWA-1839-03B",
		title: "Requested reboot while every selected node has storage failure — matrix row 3",
		steps: []string{
			"Complete the baseline and start all required observers.",
			"Reject storage traffic on all three workers. Use the persistent fault handle on the target.",
			"Require True storage conditions on all three Nodes within HEALTH_CHANGE.",
			"Create one SBR CR for the target from the runner.",
			"Keep both peer storage faults active while observing the target reboot and CR result.",
			"Require the common requested-remediation result.",
			"Restore all storage access. Normally delete the CR after the target reports False.",
			"Complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-04A): Count every boot during the initial SSH blackout. This needs a trusted host boot counter and does not change this test's pass result.
		id:    "RHWA-1839-04A",
		title: "Combined storage and API outage — matrix row 4",
		steps: []string{
			"Complete the baseline, autonomous preparation, and observer-readiness procedure.",
			"Prepare persistent storage-loss and API-loss rules on the target.",
			"Establish the first combined outage in the order specified in Section 4.5.",
			"Observe an actual changed host boot ID within REBOOT.",
			"Confirm that both faults still apply after reconnection.",
			"Observe the new boot ID for NO_REBOOT while the outage continues.",
			"Restore storage and API access. Require False storage health and matching API/host boot IDs.",
			"Observe POST_RESTORE before common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-04B): Repeat with a still-active accepted request. This needs a repeatable external timing window and must not become an internal execution-path test.
		id:    "RHWA-1839-04B",
		title: "Autonomous reboot preserves a previous request identity — matrix row 4",
		steps: []string{
			"Complete the baseline and start observers before the preparatory request.",
			"Create one SBR CR. Require the common requested-remediation result.",
			"Normally delete that CR. Require release of its restrictions.",
			"Record its retained execution.requestUID from sbr.medik8s.io/remediation-owner.",
			"Record the current host boot ID. Complete autonomous preparation.",
			"Establish the first combined outage through Section 4.5. Observe a changed host boot ID.",
			"Keep the outage active for NO_REBOOT after that changed ID.",
			"Restore access. Compare the retained request UID with the recorded value.",
			"Complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-05A): Repeat with API transport failures through an additional endpoint topology. This needs that topology and no new required assertion.
		id:    "RHWA-1839-05A",
		title: "One node loses API access — matrix row 5",
		steps: []string{
			"Complete the baseline and confirm observer readiness.",
			"Reject the target's API traffic. Confirm the transport fault.",
			"Observe the target and peers for NO_REBOOT.",
			"Remove the API fault. Require restored transport and False storage health.",
			"Observe POST_RESTORE. Complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-05B): Observe a VM during the same partition. This needs an existing VM fixture and an independent workload observer.
		id:    "RHWA-1839-05B",
		title: "One node loses cluster communication — matrix row 5",
		steps: []string{
			"Complete the baseline and confirm all observers are ready.",
			"Prepare the singleton-isolation rules with explicit storage and SSH exceptions.",
			"Activate the rules. Confirm blocked API and peer transport from worker A.",
			"Observe actual host boot IDs and target CR events for NO_REBOOT.",
			"Remove the isolation rules. Require restored connections and False storage health.",
			"Observe POST_RESTORE before common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-06): Check VMI removal or CSI attachment removal. These need suitable existing workload fixtures and are independent of this initial Pod result.
		id:    "RHWA-1839-06",
		title: "NHC remediates one storage-unhealthy node — matrix row 6",
		steps: []string{
			"Complete the baseline. Start CR, Node, host, and workload observers before the fault.",
			"Record the workload Pod UID and all host boot IDs.",
			"Reject storage traffic only on the target. Confirm the fault.",
			"Require SBRStorageUnhealthy=True within HEALTH_CHANGE.",
			"Observe the NHC-created SBR CR within REQUEST_CREATE. Record its UID.",
			"Require the common requested-remediation result.",
			"Require the original workload Pod UID to disappear within WORKLOAD_GONE after OutOfService is observed.",
			"Restore target storage. Require False storage health.",
			"Let NHC delete the CR after its 600s healthy delay. Require SBR restriction release.",
			"Complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-07): Use a true kernel-freeze stimulus. This needs a supported trigger and recovery method already available on the target platform.
		id:    "RHWA-1839-07",
		title: "Existing SysRq crash stimulus — matrix row 7",
		steps: []string{
			"Complete the baseline and confirm Node and host observers are ready.",
			"Save /proc/sys/kernel/sysrq and /proc/sys/kernel/panic through SSH.",
			"Set SysRq to 1. Run sync. Send SysRq s.",
			"Wait SYSRQ_SETTLE. Set kernel.panic to SYSRQ_PANIC_DELAY.",
			"Send c to /proc/sysrq-trigger through the prepared SSH connection.",
			"Observe a changed actual host boot ID within REBOOT.",
			"Restore the saved settings after the target returns.",
			"Require Ready and False storage health. Complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-08): Retain a completed remediation CR during this restart and check for no repeated host reboot. This needs one preparatory remediation cycle.
		id:    "RHWA-1839-08",
		title: "Planned SBR agent Pod restart — matrix row 8",
		steps: []string{
			"Complete the baseline and confirm observers are ready.",
			"Record the target agent Pod UID and all host boot IDs.",
			"Delete only the target agent Pod through normal Kubernetes deletion.",
			"Require a different agent Pod UID on the same target Node within AGENT_READY.",
			"Require the replacement agent to become Ready and the target storage condition to be False.",
			"Continue unchanged-host-boot observation for NO_REBOOT after the replacement becomes Ready.",
			"Complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-09): Check an existing application during the stopped-kubelet interval. This needs a workload health check independent of kubelet status.
		id:    "RHWA-1839-09",
		title: "Kubelet stops while storage remains healthy — matrix row 9",
		steps: []string{
			"Complete the baseline and confirm observers are ready.",
			"Stop target kubelet through SSH.",
			"Confirm systemctl is-active kubelet reports inactive.",
			"Hold kubelet inactive for NO_REBOOT. Continue host boot-ID and API observations.",
			"Start kubelet through SSH. Confirm that it is active.",
			"Require the Node to return Ready. Observe POST_RESTORE.",
			"Complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-10A): Check a supported sub-timeout latency profile without unhealthy storage. This needs a separately agreed complete-probe latency bound.
		id:    "RHWA-1839-10A",
		title: "Slow storage exceeds the configured probe deadline — matrix row 10",
		steps: []string{
			"Complete the baseline and confirm observers are ready.",
			"Prepare the owned storage traffic class with a 20s delay.",
			"Activate the class. Confirm that storage traffic uses it.",
			"Require SBRStorageUnhealthy=True within HEALTH_CHANGE.",
			"Keep the latency active for NO_REBOOT after activation.",
			"Remove the owned traffic-control objects. Require False storage health.",
			"Observe POST_RESTORE before common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-10B): Repeat with a qualified device-level I/O hang. This needs a reversible driver-specific fault already available on the cluster.
		id:    "RHWA-1839-10B",
		title: "Storage traffic remains unanswered — matrix row 10",
		steps: []string{
			"Complete the baseline and confirm observers are ready.",
			"Activate target storage DROP rules. Confirm matching traffic.",
			"Require SBRStorageUnhealthy=True within HEALTH_CHANGE.",
			"Keep the DROP rules active for NO_REBOOT after activation.",
			"Remove the DROP rules. Require False storage health.",
			"Observe POST_RESTORE before common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-10C): Restart one peer while the other remains storage-unhealthy. This needs a separate controlled peer-restart case.
		id:    "RHWA-1839-10C",
		title: "Peers process a request while their storage times out — matrix row 10",
		steps: []string{
			"Complete the baseline and confirm observers are ready.",
			"Drop storage traffic on both peers. Confirm both faults.",
			"Require True storage conditions on both peers within HEALTH_CHANGE.",
			"Create a runner-owned SBR CR for the target.",
			"Require the common requested-remediation result while both peer faults remain active.",
			"Confirm that neither peer changed its host boot ID.",
			"Restore peer storage. Normally delete the target CR.",
			"Complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-11): Check a pre-existing application's availability during the partition. This needs an application-specific observer and does not establish duplicate-writer protection.
		id:    "RHWA-1839-11",
		title: "Group partition with healthy storage — matrix row 11",
		steps: []string{
			"Complete the baseline and confirm observers are ready.",
			"Prepare rules for both directions of the worker-group partition.",
			"Activate all rules. Confirm blocked cross-group transport and retained API transport.",
			"Observe all selected Nodes for NO_REBOOT after the last rule becomes active.",
			"Remove all partition rules. Confirm cross-group transport returns.",
			"Observe POST_RESTORE before common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-12A): Check fresh storage success before the requested reboot. This needs a sufficiently long externally observable acceptance window and adds no initial pass condition.
		id:    "RHWA-1839-12A",
		title: "Storage recovers after request acceptance — matrix row 12",
		steps: []string{
			"Complete the baseline and confirm observers are ready.",
			"Reject target storage traffic. Require True storage health.",
			"Record the original host boot ID. Create a target SBR CR.",
			"Wait for a public Accepted record matching this CR UID and Node UID.",
			"Immediately remove the storage fault through prepared SSH.",
			"Record the removal acknowledgement. Immediately read the actual host boot ID.",
			"Require that this read still equals the original boot ID. Otherwise, fail the test for a timing miss.",
			"Observe the later changed boot ID and require the common requested-remediation result.",
			"Normally delete the CR and complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-12B): Repeat with a longer post-acceptance API interruption. This needs a repeatable external timing window without modifying SBR internals.
		id:    "RHWA-1839-12B",
		title: "API recovers after request acceptance — matrix row 12",
		steps: []string{
			"Complete the baseline and confirm observers are ready.",
			"Prepare the target API fault without activating it.",
			"Record the original host boot ID. Create a target SBR CR.",
			"Observe the matching public Accepted record.",
			"Activate API-loss rules immediately. Confirm transport failure from the affected namespace.",
			"Remove the API rules immediately. Confirm restored transport.",
			"Read the actual host boot ID. Require the original value to remain present.",
			"Observe the subsequent reboot and common requested-remediation result.",
			"Normally delete the CR and complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-15A): Retain a VMI or attachment instead of a Pod. This needs an existing suitable fixture and a reversible retention method.
		id:    "RHWA-1839-15A",
		title: "A terminating Pod does not retain the remediation CR — matrix row 15",
		steps: []string{
			"Complete the baseline and start all Node, CR, host, and Pod observers.",
			"Record the test Pod UID. Request normal Pod deletion without removing its finalizer.",
			"Require a deletion timestamp and the unchanged test finalizer within FAULT_READY.",
			"Create a target SBR CR and observe the host reboot.",
			"Observe OutOfService and the CR's fixed cleanup start and deadline.",
			"Keep the finalizer present through the stored cleanup deadline.",
			"Require terminal CR conditions by the deadline plus PUBLISH.",
			"Normally delete the CR while the same terminating Pod still exists.",
			"Require the CR to disappear and SBR's restrictions to disappear within RELEASE.",
			"Remove the test-owned Pod finalizer. Complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-15B): Keep two completed CRs on the same Node and delete them separately. This needs two request cycles and checks retention of shared restrictions.
		id:    "RHWA-1839-15B",
		title: "Preserve administrator-owned restrictions during CR deletion — matrix row 15",
		steps: []string{
			"Start observers and complete the common healthy preparation before adding the administrator fixtures.",
			"Make the test Pod terminating with its finalizer retained, as in case 15A.",
			"Add the three administrator fixtures. Record their exact values before creating the CR.",
			"Confirm that agents and reporters remain available. Record the target boot baseline again.",
			"Create a target SBR CR. Observe its reboot and fixed cleanup deadline.",
			"Keep the Pod retained through expiry. Require terminal conditions with Incomplete or Unknown cleanup.",
			"Normally delete the CR while the Pod and administrator fixtures still exist.",
			"Require CR removal within RELEASE. Check that all three administrator fixtures remain unchanged.",
			"Remove only the administrator fixtures added by this case. Remove the test Pod finalizer.",
			"Complete common cleanup against the original Node baseline.",
		},
	},
	{
		// TODO(RHWA-1839-16A): Check the public degradation reason once its exact resource and field are fixed. This needs a stable API location, not log parsing.
		id:    "RHWA-1839-16A",
		title: "Journal writes unavailable before a new requested reboot — matrix row 16",
		steps: []string{
			"Complete the baseline and confirm observers are ready.",
			"Save the state directory's existing flags. Add only its immutable flag.",
			"Confirm that a test-owned temporary file cannot be created in that directory.",
			"Create a runner-owned SBR CR for the target.",
			"Keep the directory unavailable for NO_REBOOT after CR creation.",
			"Require the unchanged host boot ID and no success condition for that CR throughout the interval.",
			"Request normal CR deletion while the directory remains unavailable.",
			"Require CR removal and release of any SBR restrictions within RELEASE.",
			"Restore the directory flag. Check temporary file creation and removal.",
			"Complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-16B): Check automatic resumption without an agent restart. This needs a specified public recovery contract and must not affect this initial result.
		id:    "RHWA-1839-16B",
		title: "Restore journal access and resume requested remediation — matrix row 16",
		steps: []string{
			"Complete the baseline and confirm observers are ready.",
			"Apply the immutable-directory fault. Confirm failed temporary file creation.",
			"Restore the original directory flags. Confirm successful temporary file creation and removal.",
			"Normally delete the target agent Pod once so its replacement starts with the restored directory.",
			"Require a new Ready agent Pod, False storage health, and the unchanged host boot ID.",
			"Create a new target SBR CR after that restart completes.",
			"Require the common requested-remediation result.",
			"Normally delete the CR. Complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-16C): Repeat with delayed host-filesystem writes. This needs a reversible host fault that does not affect the peer's API process.
		id:    "RHWA-1839-16C",
		title: "Peers reconcile while their journal directories reject writes — matrix row 16",
		steps: []string{
			"Complete the baseline and confirm all observers are ready.",
			"Apply the immutable-directory fault to each peer. Confirm both access failures.",
			"Create a valid SBR CR for the unaffected target.",
			"Keep both peer faults active while observing the requested result.",
			"Require the common requested-remediation result and unchanged peer host boot IDs.",
			"Restore both peer directory flags. Check temporary file creation and removal.",
			"Normally delete the target CR. Complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-17A): Count every boot during each initial blackout. This needs a trusted boot counter. Two observed request cycles do not prove an unlimited number of cycles.
		id:    "RHWA-1839-17A",
		title: "Successive valid requests during persistent storage failure — matrix row 17",
		steps: []string{
			"Complete the baseline and confirm observers are ready.",
			"Apply persistent target storage loss. Require True storage health.",
			"Create CR A. Record its UID and require the common requested-remediation result.",
			"Retain CR A for NO_REBOOT after its result. Require the new host boot ID to remain unchanged.",
			"Normally delete CR A. Require release of its restrictions.",
			"Record the current host boot ID. Create CR B with the same target name and a different UID.",
			"Require the common requested-remediation result for CR B against this new baseline.",
			"Retain CR B for NO_REBOOT. Require its resulting host boot ID to remain unchanged.",
			"Restore storage. Normally delete CR B and complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-17B): Repeat with more requested CRs during the same unresolved outage. This needs additional reboot cycles but no internal state assertions.
		id:    "RHWA-1839-17B",
		title: "A requested reboot remains permitted after an autonomous reboot — matrix row 17",
		steps: []string{
			"Complete autonomous preparation and confirm all observers are ready.",
			"Establish the first combined outage through Section 4.5. Observe the first changed host boot ID.",
			"Keep both faults active for NO_REBOOT. Require no further observed reboot.",
			"Restore only API access. Keep storage unavailable and require True storage health when reporting resumes.",
			"Record the current host boot ID. Create a new valid target SBR CR.",
			"Require its requested reboot and common terminal result despite the earlier autonomous attempt.",
			"Normally delete the CR with API access still available.",
			"Record the resulting host boot ID. Block target API access again while storage remains unavailable.",
			"Observe NO_REBOOT without another changed boot ID.",
			"Restore both paths. Complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-17C): Repeat with several brief recoveries. This needs additional outage cycles and must still use bounded external observations.
		id:    "RHWA-1839-17C",
		title: "Brief recovery does not permit another autonomous reboot — matrix row 17",
		steps: []string{
			"Complete the baseline, autonomous preparation, and observer-readiness procedure.",
			"Establish the first combined outage through Section 4.5. Observe a changed host boot ID.",
			"Keep combined loss active for NO_REBOOT after that boot. Require an unchanged new boot ID.",
			"Restore both paths. Start the brief recovery window at the first restoration command's dispatch time.",
			"Confirm restored API transport and False storage health within that 20s window.",
			"Dispatch both fault commands at 20s. Confirm both faults before the window reaches 60s.",
			"Observe the same host boot ID for NO_REBOOT after confirmed combined loss.",
			"Restore both paths and complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-17D): Repeat with the omitted five-minute default. This needs a longer run and is not covered by the configured one-minute result.
		id:    "RHWA-1839-17D",
		title: "Continuous recovery permits a later autonomous reboot — matrix row 17",
		steps: []string{
			"Complete the baseline and autonomous preparation. Confirm observers are ready.",
			"Establish the first combined outage through Section 4.5. Observe the first changed host boot ID.",
			"Keep the outage active for NO_REBOOT after that boot. Require an unchanged new boot ID.",
			"Restore both paths. Require False storage health and live API transport.",
			"Observe both healthy results continuously for 60s plus REARM_SETTLE, totaling 120s.",
			"Record the current host boot ID. Apply a new persistent combined outage.",
			"Require another changed host boot ID within REBOOT.",
			"Keep that outage active for NO_REBOOT. Require no further observed reboot.",
			"Restore both paths and complete common cleanup.",
		},
	},
	{
		// TODO(RHWA-1839-17E): Interrupt recovery with a storage failure instead of API loss. This needs a bounded, reversible fault whose first failed probe occurs inside the shorter interval.
		id:    "RHWA-1839-17E",
		title: "An API interruption restarts the recovery interval — matrix row 17",
		steps: []string{
			"Complete autonomous preparation and confirm all observers are ready.",
			"Establish the first combined outage through Section 4.5. Observe the first changed host boot ID.",
			"Keep the outage active for NO_REBOOT. Require no further observed reboot.",
			"Restore both paths. Start the first recovery window at the first restoration command's dispatch time.",
			"Confirm False storage health and live API transport. Record when both results become available.",
			"Dispatch the API fault at 40s. Keep confirmed API loss active for RECOVERY_BREAK=20s while storage remains available.",
			"Restore API access. Start the second recovery window at that command's dispatch time. Confirm both healthy results again.",
			"Dispatch combined loss at the second window's 40s point. Require each possible healthy interval to remain below 60s.",
			"Require the confirmed healthy intervals to total more than 60s. Apply the bounds in Section 4.5.",
			"Observe the current host boot ID for NO_REBOOT without a further change.",
			"Restore both paths and complete common cleanup.",
		},
	},
}

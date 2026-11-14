package waoparams

import "time"

const (
	// WebhookServiceName is the Service in front of the WAO admission listener. Every webhook
	// entry that WAO serves names it in clientConfig.service.
	WebhookServiceName = "workload-availability-operator-controller-manager-service"

	// WebhookTestPrefix starts the name of every object the webhook specs create or send, so a
	// stale object from an interrupted run is recognisable and safe to delete.
	WebhookTestPrefix = "wao-webhook-test"

	// WebhookTestNodeName is a node name that must not exist. Remediation requests and node
	// parameters use it so that no request can refer to a real node.
	WebhookTestNodeName = WebhookTestPrefix + "-no-such-node"

	// AdmissionOutagePolicyName is the NetworkPolicy that isolates the WAO admission listener.
	AdmissionOutagePolicyName = WebhookTestPrefix + "-deny-admission"

	// AdmissionRecoveryTimeout bounds the wait for admission to answer again after the listener
	// was isolated or the WAO manager Pods were deleted. A restart includes the init container
	// that reads the fence agent inventory from the FAR image.
	AdmissionRecoveryTimeout = 5 * time.Minute

	// AdmissionOutageTimeout bounds the wait for the API server to lose the listener after the
	// NetworkPolicy is created. Each attempt already takes one webhook timeout.
	AdmissionOutageTimeout = 3 * time.Minute

	// StoredObjectTimeout bounds the removal of an object the webhook specs stored.
	StoredObjectTimeout = 2 * time.Minute

	// FailurePolicyFail and FailurePolicyIgnore are the two failurePolicy values of the entries.
	FailurePolicyFail = "Fail"
	// FailurePolicyIgnore admits a request when the listener does not answer.
	FailurePolicyIgnore = "Ignore"
)

// WebhookEntry is one admission webhook that WAO registers and serves.
type WebhookEntry struct {
	// Name is the webhook name, which the API server quotes in every denial.
	Name string
	// Mutating is true for a MutatingWebhookConfiguration entry.
	Mutating bool
	// Group and Resource identify the one rule of the entry.
	Group    string
	Resource string
	// Operations are the admission operations of the rule, in registration order.
	Operations []string
	// FailurePolicy is what the API server does when the listener does not answer.
	FailurePolicy string
	// TimeoutSeconds is the call timeout of the entry.
	TimeoutSeconds int32
}

const (
	farGroup = "fence-agents-remediation.medik8s.io"
	nhcGroup = "remediation.medik8s.io"
	nmoGroup = "nodemaintenance.medik8s.io"
	snrGroup = "self-node-remediation.medik8s.io"
)

var (
	createUpdate       = []string{"CREATE", "UPDATE"}
	createUpdateDelete = []string{"CREATE", "UPDATE", "DELETE"}

	// ManagerPodLabels select the WAO manager Pods, which hold the admission listener.
	ManagerPodLabels = map[string]string{
		"app.kubernetes.io/name": "workload-availability-operator",
		"control-plane":          "controller-manager",
	}

	// WebhookEntries lists every entry WAO must register. Keep in step with
	// config/webhook/manifests.yaml and config/webhook/kustomization.yaml of the
	// workload-availability-operator. failurePolicy Ignore on vselfnoderemediation.kb.io and the
	// 15 second timeout of vnodemaintenance.kb.io are the values of the standalone operators.
	WebhookEntries = []WebhookEntry{
		{"mfenceagentsremediation.kb.io", true, farGroup, "fenceagentsremediations", createUpdate, FailurePolicyFail, 10},
		{
			"mfenceagentsremediationtemplate.kb.io", true, farGroup, "fenceagentsremediationtemplates",
			createUpdate, FailurePolicyFail, 10,
		},
		{
			"mselfnoderemediationtemplate.kb.io", true, snrGroup, "selfnoderemediationtemplates",
			createUpdate, FailurePolicyFail, 10,
		},
		{"vfenceagentsremediation.kb.io", false, farGroup, "fenceagentsremediations", createUpdate, FailurePolicyFail, 10},
		{
			"vfenceagentsremediationtemplate.kb.io", false, farGroup, "fenceagentsremediationtemplates",
			createUpdate, FailurePolicyFail, 10,
		},
		{"vnodehealthcheck.kb.io", false, nhcGroup, "nodehealthchecks", createUpdateDelete, FailurePolicyFail, 10},
		{"vnodemaintenance.kb.io", false, nmoGroup, "nodemaintenances", createUpdate, FailurePolicyFail, 15},
		{"vselfnoderemediation.kb.io", false, snrGroup, "selfnoderemediations", createUpdate, FailurePolicyIgnore, 10},
		{
			"vselfnoderemediationconfig.kb.io", false, snrGroup, "selfnoderemediationconfigs",
			createUpdateDelete, FailurePolicyFail, 10,
		},
		{
			"vselfnoderemediationtemplate.kb.io", false, snrGroup, "selfnoderemediationtemplates",
			createUpdate, FailurePolicyFail, 10,
		},
	}

	// AdmissionOperands are the operands whose admission WAO serves. WAO reports
	// status.components[].admissionReady for exactly these.
	AdmissionOperands = []string{"fenceAgentsRemediation", "nodeHealthCheck", "nodeMaintenance", "selfNodeRemediation"}
)

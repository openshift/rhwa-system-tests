package sbrparams

import "time"

// BlockPerNode parameters define the fixed configuration and observation limits.
const (
	BPNPolicyOnTimeout = "OnTimeout"
	BPNPolicyOnReboot  = "OnReboot"
	BPNRebootGrace     = 60 * time.Second

	BPNMinWorkers             = 2
	BPNSBRTimeoutSeconds      = 30
	BPNMaxConsecutiveFailures = 7

	BPNIOTimeout              = 15 * time.Second
	BPNEvaluationInterval     = 5 * time.Second
	BPNTotalOutageGracePeriod = 60 * time.Second
	BPNRecoveryPeriod         = time.Minute
	BPNBootUnitTimeout        = 4 * time.Minute
	BPNBootUnitStartAllowance = 30 * time.Second
	BPNNHCUnhealthyDuration   = 30 * time.Second
	BPNWatchdogMaxTimeout     = 120 * time.Second

	BPNBaseline        = 60 * time.Second
	BPNObserverStart   = 60 * time.Second
	BPNPoll            = 5 * time.Second
	BPNAPICall         = 10 * time.Second
	BPNHostCall        = 15 * time.Second
	BPNFaultReady      = 60 * time.Second
	BPNHealthChange    = 180 * time.Second
	BPNRequestCreate   = 300 * time.Second
	BPNReboot          = 1200 * time.Second
	BPNNodeReady       = 900 * time.Second
	BPNAgentReady      = 600 * time.Second
	BPNWorkloadReady   = 120 * time.Second
	BPNNoReboot        = 300 * time.Second
	BPNPostRestore     = 60 * time.Second
	BPNCleanupPeriod   = 300 * time.Second
	BPNPublish         = 60 * time.Second
	BPNWorkloadGone    = 300 * time.Second
	BPNRelease         = 300 * time.Second
	BPNNHCHealthyDelay = 60 * time.Second
	BPNRecoveryPart    = 40 * time.Second
	BPNRecoveryBreak   = 20 * time.Second
	BPNRearmSettle     = 60 * time.Second
	BPNSysRqSettle     = 2 * time.Second
	BPNSysRqPanicDelay = 180 * time.Second

	BPNInterruptedRecoveryPeriod = 180 * time.Second
	BPNInterruptedRecoveryPart   = 110 * time.Second
)

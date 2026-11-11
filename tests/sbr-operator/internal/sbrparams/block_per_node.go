package sbrparams

import "time"

// BlockPerNode parameters define the fixed configuration and observation limits.
const (
	BPNWorkers                = 3
	BPNNHCMinHealthy          = 2
	BPNSBRTimeoutSeconds      = 30
	BPNMaxConsecutiveFailures = 7

	BPNIOTimeout              = 15 * time.Second
	BPNEvaluationInterval     = 5 * time.Second
	BPNTotalOutageGracePeriod = 60 * time.Second
	BPNProfileDRecovery       = 5 * time.Minute
	BPNProfileRRecovery       = time.Minute
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
	BPNNHCHealthyDelay = 600 * time.Second
	BPNLatency         = 20 * time.Second
	BPNBriefRecovery   = 20 * time.Second
	BPNRecoveryPart    = 40 * time.Second
	BPNRecoveryBreak   = 20 * time.Second
	BPNRearmSettle     = 60 * time.Second
	BPNSysRqSettle     = 2 * time.Second
	BPNSysRqPanicDelay = 180 * time.Second
)

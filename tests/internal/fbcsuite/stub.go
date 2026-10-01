package fbcsuite

import (
	"context"

	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
)

type stubUpgradeOperatorTest struct{}

// NewStubUpgradeOperatorTest creates an operator upgrade test with no operator-specific behavior.
func NewStubUpgradeOperatorTest(medik8sparams.FBCUpgradeInputs) UpgradeOperatorFBCTest {
	return &stubUpgradeOperatorTest{}
}

func (*stubUpgradeOperatorTest) Setup(context.Context) error { return nil }

func (*stubUpgradeOperatorTest) BeforeUpgrade(context.Context) error { return nil }

func (*stubUpgradeOperatorTest) AfterUpgrade(context.Context) error { return nil }

func (*stubUpgradeOperatorTest) Cleanup(context.Context) {}

func (*stubUpgradeOperatorTest) FailureEvidence(context.Context) interface{} { return nil }

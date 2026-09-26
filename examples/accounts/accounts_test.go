// accounts 的校验跑的就是 Scenario() 里的四步，没有第二份断言。
package accounts

import (
	"testing"

	"github.com/sumwai/nova/examples/internal/harness"
)

func TestAccounts(t *testing.T) {
	harness.Run(t, Scenario())
}

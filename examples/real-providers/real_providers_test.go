// real-providers 的校验跑的就是 Scenario() 里的两步，没有第二份断言。
package realproviders

import (
	"testing"

	"github.com/sumwai/nova/examples/internal/harness"
)

func TestRealProviders(t *testing.T) {
	harness.Run(t, Scenario())
}

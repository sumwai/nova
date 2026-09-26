// discover 的校验跑的就是 Scenario() 里的六步，没有第二份断言。
package discover

import (
	"testing"

	"github.com/sumwai/nova/examples/internal/harness"
)

func TestDiscover(t *testing.T) {
	harness.Run(t, Scenario())
}

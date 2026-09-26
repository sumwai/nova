// quickstart 的校验跑的就是 Scenario() 里的脚本，没有第二份断言。
package quickstart

import (
	"testing"

	"github.com/sumwai/nova/examples/internal/harness"
)

func TestQuickstart(t *testing.T) {
	harness.Run(t, Scenario())
}

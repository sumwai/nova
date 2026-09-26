// convert 的校验跑的就是 Scenario() 里的 32 步矩阵，没有第二份断言。
package convert

import (
	"testing"

	"github.com/sumwai/nova/examples/internal/harness"
)

func TestConvert(t *testing.T) {
	harness.Run(t, Scenario())
}

// route-fallback 的校验跑的就是 Scenario() 里的三步，没有第二份断言。
package routefallback

import (
	"testing"

	"github.com/sumwai/nova/examples/internal/harness"
)

func TestRouteFallback(t *testing.T) {
	harness.Run(t, Scenario())
}

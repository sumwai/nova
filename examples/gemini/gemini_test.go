// gemini 的校验跑的就是 Scenario() 里的五步，没有第二份断言。
package gemini

import (
	"testing"

	"github.com/sumwai/nova/examples/internal/harness"
)

func TestGemini(t *testing.T) {
	harness.Run(t, Scenario())
}

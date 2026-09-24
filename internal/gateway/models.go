package gateway

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/sumwai/nova/internal/domain"
)

// modelsPath 是客户端查询模型清单的路径。
//
// 它与三个转发路径同级挂在数据面上：OpenAI 与 Anthropic 的模型清单接口路径都是它，
// 形状由请求头判定（见 protocolForModelsRequest）。
const modelsPath = "/v1/models"

// anthropicVersionHeader 是 Anthropic 客户端在全部接口上都会带的内置头。
//
// 它是判定「这次请求要哪种清单形状」的唯一依据：清单路径在两种协议下相同，
// 而鉴权头形态（Authorization 与 x-api-key）在真实客户端里混用。
const anthropicVersionHeader = "anthropic-version"

// modelObjectType 是两种形状里单条模型的类型标记取值。
const modelObjectType = "model"

// ownedByNova 是 OpenAI 形状里模型的归属取值。
//
// 固定填网关自己，不用 provider 名：那个名字是内部拓扑（有几条渠道、分别指向哪里），
// 对客户端没有用处，而它会出现在任何能列模型的人手里。
const ownedByNova = "nova"

// unknownModelTime 是上游没给创建时间时写进响应的时间。
//
// 用具名取值表达「未知」而不是省略字段：两种协议的模型对象里这个字段都是必需，
// 省略会让强类型的客户端 SDK 反序列化失败，而一个 1970 年的时间戳至少是可解析的。
const unknownModelTime = 0

// ModelSource 报告一条对外模型来自哪里。
type ModelSource string

const (
	// ModelSourceStatic 表示这条模型由配置里显式声明的 model 行提供。
	ModelSourceStatic ModelSource = "static"
	// ModelSourceDiscovered 表示这条模型来自上游清单。
	ModelSourceDiscovered ModelSource = "discovered"
)

// ModelEntry 是一条对外模型事实。
type ModelEntry struct {
	// Name 是对外名，客户端请求里用它。
	Name string
	// DisplayName 是上游给的展示名；显式声明的模型与未给展示名的发现模型为空。
	DisplayName string
	// CreatedAt 是上游给的创建时间；零值表示上游没给。
	CreatedAt time.Time
	// Source 报告这条模型由显式声明提供还是由上游清单提供。
	Source ModelSource
}

// newModelsHandler 造模型清单的处理器。
//
// 它只读传入的目录快照，不触发发现：目录在装配期已经定好，查询是纯读。
// 目录随装配整体换入，因此 reload 之后的查询自然读到新目录。
func newModelsHandler(models []ModelEntry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "模型清单只接受 GET", http.StatusMethodNotAllowed)
			return
		}
		body, err := renderModels(models, protocolForModelsRequest(r))
		if err != nil {
			http.Error(w, "编码模型清单失败", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

// protocolForModelsRequest 按请求头判定模型清单该用哪种形状。
//
// 带 anthropic-version 的按 Anthropic 形状回答，其余按 OpenAI 形状。不按鉴权头形态判定：
// 两种形态在真实客户端里混用，而这个头是协议自身的必需头，Anthropic 客户端一定会带。
func protocolForModelsRequest(r *http.Request) domain.Protocol {
	if r.Header.Get(anthropicVersionHeader) != "" {
		return domain.ProtocolAnthropicMessages
	}
	return domain.ProtocolOpenAIChat
}

// renderModels 按客户端协议渲染模型清单。
//
// 返回形状与提供这些模型的端点协议无关：一条 Anthropic 形状的端点下发现的模型，
// 同样能以 OpenAI 形状回给 OpenAI 客户端。上游没给的事实不编造，缺省取值只为可反序列化。
func renderModels(entries []ModelEntry, protocol domain.Protocol) ([]byte, error) {
	if protocol == domain.ProtocolAnthropicMessages {
		return renderAnthropicModels(entries)
	}
	return renderOpenAIModels(entries)
}

// openAIModelList 是 OpenAI 形状的清单响应。
type openAIModelList struct {
	Object string        `json:"object"`
	Data   []openAIModel `json:"data"`
}

type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func renderOpenAIModels(entries []ModelEntry) ([]byte, error) {
	list := openAIModelList{
		Object: "list",
		Data:   make([]openAIModel, 0, len(entries)),
	}
	for _, entry := range entries {
		created := int64(unknownModelTime)
		if !entry.CreatedAt.IsZero() {
			created = entry.CreatedAt.Unix()
		}
		list.Data = append(list.Data, openAIModel{
			ID:      entry.Name,
			Object:  modelObjectType,
			Created: created,
			OwnedBy: ownedByNova,
		})
	}
	return json.Marshal(list)
}

// anthropicModelList 是 Anthropic 形状的清单响应。
//
// has_more 恒为 false：网关一次给出全量，不翻页。first_id 与 last_id 在清单为空时省略，
// 空串在这两个字段上没有意义。
type anthropicModelList struct {
	Data    []anthropicModel `json:"data"`
	HasMore bool             `json:"has_more"`
	FirstID string           `json:"first_id,omitempty"`
	LastID  string           `json:"last_id,omitempty"`
}

type anthropicModel struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

func renderAnthropicModels(entries []ModelEntry) ([]byte, error) {
	list := anthropicModelList{
		Data:    make([]anthropicModel, 0, len(entries)),
		HasMore: false,
	}
	for _, entry := range entries {
		displayName := entry.DisplayName
		if displayName == "" {
			// 展示名缺失时退回对外名：这个字段是必需，而对外名总是一个可读的标识。
			displayName = entry.Name
		}
		createdAt := time.Unix(unknownModelTime, 0).UTC().Format(time.RFC3339)
		if !entry.CreatedAt.IsZero() {
			createdAt = entry.CreatedAt.UTC().Format(time.RFC3339)
		}
		list.Data = append(list.Data, anthropicModel{
			Type:        modelObjectType,
			ID:          entry.Name,
			DisplayName: displayName,
			CreatedAt:   createdAt,
		})
	}
	if len(entries) > 0 {
		list.FirstID = entries[0].Name
		list.LastID = entries[len(entries)-1].Name
	}
	return json.Marshal(list)
}

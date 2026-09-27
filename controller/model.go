package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common/client"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/model"
	relay "github.com/pai801/myapi/relay"
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/adaptor/openai"
	"github.com/pai801/myapi/relay/apitype"
	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/meta"
	relaymodel "github.com/pai801/myapi/relay/model"
)

// https://platform.openai.com/docs/api-reference/models/list

type OpenAIModelPermission struct {
	Id                 string  `json:"id"`
	Object             string  `json:"object"`
	Created            int     `json:"created"`
	AllowCreateEngine  bool    `json:"allow_create_engine"`
	AllowSampling      bool    `json:"allow_sampling"`
	AllowLogprobs      bool    `json:"allow_logprobs"`
	AllowSearchIndices bool    `json:"allow_search_indices"`
	AllowView          bool    `json:"allow_view"`
	AllowFineTuning    bool    `json:"allow_fine_tuning"`
	Organization       string  `json:"organization"`
	Group              *string `json:"group"`
	IsBlocking         bool    `json:"is_blocking"`
}

type OpenAIModels struct {
	Id                       string                  `json:"id"`
	Object                   string                  `json:"object"`
	Created                  int                     `json:"created"`
	OwnedBy                  string                  `json:"owned_by"`
	Permission               []OpenAIModelPermission `json:"permission"`
	Root                     string                  `json:"root"`
	Parent                   *string                 `json:"parent"`
	SupportedEndpointTypes   []apitype.EndpointType  `json:"supported_endpoint_types,omitempty"`
	DisplayName              string                  `json:"display_name,omitempty"`
	Visibility               string                  `json:"visibility,omitempty"`
	SupportedInApi           bool                    `json:"supported_in_api,omitempty"`
	Priority                 int                     `json:"priority,omitempty"`
	DefaultReasoningLevel    string                  `json:"default_reasoning_level,omitempty"`
	SupportedReasoningLevels []string                `json:"supported_reasoning_levels,omitempty"`
	ContextWindow            int                     `json:"context_window,omitempty"`
	TruncationPolicy         string                  `json:"truncation_policy,omitempty"`
	InputModalities          []string                `json:"input_modalities,omitempty"`
	ApplyPatchToolType       string                  `json:"apply_patch_tool_type,omitempty"`
	WebSearchToolType        string                  `json:"web_search_tool_type,omitempty"`
}

var (
	models            []OpenAIModels
	modelsMap         map[string]OpenAIModels
	channelId2Models  map[int][]string
	defaultPermission []OpenAIModelPermission

	// catalogMu / catalogBuilt 守卫模型目录的惰性构建（见 ensureModelCatalog）。
	// 用可重置的布尔标记而非 sync.Once：同包测试需要在临时登记/注销扩展渠道后重建目录，
	// sync.Once 无法重置，会让测试无法复原全局状态。
	catalogMu    sync.Mutex
	catalogBuilt bool
)

func init() {
	defaultPermission = append(defaultPermission, OpenAIModelPermission{
		Id:                 "modelperm-LwHkVFn8AcMItP432fKKDIKJ",
		Object:             "model_permission",
		Created:            1626777600,
		AllowCreateEngine:  true,
		AllowSampling:      true,
		AllowLogprobs:      true,
		AllowSearchIndices: false,
		AllowView:          true,
		AllowFineTuning:    false,
		Organization:       "*",
		Group:              nil,
		IsBlocking:         false,
	})
}

// ensureModelCatalog 惰性构建模型目录（models / modelsMap / channelId2Models），只构建一次。
//
// 动机：目录构建要读取 adaptor 注册表，而扩展渠道的 adaptor 与其 apiType 映射由扩展方在
// **各自的 init()** 里经 relay.Register / channeltype.RegisterAPIType 完成。若在 init() 期
// 构建，构建时点相对扩展包 init() 的先后由**包初始化顺序**决定：一旦本包的 init() 先跑，
// 扩展渠道就会在此刻取不到 adaptor（ToAPIType 走不到注册表、GetAdaptor 返回 nil）而被**静默**
// 漏掉——不报错、只是清单为空。改为首次访问时构建后，所有包的 init() 都已完成、注册表处于
// 最终状态，扩展渠道不会再被漏掉。
func ensureModelCatalog() {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if catalogBuilt {
		return
	}
	buildModelCatalog()
	catalogBuilt = true
}

// buildModelCatalog 构建模型目录的全部产物。
//
// 覆盖范围 = 内置枚举 + 注册表登记项：前者保证内置行为逐字不变，后者取自注册表，
// 使扩展方新增的渠道无需本仓改动即被纳入。
func buildModelCatalog() {
	models = nil
	// https://platform.openai.com/docs/models/model-endpoint-compatibility
	for i := 0; i < apitype.Dummy; i++ { // 内置范围
		appendModelsForAPIType(i)
	}
	// 注册表登记项：含内置与扩展，按 apitype.Dummy 过滤后只追加扩展部分，避免与内置重复。
	for _, apiType := range adaptor.RegisteredAPITypes() {
		if apiType < apitype.Dummy {
			continue
		}
		appendModelsForAPIType(apiType)
	}
	for _, channelType := range openai.CompatibleChannels {
		if channelType == channeltype.Azure {
			continue
		}
		channelName, channelModelList := openai.GetCompatibleChannelMeta(channelType)
		for _, modelName := range channelModelList {
			modelObj := createModelObject(modelName, channelName)
			models = append(models, modelObj)
		}
	}
	modelsMap = make(map[string]OpenAIModels)
	for _, m := range models {
		modelsMap[m.Id] = m
	}
	buildChannelId2Models()
}

// appendModelsForAPIType 把 apiType 对应 adaptor 的模型清单追加进 models。
// 跳过 AIProxyLibrary（保持既有枚举语义）与未注册 adaptor（nil 安全跳过）。
func appendModelsForAPIType(apiType int) {
	if apiType == apitype.AIProxyLibrary {
		return
	}
	adp := relay.GetAdaptor(apiType)
	if adp == nil { // 未注册的 apiType（扩展渠道未注册）安全跳过
		return
	}
	channelName := adp.GetChannelName()
	modelNames := adp.GetModelList()
	for _, modelName := range modelNames {
		models = append(models, createModelObject(modelName, channelName))
	}
}

// buildChannelId2Models 构建 channelType -> 模型清单映射。
// 覆盖内置渠道（1 .. Dummy-1）与注册表登记的扩展渠道（内置范围之外），
// 使扩展渠道在渠道编辑的默认模型下拉中可见，而无需本仓登记任何具体扩展渠道。
//
// 该映射**仅用于界面展示与候选**（渠道编辑的默认模型下拉 / DashboardListModels）。
// 它 MUST NOT 被当作出站请求的模型准入白名单：出站路径不得以「模型不在本清单内」为由
// 在本地拒绝合法模型，模型合法性由上游裁决。清单陈旧只影响展示，不得成为拒绝合法模型的理由。
func buildChannelId2Models() {
	channelId2Models = make(map[int][]string)
	for i := 1; i < channeltype.Dummy; i++ { // 内置范围（行为逐字不变）
		fillChannelModels(i)
	}
	for _, ct := range channeltype.RegisteredChannelTypes() { // 内置范围之外的扩展渠道
		// 防御：注册表优先语义下内置类型也可能被登记，避免与上面的内置循环重复填充。
		if ct >= 1 && ct < channeltype.Dummy {
			continue
		}
		fillChannelModels(ct)
	}
}

// fillChannelModels 记录 channelType 对应 adaptor 的模型清单。
// 未注册的 apiType（扩展渠道未注册）其 adaptor 为 nil，直接跳过，
// 避免在 nil 上调用 Init / GetModelList 而 panic（PRD §7.2 决策 D5）。
func fillChannelModels(channelType int) {
	adp := relay.GetAdaptor(channeltype.ToAPIType(channelType))
	if adp == nil { // 未注册的 apiType 安全跳过
		return
	}
	meta := &meta.Meta{
		ChannelType: channelType,
	}
	adp.Init(meta)
	// nil 切片经 encoding/json 序列化为 JSON null（而非 []），而前端把每个渠道的模型
	// 清单当数组用，取到 null 会直接崩溃；故此处统一归为零值空切片。
	list := adp.GetModelList()
	if list == nil {
		list = []string{}
	}
	channelId2Models[channelType] = list
}

func createModelObject(modelName, channelName string) OpenAIModels {
	modelObj := OpenAIModels{
		Id:                     modelName,
		Object:                 "model",
		Created:                1626777600,
		OwnedBy:                channelName,
		Permission:             defaultPermission,
		Root:                   modelName,
		Parent:                 nil,
		SupportedEndpointTypes: model.GetModelEndpointTypes(modelName),
	}
	applyMetadataToModel(&modelObj)
	return modelObj
}

func applyMetadataToModel(modelObj *OpenAIModels) {
	metadata := model.GetOrCreateDefaultMetadata(model.SimplifyModelName(modelObj.Id))
	modelObj.DisplayName = metadata.DisplayName
	modelObj.Visibility = metadata.Visibility
	modelObj.SupportedInApi = metadata.SupportedInApi
	modelObj.Priority = metadata.Priority
	modelObj.DefaultReasoningLevel = metadata.DefaultReasoningLevel
	modelObj.SupportedReasoningLevels = metadata.SupportedReasoningLevels
	modelObj.ContextWindow = metadata.ContextWindow
	modelObj.TruncationPolicy = metadata.TruncationPolicy
	modelObj.InputModalities = metadata.InputModalities
	modelObj.ApplyPatchToolType = metadata.ApplyPatchToolType
	modelObj.WebSearchToolType = metadata.WebSearchToolType
}

func DashboardListModels(c *gin.Context) {
	ensureModelCatalog()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    channelId2Models,
	})
}

func ListAllModels(c *gin.Context) {
	ensureModelCatalog()
	data := make([]OpenAIModels, 0, len(models))
	for _, m := range models {
		m.SupportedEndpointTypes = model.GetModelEndpointTypes(m.Id)
		applyMetadataToModel(&m)
		data = append(data, m)
	}
	c.JSON(200, gin.H{
		"object": "list",
		"data":   data,
	})
}

func ListModels(c *gin.Context) {
	ensureModelCatalog()
	ctx := c.Request.Context()
	var availableModels []string
	if c.GetString(ctxkey.AvailableModels) != "" {
		availableModels = strings.Split(c.GetString(ctxkey.AvailableModels), ",")
	} else {
		// 管理员视角：所有分组模型的并集
		availableModels = getAllGroupsModels(ctx)
	}
	modelSet := make(map[string]bool)
	for _, availableModel := range availableModels {
		modelSet[availableModel] = true
	}
	availableOpenAIModels := make([]OpenAIModels, 0)
	for _, m := range models {
		// 内置清单中同一 id 可能有多条（不同渠道类型），只输出首条，避免 /v1/models 出现重复模型
		if modelSet[m.Id] {
			modelSet[m.Id] = false
			m.SupportedEndpointTypes = model.GetModelEndpointTypes(m.Id)
			applyMetadataToModel(&m)
			availableOpenAIModels = append(availableOpenAIModels, m)
		}
	}
	for modelName, ok := range modelSet {
		if ok {
			modelObj := OpenAIModels{
				Id:                     modelName,
				Object:                 "model",
				Created:                1626777600,
				OwnedBy:                "custom",
				Permission:             defaultPermission,
				Root:                   modelName,
				Parent:                 nil,
				SupportedEndpointTypes: model.GetModelEndpointTypes(modelName),
			}
			applyMetadataToModel(&modelObj)
			availableOpenAIModels = append(availableOpenAIModels, modelObj)
		}
	}
	c.JSON(200, gin.H{
		"object": "list",
		"data":   availableOpenAIModels,
	})
}

func RetrieveModel(c *gin.Context) {
	ensureModelCatalog()
	modelId := c.Param("model")
	if m, ok := modelsMap[modelId]; ok {
		m.SupportedEndpointTypes = model.GetModelEndpointTypes(modelId)
		applyMetadataToModel(&m)
		c.JSON(200, m)
	} else {
		Error := relaymodel.Error{
			Message: fmt.Sprintf("The model '%s' does not exist", modelId),
			Type:    "invalid_request_error",
			Param:   "model",
			Code:    "model_not_found",
		}
		c.JSON(200, gin.H{
			"error": Error,
		})
	}
}

func GetUserAvailableModels(c *gin.Context) {
	ctx := c.Request.Context()
	models := getAllGroupsModels(ctx)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    models,
	})
	return
}

type FetchModelsRequest struct {
	BaseURL     string `json:"base_url"`
	Key         string `json:"key"`
	ChannelID   int    `json:"channel_id"`
	ChannelType int    `json:"channel_type"`
	// Config 为可选的渠道配置（原始 JSON）。向后兼容：既有调用方不传时该字段为 nil，
	// 后端回落到渠道记录中已保存的配置（见 FetchChannelModels 的解析优先级）。
	Config json.RawMessage `json:"config"`
}

type openAIModelListResponse struct {
	Object string            `json:"object"`
	Data   []openAIModelItem `json:"data"`
}

type openAIModelItem struct {
	Id      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// FetchChannelModels 从上游拉取模型列表。
// 编辑渠道时传 channel_id（后端从 DB 取 key）；新增渠道时传 key。
// base_url 优先用请求值，空则回退到 channeltype.ChannelBaseURLs。
//
// 分派：先按 channel_type 解析适配器，若其实现了可选能力 adaptor.ModelLister，
// 则把请求委托给渠道自身的拉取实现（成功以渠道返回值为准，失败直接返回明确错误、
// 不回退）；否则走通用 OpenAI 兼容回退路径：先尝试 {base_url}/v1/models，
// 若返回非 2xx 则回退到 {base_url}/models。base_url 必填校验只约束通用回退分支。
func FetchChannelModels(c *gin.Context) {
	var req FetchModelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "invalid request body: " + err.Error(),
		})
		return
	}

	// —— 分派判断先行于 base_url 必填门（设计 D3）——
	// 先解析渠道适配器并判定其是否实现可选的 ModelLister 能力，再决定是否施加 base_url 必填约束：
	// 实现能力的渠道即使 base_url 为空也 MUST NOT 返回 400（渠道按其声明默认基址回落，与推理路径同口径）。
	apiType := channeltype.ToAPIType(req.ChannelType)
	adp := relay.GetAdaptor(apiType)
	var lister adaptor.ModelLister
	if adp != nil {
		// 仅在 adp 非 nil 时做类型断言：adp == nil（未注册 apiType）安全走通用回退，不 panic。
		lister, _ = adp.(adaptor.ModelLister)
	}

	// —— 确定 key 与 config ——
	apiKey := req.Key
	var channel *model.Channel
	// 需要 channel 的两种情形：取 key（请求未提供 key）或取 config（仅渠道能力分支需要，
	// 且请求未携带 config）。二者共用一次加载，避免重复读库。
	//
	// 关键：通用回退分支（lister == nil）**不**为 config 加载 channel，故其读库行为与改造前
	// 逐字节一致（仅在请求未提供 key 时读一次），既有日志与错误文案均不受影响。
	needKey := apiKey == ""
	needConfig := lister != nil && req.Config == nil
	if req.ChannelID > 0 && (needKey || needConfig) {
		ch, err := model.GetChannelById(req.ChannelID, true) // selectAll=true 包含 key
		if err != nil {
			if needKey {
				// 取 key 失败：保持既有 400 语义（逐字节不变）。
				logger.Log.Errorf("fetch models: failed to get channel %d: %v", req.ChannelID, err)
				c.JSON(http.StatusBadRequest, gin.H{
					"success": false,
					"message": "failed to get channel: " + err.Error(),
				})
				return
			}
			// 仅因取 config 失败：记 warn 并以零值 config 继续，绝不因此让原本能成功的请求失败。
			logger.Log.Warnf("fetch models: failed to get channel %d for config, falling back to empty config: %v", req.ChannelID, err)
		} else {
			channel = ch
			if needKey {
				apiKey = ch.Key
			}
		}
	}

	// —— 确定 base_url（解析后的值，可为空；供 meta 与通用回退共用）——
	baseURL := strings.TrimRight(req.BaseURL, "/")
	if baseURL == "" {
		// 用存在性判断而非 `ChannelType < len(map)`：ChannelBaseURLs 的键由内置渠道与
		// 扩展方按需登记，可能稀疏（如扩展方用键 100），len 不能作为上界，否则扩展渠道
		// 的默认 BaseURL 取不到、静默退化为 400。
		if base, ok := channeltype.ChannelBaseURLs[req.ChannelType]; ok && req.ChannelType > 0 {
			baseURL = strings.TrimRight(base, "/")
		}
	}

	// —— 渠道实现专属拉取能力：委托并直接返回，成功不回退、失败也不回退（设计 D4）——
	if lister != nil {
		m := &meta.Meta{
			ChannelType: req.ChannelType,
			ChannelId:   req.ChannelID,
			BaseURL:     baseURL,
			APIKey:      apiKey,
			Config:      resolveFetchModelsConfig(req.Config, channel),
		}
		m.APIType = apiType
		adp.Init(m)

		ids, err := lister.FetchModels(c.Request.Context(), m)
		if err != nil {
			reason := classifyModelListerError(err)
			channelName := adp.GetChannelName()
			if channelName == "" {
				channelName = fmt.Sprintf("channel type %d", req.ChannelType)
			}
			// 上游原始错误只进服务端日志（截断后），绝不回显到响应文案：原文可能含上游
			// body 或渠道凭证。响应只暴露受控的归类原因与渠道名。
			logger.Log.Errorf("fetch models: channel %d (%s) ModelLister failed (%s): %s",
				req.ChannelID, channelName, reason, truncateBody([]byte(err.Error()), 1024))
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"message": fmt.Sprintf("渠道 %s 拉取模型失败：%s", channelName, reason),
			})
			return
		}
		// 归一责任在渠道侧：controller 只过滤空串，不 trim、不去重、不排序。
		// nil 结果须序列化为 []（而非 null），故预分配空切片。
		data := make([]string, 0, len(ids))
		for _, id := range ids {
			if id != "" {
				data = append(data, id)
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "",
			"data":    data,
		})
		return
	}

	// —— 通用回退：base_url 必填门只在此分支内生效（设计 D3）——
	if baseURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "base_url is required (provide it or select a channel type with a default base URL)",
		})
		return
	}
	var urlsToTry []string
	if strings.HasSuffix(baseURL, "/v1") {
		urlsToTry = []string{
			baseURL + "/models",
		}
	} else {
		urlsToTry = []string{
			baseURL + "/v1/models",
			baseURL + "/models",
		}
	}

	var lastErr error
	var resp *http.Response
	var fetchURL string
	for _, u := range urlsToTry {
		httpReq, err := http.NewRequestWithContext(c.Request.Context(), "GET", u, nil)
		if err != nil {
			logger.Log.Warnf("fetch models: failed to create request for %s: %v", u, err)
			lastErr = err
			continue
		}
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		httpReq.Header.Set("Accept", "application/json")

		resp, err = client.HTTPClient.Do(httpReq)
		if err != nil {
			logger.Log.Warnf("fetch models: request failed for %s: %v", u, err)
			lastErr = err
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			logger.Log.Warnf("fetch models: %s returned status %d: %s", u, resp.StatusCode, string(body))
			lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
			resp = nil // 关键：置 nil 避免后面走到解析已关闭 body 的逻辑
			continue
		}
		fetchURL = u
		break
	}

	if resp == nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": fmt.Sprintf("failed to fetch models: %v", lastErr),
		})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "failed to read response: " + err.Error(),
		})
		return
	}

	var modelListResp openAIModelListResponse
	if err := json.Unmarshal(body, &modelListResp); err != nil {
		logger.Log.Errorf("fetch models: failed to parse response from %s (body length %d): %v\nResponse body: %s",
			fetchURL, len(body), err, truncateBody(body, 1024))
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": fmt.Sprintf("failed to parse response from %s: %v", fetchURL, err),
		})
		return
	}

	modelIDs := make([]string, 0, len(modelListResp.Data))
	for _, m := range modelListResp.Data {
		if m.Id != "" {
			modelIDs = append(modelIDs, m.Id)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    modelIDs,
	})
}

// truncateBody 截断过长的响应体用于日志输出。
func truncateBody(body []byte, maxLen int) string {
	if len(body) <= maxLen {
		return string(body)
	}
	return string(body[:maxLen]) + fmt.Sprintf("... (truncated %d more bytes)", len(body)-maxLen)
}

// resolveFetchModelsConfig 按设计 D5 的优先级解析提供给渠道能力的配置：
//  1. 请求携带 config（json.RawMessage 非 nil）→ 用请求值；
//  2. 否则 channel 已加载 → 用 channel.LoadConfig()；
//  3. 否则零值。
//
// 请求值路径复用 Channel.LoadConfig 的 JSON→ChannelConfig 口径（含 Raw 全量键视图），
// 而**不是** json.Unmarshal(req.Config, &cfg)——后者不会填充 Raw，会与推理路径看到的
// 配置结构分叉。用 (&model.Channel{Config: string(req.Config)}) 承载同一段 JSON，
// 即复用同一份解析逻辑。
//
// 已拍板语义：显式 `config: null`（RawMessage 为字面 "null" 且非 nil）视为「已提供」，
// 按 LoadConfig 对 JSON null 的口径得零值（typed 字段零值、Raw 为 nil），**不回落数据库**。
// 只有字段缺失（RawMessage 为 nil）才回落。
func resolveFetchModelsConfig(raw json.RawMessage, channel *model.Channel) model.ChannelConfig {
	if raw != nil {
		cfg, err := (&model.Channel{Config: string(raw)}).LoadConfig()
		if err != nil {
			// 请求 config 非法 JSON：以零值继续（与 LoadConfig 的降级口径一致），仅记 warn。
			logger.Log.Warnf("fetch models: failed to parse request config, falling back to empty config: %v", err)
		}
		return cfg
	}
	if channel != nil {
		cfg, err := channel.LoadConfig()
		if err != nil {
			// 与推理路径 SetupContextForSelectedChannel 一致：忽略错误、以零值继续。
			logger.Log.Warnf("fetch models: failed to load config for channel %d, falling back to empty config: %v", channel.Id, err)
		}
		return cfg
	}
	return model.ChannelConfig{}
}

// 渠道拉取能力失败时的受控归类文案（MUST NOT 回显 err.Error() 的原始内容，原文可能含
// 上游 body 或渠道凭证）。原文只进服务端日志（见 FetchChannelModels 的 truncateBody 用法）。
const (
	modelListerReasonAuth     = "鉴权失败"
	modelListerReasonNetwork  = "网络错误"
	modelListerReasonUpstream = "上游业务错误"
	modelListerReasonParse    = "解析失败"
	modelListerReasonOther    = "其它错误"
)

func classifyModelListerError(err error) string {
	if err == nil {
		return modelListerReasonOther
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return modelListerReasonNetwork
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return modelListerReasonNetwork
	}
	// 文本 marker 兜底分类：仅用于选文案，不外泄原文。
	text := strings.ToLower(err.Error())
	switch {
	case containsAnyMarker(text, "unauthorized", "forbidden", "invalid api key", "invalid_api_key",
		"authentication", "permission denied", "鉴权", "无权"):
		return modelListerReasonAuth
	case containsAnyMarker(text, "timeout", "timed out", "connection refused", "connection reset",
		"no such host", "network", "eof", "网络", "超时"):
		return modelListerReasonNetwork
	case containsAnyMarker(text, "unmarshal", "parse", "malformed", "invalid character", "解析"):
		return modelListerReasonParse
	case containsAnyMarker(text, "business", "upstream error", "upstream_error", "业务", "上游"):
		return modelListerReasonUpstream
	}
	return modelListerReasonOther
}

// containsAnyMarker 判定 s 是否包含任一 marker。
func containsAnyMarker(s string, markers ...string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// getAllGroupsModels 返回所有分组模型的并集（用于管理员视角的模型可见性）。
func getAllGroupsModels(ctx context.Context) []string {
	groups, err := model.GetAllGroups()
	if err != nil {
		logger.Log.Errorf("GetAllGroups failed: %v", err)
		return nil
	}
	modelSet := make(map[string]bool)
	for _, g := range groups {
		models, err := model.CacheGetGroupModels(ctx, g.Name)
		if err != nil {
			continue
		}
		for _, m := range models {
			modelSet[m] = true
		}
	}
	result := make([]string, 0, len(modelSet))
	for m := range modelSet {
		result = append(result, m)
	}
	return result
}

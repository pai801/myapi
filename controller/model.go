package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
// 先尝试 {base_url}/v1/models，若返回非 2xx 则回退到 {base_url}/models。
func FetchChannelModels(c *gin.Context) {
	var req FetchModelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "invalid request body: " + err.Error(),
		})
		return
	}

	// —— 确定 key ——
	apiKey := req.Key
	if len(apiKey) == 0 && req.ChannelID > 0 {
		channel, err := model.GetChannelById(req.ChannelID, true) // selectAll=true 包含 key
		if err != nil {
			logger.Log.Errorf("fetch models: failed to get channel %d: %v", req.ChannelID, err)
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"message": "failed to get channel: " + err.Error(),
			})
			return
		}
		apiKey = channel.Key
	}

	// —— 确定 base_url ——
	baseURL := strings.TrimRight(req.BaseURL, "/")
	if baseURL == "" {
		// 用存在性判断而非 `ChannelType < len(map)`：ChannelBaseURLs 的键由内置渠道与
		// 扩展方按需登记，可能稀疏（如扩展方用键 100），len 不能作为上界，否则扩展渠道
		// 的默认 BaseURL 取不到、静默退化为 400。
		if base, ok := channeltype.ChannelBaseURLs[req.ChannelType]; ok && req.ChannelType > 0 {
			baseURL = strings.TrimRight(base, "/")
		}
	}
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

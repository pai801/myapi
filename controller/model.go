package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common/client"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/model"
	relay "github.com/pai801/myapi/relay"
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

var models []OpenAIModels
var modelsMap map[string]OpenAIModels
var channelId2Models map[int][]string
var defaultPermission []OpenAIModelPermission

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
	// https://platform.openai.com/docs/models/model-endpoint-compatibility
	for i := 0; i < apitype.Dummy; i++ {
		if i == apitype.AIProxyLibrary {
			continue
		}
		adaptor := relay.GetAdaptor(i)
		channelName := adaptor.GetChannelName()
		modelNames := adaptor.GetModelList()
		for _, modelName := range modelNames {
			modelObj := createModelObject(modelName, channelName)
			models = append(models, modelObj)
		}
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
	for _, model := range models {
		modelsMap[model.Id] = model
	}
	channelId2Models = make(map[int][]string)
	for i := 1; i < channeltype.Dummy; i++ {
		adaptor := relay.GetAdaptor(channeltype.ToAPIType(i))
		meta := &meta.Meta{
			ChannelType: i,
		}
		adaptor.Init(meta)
		channelId2Models[i] = adaptor.GetModelList()
	}
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
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    channelId2Models,
	})
}

func ListAllModels(c *gin.Context) {
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
		if _, ok := modelSet[m.Id]; ok {
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
		if req.ChannelType > 0 && req.ChannelType < len(channeltype.ChannelBaseURLs) {
			baseURL = strings.TrimRight(channeltype.ChannelBaseURLs[req.ChannelType], "/")
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

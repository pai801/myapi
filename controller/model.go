package controller

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/ctxkey"
	"github.com/songquanpeng/one-api/model"
	relay "github.com/songquanpeng/one-api/relay"
	"github.com/songquanpeng/one-api/relay/adaptor/openai"
	"github.com/songquanpeng/one-api/relay/apitype"
	"github.com/songquanpeng/one-api/relay/channeltype"
	"github.com/songquanpeng/one-api/relay/meta"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
	"net/http"
	"strings"
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
		userId := c.GetInt(ctxkey.Id)
		userGroup, _ := model.CacheGetUserGroup(userId)
		availableModels, _ = model.CacheGetGroupModels(ctx, userGroup)
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
	id := c.GetInt(ctxkey.Id)
	userGroup, err := model.CacheGetUserGroup(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	models, err := model.CacheGetGroupModels(ctx, userGroup)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    models,
	})
	return
}

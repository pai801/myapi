package model

import (
	"sync"

	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/relay/apitype"
)

var modelMetadataMap = make(map[string]*ModelMetadata)
var modelMetadataLock sync.RWMutex

type ModelMetadata struct {
	Name                       string                  `json:"name" gorm:"primaryKey"`
	DisplayName                string                  `json:"display_name"`
	Visibility                 string                  `json:"visibility"`
	SupportedInApi             bool                  `json:"supported_in_api"`
	Priority                   int                     `json:"priority"`
	DefaultReasoningLevel      string                  `json:"default_reasoning_level"`
	SupportedReasoningLevels   []string                `json:"supported_reasoning_levels" gorm:"type:text"`
	ContextWindow              int                     `json:"context_window"`
	TruncationPolicy           string                  `json:"truncation_policy"`
	InputModalities            []string                `json:"input_modalities" gorm:"type:text"`
	OutputModalities           []string                `json:"output_modalities" gorm:"type:text"`
	SupportedEndpointTypes     []apitype.EndpointType  `json:"supported_endpoint_types" gorm:"type:text"`
	ApplyPatchToolType        string                  `json:"apply_patch_tool_type"`
	WebSearchToolType          string                  `json:"web_search_tool_type"`
	MaxOutputTokens            int                     `json:"max_output_tokens"`
	CreatedAt                  int64                   `json:"created_at"`
	UpdatedAt                  int64                   `json:"updated_at"`
}

func (ModelMetadata) TableName() string {
	return "model_metadata"
}

func InitModelMetadataMap() {
	modelMetadataLock.Lock()
	defer modelMetadataLock.Unlock()

	modelMetadataMap = make(map[string]*ModelMetadata)

	var metadataList []*ModelMetadata
	if err := DB.Find(&metadataList).Error; err != nil {
		logger.SysError("failed to load model metadata: " + err.Error())
		return
	}

	for _, metadata := range metadataList {
		modelMetadataMap[metadata.Name] = metadata
	}
}

func GetAllModelMetadata() ([]*ModelMetadata, error) {
	var metadataList []*ModelMetadata
	err := DB.Find(&metadataList).Error
	return metadataList, err
}

func GetModelMetadata(name string) (*ModelMetadata, error) {
	var metadata ModelMetadata
	err := DB.First(&metadata, "name = ?", name).Error
	if err != nil {
		return nil, err
	}
	return &metadata, nil
}

func CreateModelMetadata(metadata *ModelMetadata) error {
	now := helper.GetTimestamp()
	metadata.CreatedAt = now
	metadata.UpdatedAt = now
	return DB.Create(metadata).Error
}

func UpdateModelMetadata(metadata *ModelMetadata) error {
	metadata.UpdatedAt = helper.GetTimestamp()
	return DB.Save(metadata).Error
}

func DeleteModelMetadata(name string) error {
	modelMetadataLock.Lock()
	defer modelMetadataLock.Unlock()

	if err := DB.Delete(&ModelMetadata{}, "name = ?", name).Error; err != nil {
		return err
	}

	delete(modelMetadataMap, name)
	return nil
}

func GetModelMetadataBySimplifiedName(simplifiedName string) *ModelMetadata {
	modelMetadataLock.RLock()
	defer modelMetadataLock.RUnlock()

	if metadata, ok := modelMetadataMap[simplifiedName]; ok {
		return metadata
	}
	return nil
}

func GetOrCreateDefaultMetadata(simplifiedName string) *ModelMetadata {
	metadata := GetModelMetadataBySimplifiedName(simplifiedName)
	if metadata != nil {
		return metadata
	}

	return &ModelMetadata{
		Name:                      simplifiedName,
		DisplayName:               simplifiedName,
		Visibility:                "list",
		SupportedInApi:            true,
		Priority:                  999,
		DefaultReasoningLevel:     "medium",
		SupportedReasoningLevels:  []string{"low", "medium", "high"},
		ContextWindow:             128000,
		TruncationPolicy:          "auto",
		InputModalities:           []string{"text"},
		OutputModalities:          []string{"text"},
		SupportedEndpointTypes:    []apitype.EndpointType{apitype.EndpointTypeOpenAI},
	}
}

func RefreshModelMetadataMap() {
	modelMetadataLock.Lock()
	defer modelMetadataLock.Unlock()

	modelMetadataMap = make(map[string]*ModelMetadata)

	var metadataList []*ModelMetadata
	if err := DB.Find(&metadataList).Error; err != nil {
		logger.SysError("failed to refresh model metadata: " + err.Error())
		return
	}

	for _, metadata := range metadataList {
		modelMetadataMap[metadata.Name] = metadata
	}
}

func IsMetadataExists(name string) bool {
	_, err := GetModelMetadata(name)
	return err == nil
}

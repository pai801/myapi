package model

import (
	"sync"

	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/relay/apitype"
)

var modelMetadataMap = make(map[string]*ModelMetadata)

// canonicalAliasMap 记录"成员模型简化名 -> 等价组主名简化名"的归一化映射
var canonicalAliasMap = make(map[string]string)
var modelMetadataLock sync.RWMutex

type ModelMetadata struct {
	Name                       string                  `json:"name" gorm:"primaryKey"`
	CanonicalName              string                  `json:"canonical_name" gorm:"default:''"`
	DisplayName                string                  `json:"display_name"`
	Visibility                 string                  `json:"visibility"`
	SupportedInApi             bool                  `json:"supported_in_api"`
	Priority                   int                     `json:"priority"`
	DefaultReasoningLevel      string                  `json:"default_reasoning_level"`
	SupportedReasoningLevels   []string                `json:"supported_reasoning_levels" gorm:"type:text;serializer:json"`
	ContextWindow              int                     `json:"context_window"`
	TruncationPolicy           string                  `json:"truncation_policy"`
	InputModalities            []string                `json:"input_modalities" gorm:"type:text;serializer:json"`
	OutputModalities           []string                `json:"output_modalities" gorm:"type:text;serializer:json"`
	SupportedEndpointTypes     []apitype.EndpointType  `json:"supported_endpoint_types" gorm:"type:text;serializer:json"`
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
	canonicalAliasMap = make(map[string]string)

	var metadataList []*ModelMetadata
	if err := DB.Find(&metadataList).Error; err != nil {
		logger.Log.Errorf("failed to load model metadata: " + err.Error())
		return
	}

	for _, metadata := range metadataList {
		modelMetadataMap[metadata.Name] = metadata
		// CanonicalName 非空时表示该模型与主模型等价，建立简化名级别的归一化映射
		if metadata.CanonicalName != "" {
			if canonical := SimplifyModelName(metadata.CanonicalName); canonical != "" {
				canonicalAliasMap[SimplifyModelName(metadata.Name)] = canonical
			}
		}
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
	// 与 modelMetadataMap 同步清理，避免已删除的等价配置继续参与路由归并
	delete(canonicalAliasMap, SimplifyModelName(name))
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
	canonicalAliasMap = make(map[string]string)

	var metadataList []*ModelMetadata
	if err := DB.Find(&metadataList).Error; err != nil {
		logger.Log.Errorf("failed to refresh model metadata: " + err.Error())
		return
	}

	for _, metadata := range metadataList {
		modelMetadataMap[metadata.Name] = metadata
		// 与 InitModelMetadataMap 保持一致，同步重建等价组归一化映射
		if metadata.CanonicalName != "" {
			if canonical := SimplifyModelName(metadata.CanonicalName); canonical != "" {
				canonicalAliasMap[SimplifyModelName(metadata.Name)] = canonical
			}
		}
	}
}

// CanonicalizeSimplifiedName 将简化名归一化为等价组主名的简化名；未配置等价关系时原样返回。
func CanonicalizeSimplifiedName(simplifiedName string) string {
	modelMetadataLock.RLock()
	defer modelMetadataLock.RUnlock()

	if canonical, ok := canonicalAliasMap[simplifiedName]; ok {
		return canonical
	}
	return simplifiedName
}

// SetCanonicalAliasForTest 仅供测试预置等价映射，生产代码禁止调用。
func SetCanonicalAliasForTest(simplifiedName, canonicalSimplified string) {
	modelMetadataLock.Lock()
	defer modelMetadataLock.Unlock()
	canonicalAliasMap[simplifiedName] = canonicalSimplified
}

// ResetCanonicalAliasForTest 清空测试预置的等价映射，避免用例间污染。
func ResetCanonicalAliasForTest() {
	modelMetadataLock.Lock()
	defer modelMetadataLock.Unlock()
	canonicalAliasMap = make(map[string]string)
}

func IsMetadataExists(name string) bool {
	_, err := GetModelMetadata(name)
	return err == nil
}

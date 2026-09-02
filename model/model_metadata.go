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

// modelMetadataMapLoaded 标记内存索引是否已经从 DB 完整重建过；
// 与 canonicalAliasMap 是否非空语义不同（部署无等价配置时 alias map 为空但索引仍可视为已加载）。
// 仅读接口据此判断是否需要触发懒加载 Refresh，避免每次都打 DB + 持全局写锁。
var modelMetadataMapLoaded bool

// metadataMapOnce 用于在仅读接口首次访问时一次性触发懒加载 Refresh，避免高并发下
// 多个 goroutine 同时通过 IsCanonicalAliasMapLoaded==false 进入 Refresh 造成双刷。
// 手动 Create/Update/Delete 路径直接调用 RefreshModelMetadataMap 并置位标志位，不依赖 Once。
var metadataMapOnce sync.Once

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

	var metadataList []*ModelMetadata
	if err := DB.Find(&metadataList).Error; err != nil {
		// 启动期 DB 失败时保留旧索引（即便旧索引为空也不破坏），仅记日志交由后续
		// Refresh/EnsureModelMetadataMapLoaded 兜底；与 RefreshModelMetadataMap 失败语义保持一致。
		logger.Log.Errorf("failed to load model metadata: " + err.Error())
		return
	}

	newMetadataMap := make(map[string]*ModelMetadata, len(metadataList))
	newCanonicalAliasMap := make(map[string]string)
	for _, metadata := range metadataList {
		// DB 保留原始 Name（带横线/点号），内存索引统一用简化名以复用 GetModelMetadataBySimplifiedName
		newMetadataMap[SimplifyModelName(metadata.Name)] = metadata
		// CanonicalName 非空时表示该模型与主模型等价，建立简化名级别的归一化映射
		if metadata.CanonicalName != "" {
			if canonical := SimplifyModelName(metadata.CanonicalName); canonical != "" {
				newCanonicalAliasMap[SimplifyModelName(metadata.Name)] = canonical
			}
		}
	}

	// 全量加载成功后再原子替换全局索引与标志位，避免加载失败时清空可用旧索引
	modelMetadataMap = newMetadataMap
	canonicalAliasMap = newCanonicalAliasMap
	modelMetadataMapLoaded = true
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

	delete(modelMetadataMap, SimplifyModelName(name))
	// 与 modelMetadataMap 同步清理，避免已删除的等价配置继续参与路由归并
	delete(canonicalAliasMap, SimplifyModelName(name))
	return nil
}

// ModelMetadataExistsBySimplifiedName 判断 DB 中是否存在原始 Name 经 SimplifyModelName
// 简化后等于 simplifiedName 的记录；用于 CreateMetadata 唯一性兜底（内存 map 未加载/被旁路时）。
// 实现策略：model_metadata 表本身体量很小（用户维护的模型元数据），全量 Find 后在 Go 侧
// 用 SimplifyModelName 做精确比对。原先依赖 LIKE 子序列粗筛的实现在 PostgreSQL 上区分
// 大小写（DB 保留原始 Name），与 MySQL/SQLite 默认 ci collation 不一致，PG 下当 DB 已存
// "DeepSeek-V4-Flash" 时再以 "deepseek-v4-flash" 创建会绕过兜底允许重复，故改为跨方言稳定的全量比对。
// 查询失败时返回 error 以便调用方决定是拒绝创建还是放行，不再吞错。
func ModelMetadataExistsBySimplifiedName(simplifiedName string) (bool, error) {
	if simplifiedName == "" {
		return false, nil
	}
	var metadataList []ModelMetadata
	if err := DB.Find(&metadataList).Error; err != nil {
		logger.Log.Errorf("failed to check model metadata existence by simplified name: " + err.Error())
		return false, err
	}
	for i := range metadataList {
		if SimplifyModelName(metadataList[i].Name) == simplifiedName {
			return true, nil
		}
	}
	return false, nil
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

	var metadataList []*ModelMetadata
	if err := DB.Find(&metadataList).Error; err != nil {
		// DB 失败时保留旧索引与标志位不变：避免热路径上正在用旧索引做路由归并时
		// 因一次 DB 抖动导致全局 map 被清空（错误地暴露"等价归一化失效"而非"DB 抖动"）。
		logger.Log.Errorf("failed to refresh model metadata: " + err.Error())
		return
	}

	newMetadataMap := make(map[string]*ModelMetadata, len(metadataList))
	newCanonicalAliasMap := make(map[string]string)
	for _, metadata := range metadataList {
		// 与 InitModelMetadataMap 保持一致：内存索引 key 用简化名
		newMetadataMap[SimplifyModelName(metadata.Name)] = metadata
		// 与 InitModelMetadataMap 保持一致，同步重建等价组归一化映射
		if metadata.CanonicalName != "" {
			if canonical := SimplifyModelName(metadata.CanonicalName); canonical != "" {
				newCanonicalAliasMap[SimplifyModelName(metadata.Name)] = canonical
			}
		}
	}

	// 全部加载成功后再原子替换全局索引与标志位，DB 失败路径不会修改任何全局状态
	modelMetadataMap = newMetadataMap
	canonicalAliasMap = newCanonicalAliasMap
	modelMetadataMapLoaded = true
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

// IsCanonicalAliasMapLoaded 返回内存索引（modelMetadataMap / canonicalAliasMap）是否已经从
// DB 完整重建过。该标志位由 InitModelMetadataMap / RefreshModelMetadataMap 在 DB 查询与 map
// 重建成功后置 true；查询失败时保持 false。仅读接口据此判断是否需要触发懒加载 Refresh。
// 注意：与"len(canonicalAliasMap) > 0"语义不同——部署无等价配置时 alias map 为空但索引仍可视为已加载。
func IsCanonicalAliasMapLoaded() bool {
	modelMetadataLock.RLock()
	defer modelMetadataLock.RUnlock()
	return modelMetadataMapLoaded
}

// EnsureModelMetadataMapLoaded 保证内存索引至少从 DB 加载过一次；若标志位为 false 则触发
// RefreshModelMetadataMap。配合包级 metadataMapOnce 保证高并发下只刷一次。
//
// 已知边界（可接受）：metadataMapOnce 一旦被消费，若首次 RefreshModelMetadataMap 失败（DB 抖动），
// 标志位保持 false，后续 EnsureModelMetadataMapLoaded 因 Once 已 done 不会再尝试刷新——
// 等价归一化会持续失效，直到下一次手动 Create/Update/Delete 路径直接调用
// RefreshModelMetadataMap（不走 Once）成功恢复。GetModelMetadata/GetAllModelMetadata 等
// 直接打 DB 的接口不受影响，仅依赖内存索引的 CanonicalizeSimplifiedName 路由归并会失效。
// 该退化窗口用户可观测（路由不一致）但不会损坏数据；引入复杂重试机制得不偿失，故保留现状。
// 注意：minor-3 修复后 RefreshModelMetadataMap 在 DB 失败时不会把已为 true 的标志位置回 false，
// 因此正常启动场景下标志位已 true 的用例进 EnsureModelMetadataMapLoaded 走 IsCanonicalAliasMapLoaded
// 短路分支、不会触发 Once.Do，注释与最终实现一致。
func EnsureModelMetadataMapLoaded() {
	if IsCanonicalAliasMapLoaded() {
		return
	}
	metadataMapOnce.Do(func() {
		// 二次检查：可能在 Once 排队等待期间别的 goroutine 已经成功置位
		if !IsCanonicalAliasMapLoaded() {
			RefreshModelMetadataMap()
		}
	})
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

// ResetModelMetadataMapForTest 重置全部包级元数据缓存（索引 + alias + 加载标志 + Once）。
// 仅供测试在用例入口/出口调用，保证用例间互不污染、单独运行也能稳定通过。
func ResetModelMetadataMapForTest() {
	modelMetadataLock.Lock()
	defer modelMetadataLock.Unlock()
	modelMetadataMap = make(map[string]*ModelMetadata)
	canonicalAliasMap = make(map[string]string)
	modelMetadataMapLoaded = false
	metadataMapOnce = sync.Once{}
}

func IsMetadataExists(name string) bool {
	_, err := GetModelMetadata(name)
	return err == nil
}

package controller

import (
	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/model"
	"net/http"
	"strings"
)

func GetAllMetadata(g *gin.Context) {
	// 仅读接口首次访问兜底加载等价归一化映射，避免启动未调用 InitModelMetadataMap 时路由归并失效；
	// 包内 EnsureModelMetadataMapLoaded 用 sync.Once 保证高并发下只刷一次，避免持全局写锁争用路由热路径。
	model.EnsureModelMetadataMapLoaded()
	metadataList, err := model.GetAllModelMetadata()
	if err != nil {
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	g.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    metadataList,
	})
}

func GetMetadata(g *gin.Context) {
	name := g.Param("name")
	// 仅读接口首次访问兜底加载等价归一化映射（Once 保护，避免并发双刷）
	model.EnsureModelMetadataMapLoaded()
	metadata, err := model.GetModelMetadata(name)
	if err != nil {
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	g.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    metadata,
	})
}

func CreateMetadata(g *gin.Context) {
	var metadata model.ModelMetadata
	if err := g.ShouldBindJSON(&metadata); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Invalid JSON format: " + err.Error(),
		})
		return
	}

	if strings.TrimSpace(metadata.Name) == "" {
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Model name cannot be empty",
		})
		return
	}

	// 唯一性按简化名判定，防止 deepseek-v4-flash-0731 与 deepseekv4flash0731 同名重复创建；
	// 先查内存索引（快路径），未命中再查 DB 兜底（覆盖内存未加载/被旁路的并发 Create 场景）。
	simplified := model.SimplifyModelName(metadata.Name)
	if model.GetModelMetadataBySimplifiedName(simplified) != nil {
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Model metadata already exists for name: " + metadata.Name,
		})
		return
	}
	exists, err := model.ModelMetadataExistsBySimplifiedName(simplified)
	if err != nil {
		// 保守拒绝：兜底查询失败时拒绝创建以避免并发场景下绕过唯一性检查产生重复记录；
		// 错误已由 model 层 logger 记录，这里把 error 透传给客户端便于排查（不吞错）。
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Failed to check model metadata existence: " + err.Error(),
		})
		return
	}
	if exists {
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Model metadata already exists for name: " + metadata.Name,
		})
		return
	}

	if err := model.CreateModelMetadata(&metadata); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Failed to create model metadata: " + err.Error(),
		})
		return
	}

	model.RefreshModelMetadataMap()

	g.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Model metadata created successfully",
		"data":    metadata,
	})
}

func UpdateMetadata(g *gin.Context) {
	var metadata model.ModelMetadata
	if err := g.ShouldBindJSON(&metadata); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Invalid JSON format: " + err.Error(),
		})
		return
	}

	if strings.TrimSpace(metadata.Name) == "" {
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Model name cannot be empty",
		})
		return
	}

	existingMetadata, err := model.GetModelMetadata(metadata.Name)
	if err != nil {
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Model metadata does not exist for name: " + metadata.Name,
		})
		return
	}

	metadata.CreatedAt = existingMetadata.CreatedAt

	if err := model.UpdateModelMetadata(&metadata); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Failed to update model metadata: " + err.Error(),
		})
		return
	}

	model.RefreshModelMetadataMap()

	g.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Model metadata updated successfully",
		"data":    metadata,
	})
}

func DeleteMetadata(g *gin.Context) {
	name := g.Param("name")

	if !model.IsMetadataExists(name) {
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Model metadata does not exist for name: " + name,
		})
		return
	}

	if err := model.DeleteModelMetadata(name); err != nil {
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Failed to delete model metadata: " + err.Error(),
		})
		return
	}

	g.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Model metadata deleted successfully",
	})
}

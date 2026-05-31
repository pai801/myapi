package controller

import (
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/model"
	"net/http"
	"strings"
)

func GetAllMetadata(g *gin.Context) {
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
	name = model.SimplifyModelName(name)
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

	metadata.Name = model.SimplifyModelName(metadata.Name)

	if model.IsMetadataExists(metadata.Name) {
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

	metadata.Name = model.SimplifyModelName(metadata.Name)

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
	simplifiedName := model.SimplifyModelName(name)

	if !model.IsMetadataExists(simplifiedName) {
		g.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Model metadata does not exist for name: " + simplifiedName,
		})
		return
	}

	if err := model.DeleteModelMetadata(simplifiedName); err != nil {
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

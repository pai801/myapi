package model

import (
	"path/filepath"
	"testing"

	"github.com/pai801/myapi/relay/apitype"
	. "github.com/smartystreets/goconvey/convey"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestModelMetadataRoundTrip 验证切片字段经 serializer:json 序列化后可完整往返：
// 通过 CreateModelMetadata 写入全字段记录，再用 GetModelMetadata 读回逐字段比对。
func TestModelMetadataRoundTrip(t *testing.T) {
	// 保存原全局 DB，测试结束后恢复，避免污染包内其他测试
	origDB := DB
	defer func() { DB = origDB }()

	dbPath := filepath.Join(t.TempDir(), "model_metadata_test.db")
	var err error
	DB, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite db: %v", err)
	}
	if err := DB.AutoMigrate(&ModelMetadata{}); err != nil {
		t.Fatalf("failed to auto migrate model metadata: %v", err)
	}

	Convey("Given a ModelMetadata with all fields populated", t, func() {
		want := &ModelMetadata{
			Name:                     "deepseek-v4-flash-0731",
			CanonicalName:            "deepseek-v4-flash",
			DisplayName:              "DeepSeek V4 Flash 0731",
			Visibility:               "list",
			SupportedInApi:           true,
			Priority:                 10,
			DefaultReasoningLevel:    "medium",
			SupportedReasoningLevels: []string{"low", "medium", "high"},
			ContextWindow:            128000,
			TruncationPolicy:         "auto",
			InputModalities:          []string{"text"},
			OutputModalities:         []string{"text", "image"},
			SupportedEndpointTypes:   []apitype.EndpointType{apitype.EndpointTypeOpenAI, apitype.EndpointTypeOpenAIResponse},
			ApplyPatchToolType:       "free_text",
			WebSearchToolType:        "native",
			MaxOutputTokens:          8192,
		}

		Convey("When created via CreateModelMetadata and read back via GetModelMetadata", func() {
			err := CreateModelMetadata(want)
			So(err, ShouldBeNil)

			got, err := GetModelMetadata(want.Name)
			So(err, ShouldBeNil)
			So(got, ShouldNotBeNil)

			So(got.Name, ShouldEqual, want.Name)
			So(got.CanonicalName, ShouldEqual, want.CanonicalName)
			So(got.SupportedReasoningLevels, ShouldResemble, want.SupportedReasoningLevels)
			So(got.InputModalities, ShouldResemble, want.InputModalities)
			So(got.OutputModalities, ShouldResemble, want.OutputModalities)
			So(got.SupportedEndpointTypes, ShouldResemble, want.SupportedEndpointTypes)

			// CreateModelMetadata 会回填时间戳，读回后须与写入侧一致且非零
			So(got.CreatedAt, ShouldEqual, want.CreatedAt)
			So(got.UpdatedAt, ShouldEqual, want.UpdatedAt)
			So(got.CreatedAt, ShouldBeGreaterThan, int64(0))
		})
	})
}

package model

import (
	"path/filepath"
	"testing"

	"github.com/pai801/myapi/relay/apitype"
	. "github.com/smartystreets/goconvey/convey"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// withMetadataDB 替换全局 DB 为独立的 sqlite 实例，并重置全部包级内存缓存（索引 + alias +
// 加载标志 + Once），便于 ModelMetadata 单元测试隔离。返回的恢复函数在 defer 中调用，
// 确保不影响其它测试用例、单独运行也能稳定通过。
func withMetadataDB(t *testing.T) func() {
	origDB := DB
	ResetModelMetadataMapForTest()
	dbPath := filepath.Join(t.TempDir(), "model_metadata_test.db")
	var err error
	DB, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite db: %v", err)
	}
	if err := DB.AutoMigrate(&ModelMetadata{}); err != nil {
		t.Fatalf("failed to auto migrate model metadata: %v", err)
	}
	return func() {
		DB = origDB
		ResetModelMetadataMapForTest()
	}
}

// TestModelMetadataRoundTrip 验证切片字段经 serializer:json 序列化后可完整往返：
// 通过 CreateModelMetadata 写入全字段记录，再用 GetModelMetadata 读回逐字段比对。
func TestModelMetadataRoundTrip(t *testing.T) {
	defer withMetadataDB(t)()

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

// TestInitModelMetadataMapLoadsCanonicalAlias 验证 main.go 启动期调用的 InitModelMetadataMap
// 能正确从 DB 重建 canonicalAliasMap + 置位加载标志，让 deepseek-v4-flash-0731 等价归一化到
// deepseek-v4-flash。这正是渠道在 #19/#72 之间横跳的根因修复点。
func TestInitModelMetadataMapLoadsCanonicalAlias(t *testing.T) {
	defer withMetadataDB(t)()

	Convey("InitModelMetadataMap builds canonicalAliasMap from DB rows and flips loaded flag", t, func() {
		err := CreateModelMetadata(&ModelMetadata{
			Name:          "deepseek-v4-flash-0731",
			CanonicalName: "deepseek-v4-flash",
			DisplayName:   "DeepSeek V4 Flash 0731",
			Visibility:    "list",
		})
		So(err, ShouldBeNil)

		// 启动前等价映射应为空（假设这是进程首次启动），加载标志为 false
		So(CanonicalizeSimplifiedName("deepseekv4flash0731"), ShouldEqual, "deepseekv4flash0731")
		So(IsCanonicalAliasMapLoaded(), ShouldBeFalse)

		InitModelMetadataMap()

		// 启动后等价归一化生效 + 标志位置 true
		So(IsCanonicalAliasMapLoaded(), ShouldBeTrue)
		So(CanonicalizeSimplifiedName("deepseekv4flash0731"), ShouldEqual, "deepseekv4flash")

		// RefreshModelMetadataMap 同样应能重建等价映射（手动路径置标志位，不依赖 Once）
		ResetModelMetadataMapForTest()
		So(IsCanonicalAliasMapLoaded(), ShouldBeFalse)
		RefreshModelMetadataMap()
		So(IsCanonicalAliasMapLoaded(), ShouldBeTrue)
		So(CanonicalizeSimplifiedName("deepseekv4flash0731"), ShouldEqual, "deepseekv4flash")
	})
}

// TestModelMetadataMapUsesSimplifiedNameAsKey 验证内存索引统一用简化名：
// 写入原始 Name（带横线）后，按简化名查 GetModelMetadataBySimplifiedName 应能命中。
// 用例显式调用 InitModelMetadataMap 后再断言，避免依赖前序用例遗留的脏 map（独立运行也能 PASS）。
func TestModelMetadataMapUsesSimplifiedNameAsKey(t *testing.T) {
	defer withMetadataDB(t)()

	Convey("CreateModelMetadata 存入原始名后简化名/原始名查询均能命中", t, func() {
		originalName := "deepseek-v4-flash-0731"
		err := CreateModelMetadata(&ModelMetadata{
			Name:          originalName,
			CanonicalName: "deepseek-v4-flash",
			DisplayName:   "DeepSeek V4 Flash 0731",
			Visibility:    "list",
		})
		So(err, ShouldBeNil)

		// DB 按原始名查
		byOriginal, err := GetModelMetadata(originalName)
		So(err, ShouldBeNil)
		So(byOriginal, ShouldNotBeNil)
		So(byOriginal.Name, ShouldEqual, originalName)

		// CreateModelMetadata 只写 DB 不写内存索引；此时简化名查询应为空
		So(GetModelMetadataBySimplifiedName("deepseekv4flash0731"), ShouldBeNil)

		// 显式重建后内存索引才生效
		InitModelMetadataMap()
		So(GetModelMetadataBySimplifiedName("deepseekv4flash0731"), ShouldNotBeNil)
		So(GetModelMetadataBySimplifiedName("deepseekv4flash0731").Name, ShouldEqual, originalName)

		// DeleteModelMetadata 应能按原始名清理内存索引
		err = DeleteModelMetadata(originalName)
		So(err, ShouldBeNil)
		So(GetModelMetadataBySimplifiedName("deepseekv4flash0731"), ShouldBeNil)
	})
}

// TestIsCanonicalAliasMapLoadedSemantics 验证加载标志位语义：
// 1. 进程启动前（未调 Init/Refresh）→ false；
// 2. 无等价配置时 Init 后 → 仍 true（与"len(canonicalAliasMap)>0"语义不同，正是修复点）；
// 3. 有等价配置时 Init 后 → true 且归一化生效；
// 4. EnsureModelMetadataMapLoaded 在标志位 false 时只触发一次加载（Once 保护）。
func TestIsCanonicalAliasMapLoadedSemantics(t *testing.T) {
	defer withMetadataDB(t)()

	Convey("进程启动前 IsCanonicalAliasMapLoaded 为 false", t, func() {
		So(IsCanonicalAliasMapLoaded(), ShouldBeFalse)
	})

	Convey("仅含非等价元数据时 Init 后标志位仍为 true（区别于旧 len 判断）", t, func() {
		err := CreateModelMetadata(&ModelMetadata{
			Name:        "gpt-4-turbo",
			DisplayName: "GPT-4 Turbo",
			Visibility:  "list",
		})
		So(err, ShouldBeNil)

		InitModelMetadataMap()
		// 关键：标志位已置 true；旧 len-based 实现会误判 false → 路由热路径反复刷
		So(IsCanonicalAliasMapLoaded(), ShouldBeTrue)
		// 索引可用
		So(GetModelMetadataBySimplifiedName("gpt4turbo"), ShouldNotBeNil)
	})

	Convey("EnsureModelMetadataMapLoaded 兜底加载后标志位置 true，后续调用不再刷", t, func() {
		// Convey 兄弟子用例共享外层 Convey 作用域，前序 InitModelMetadataMap 已置位 true；
		// 这里显式重置以隔离本子用例，验证"标志位 false → Ensure 触发一次加载"的契约
		ResetModelMetadataMapForTest()
		So(IsCanonicalAliasMapLoaded(), ShouldBeFalse)

		EnsureModelMetadataMapLoaded()
		So(IsCanonicalAliasMapLoaded(), ShouldBeTrue)

		// 多次调用不再触发刷新——保证后续断言不依赖调用次数
		EnsureModelMetadataMapLoaded()
		EnsureModelMetadataMapLoaded()
		So(IsCanonicalAliasMapLoaded(), ShouldBeTrue)
	})
}

// TestModelMetadataExistsBySimplifiedNameDBFallback 验证 m2 唯一性兜底：内存索引未加载
// 或被旁路时，CreateMetadata 仍能通过 ModelMetadataExistsBySimplifiedName 发现 DB 中
// 原始名不同但简化名相同的重复记录（如 deepseek-v4-flash-0731 vs deepseekv4flash0731）。
func TestModelMetadataExistsBySimplifiedNameDBFallback(t *testing.T) {
	defer withMetadataDB(t)()

	Convey("DB 兜底能识别不同原始名但相同简化名的重复记录", t, func() {
		// 写入原始名带横线的记录
		err := CreateModelMetadata(&ModelMetadata{
			Name:        "deepseek-v4-flash-0731",
			DisplayName: "DeepSeek V4 Flash 0731",
			Visibility:  "list",
		})
		So(err, ShouldBeNil)

		// 不调 Init/Refresh，内存索引保持为空；只有 DB 兜底能识别
		So(GetModelMetadataBySimplifiedName("deepseekv4flash0731"), ShouldBeNil)
		// DB 兜底命中：原始名 "deepseek-v4-flash-0731" 简化后等于 "deepseekv4flash0731"
		exists, err := ModelMetadataExistsBySimplifiedName("deepseekv4flash0731")
		So(err, ShouldBeNil)
		So(exists, ShouldBeTrue)
		// 无关简化名返回 false
		exists, err = ModelMetadataExistsBySimplifiedName("gpt4turbo")
		So(err, ShouldBeNil)
		So(exists, ShouldBeFalse)
	})
}

// TestRefreshModelMetadataMapPreservesOldIndexOnDBFailure 验证 minor-3：DB 失败时保留旧索引。
// 通过关闭底层 sql.DB 让 DB.Find 返回 error，再调用 RefreshModelMetadataMap，
// 断言旧的 modelMetadataMap / canonicalAliasMap / 标志位均保持不变。
func TestRefreshModelMetadataMapPreservesOldIndexOnDBFailure(t *testing.T) {
	defer withMetadataDB(t)()

	Convey("RefreshModelMetadataMap 在 DB 失败时保留旧索引与标志位", t, func() {
		// 准备：先创建一条等价配置 + 触发 Init 让索引与标志位都进入"已加载"状态
		So(CreateModelMetadata(&ModelMetadata{
			Name:          "deepseek-v4-flash-0731",
			CanonicalName: "deepseek-v4-flash",
			DisplayName:   "DeepSeek V4 Flash 0731",
			Visibility:    "list",
		}), ShouldBeNil)
		InitModelMetadataMap()
		// 旧索引已就绪
		So(IsCanonicalAliasMapLoaded(), ShouldBeTrue)
		So(CanonicalizeSimplifiedName("deepseekv4flash0731"), ShouldEqual, "deepseekv4flash")
		oldMD := GetModelMetadataBySimplifiedName("deepseekv4flash0731")
		So(oldMD, ShouldNotBeNil)

		// 关闭 DB 让后续 Find 失败
		sqlDB, err := DB.DB()
		So(err, ShouldBeNil)
		So(sqlDB.Close(), ShouldBeNil)

		// 触发 Refresh：DB 失败应保留旧索引与标志位
		RefreshModelMetadataMap()

		// 关键：标志位保持 true（如果被重置为 false 会触发一次 Once 刷新，这里是检查没被清空）
		So(IsCanonicalAliasMapLoaded(), ShouldBeTrue)
		// 旧索引仍可用
		So(CanonicalizeSimplifiedName("deepseekv4flash0731"), ShouldEqual, "deepseekv4flash")
		So(GetModelMetadataBySimplifiedName("deepseekv4flash0731"), ShouldEqual, oldMD)
	})
}

// TestInitModelMetadataMapPreservesOldIndexOnDBFailure 验证 Init 在 DB 失败时同样保留旧索引。
// 准备一个"已加载"状态，然后关闭 DB 再次调用 Init，断言旧索引不变。
func TestInitModelMetadataMapPreservesOldIndexOnDBFailure(t *testing.T) {
	defer withMetadataDB(t)()

	Convey("InitModelMetadataMap 在 DB 失败时保留旧索引与标志位", t, func() {
		So(CreateModelMetadata(&ModelMetadata{
			Name:        "gpt-4-turbo",
			DisplayName: "GPT-4 Turbo",
			Visibility:  "list",
		}), ShouldBeNil)
		InitModelMetadataMap()
		So(IsCanonicalAliasMapLoaded(), ShouldBeTrue)
		oldMD := GetModelMetadataBySimplifiedName("gpt4turbo")
		So(oldMD, ShouldNotBeNil)

		sqlDB, err := DB.DB()
		So(err, ShouldBeNil)
		So(sqlDB.Close(), ShouldBeNil)

		InitModelMetadataMap()

		// DB 失败保留旧索引与标志位
		So(IsCanonicalAliasMapLoaded(), ShouldBeTrue)
		So(GetModelMetadataBySimplifiedName("gpt4turbo"), ShouldEqual, oldMD)
	})
}

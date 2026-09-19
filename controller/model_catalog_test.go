package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/meta"
	relaymodel "github.com/pai801/myapi/relay/model"
)

// TestMain 在本包测试二进制启动时先构建一次模型目录，再跑用例。
//
// 动机：模型目录已改为惰性构建（见 controller/model.go 的 ensureModelCatalog）。包内若干既有
// 测试（如 TestChannelId2ModelsNeverNil）直接读取包级 models / channelId2Models 并断言非空，
// 而它们本身不经过消费入口，若无人先触发构建就会读到零值而误红。此处统一预热一次，
// 让既有测试的语义（「init 后目录即为构建态」）保持不变。
func TestMain(m *testing.M) {
	ensureModelCatalog()
	os.Exit(m.Run())
}

// resetModelCatalog 清除惰性构建标记，使下一次 ensureModelCatalog 强制重建。
// 仅供同包测试在临时改动注册表后重建目录用。
func resetModelCatalog() {
	catalogMu.Lock()
	catalogBuilt = false
	catalogMu.Unlock()
}

// fakeCatalogAdaptor 是验证「扩展渠道被纳入模型清单」用的最小 Adaptor 实现。
// 只有 Init / GetModelList / GetChannelName 有实际语义，其余方法为满足接口的空桩。
type fakeCatalogAdaptor struct {
	models []string
	name   string
}

func (f *fakeCatalogAdaptor) Init(*meta.Meta) {}

func (f *fakeCatalogAdaptor) GetRequestURL(*meta.Meta) (string, error) { return "", nil }

func (f *fakeCatalogAdaptor) SetupRequestHeader(*gin.Context, *http.Request, *meta.Meta) error {
	return nil
}

func (f *fakeCatalogAdaptor) ConvertRequest(*gin.Context, int, *relaymodel.GeneralOpenAIRequest) (any, error) {
	return nil, nil
}

func (f *fakeCatalogAdaptor) ConvertImageRequest(*relaymodel.ImageRequest) (any, error) {
	return nil, nil
}

func (f *fakeCatalogAdaptor) DoRequest(*gin.Context, *meta.Meta, io.Reader) (*http.Response, error) {
	return nil, nil
}

func (f *fakeCatalogAdaptor) DoResponse(*gin.Context, *http.Response, *meta.Meta) (*relaymodel.Usage, *relaymodel.ErrorWithStatusCode) {
	return nil, nil
}

func (f *fakeCatalogAdaptor) GetModelList() []string { return f.models }

func (f *fakeCatalogAdaptor) GetChannelName() string { return f.name }

// TestModelCatalogIncludesRegisteredExtensionChannel 是本任务的守卫：证明「本仓不改一行代码」
// 时，扩展方经 relay.Register + channeltype.RegisterAPIType 登记的渠道会被纳入两处模型清单
// ——channelId2Models（渠道编辑的默认模型下拉）与 models/modelsMap（/v1/models 与按 id 查模型）。
//
// 反向意义：若 controller/model.go 把「扩展渠道循环」或「扩展 apiType 循环」删掉，本测试必红。
func TestModelCatalogIncludesRegisteredExtensionChannel(t *testing.T) {
	const (
		extAPIType     = 199 // 落在内置 apiType 枚举（0..apitype.Dummy-1）之外的临时 apiType
		extChannelType = 999 // 落在内置渠道枚举（1..channeltype.Dummy-1）之外的临时渠道类型
	)
	extModels := []string{"ext-alpha", "ext-beta"}
	const extChannelName = "ext-provider"

	// 走真实注册路径：先登记 adaptor，再登记「渠道类型 → apiType」映射（与扩展方 init() 一致）。
	adaptor.Register(extAPIType, func() adaptor.Adaptor {
		return &fakeCatalogAdaptor{models: extModels, name: extChannelName}
	})
	channeltype.RegisterAPIType(extChannelType, extAPIType)

	// 收尾必须把注册表与目录都还原干净，避免污染同进程内其它测试。
	t.Cleanup(func() {
		channeltype.UnregisterAPIType(extChannelType)
		adaptor.Unregister(extAPIType)
		resetModelCatalog()
		ensureModelCatalog()
	})

	// 模拟真实场景「扩展方注册发生在首次访问之前」：重置惰性标记后触发一次重建。
	resetModelCatalog()
	ensureModelCatalog()

	// 断言①：扩展渠道被纳入 channelId2Models，且清单与其 adaptor 一致（非 nil）。
	got, ok := channelId2Models[extChannelType]
	if !ok {
		t.Fatalf("channelId2Models missing extension channel type %d (扩展渠道被静默漏掉)", extChannelType)
	}
	if !reflect.DeepEqual(got, extModels) {
		t.Errorf("channelId2Models[%d] = %v, want %v", extChannelType, got, extModels)
	}

	// 断言②：扩展渠道的模型被纳入 models/modelsMap（/v1/models 与按 id 查模型的数据源）。
	for _, id := range extModels {
		if _, ok := modelsMap[id]; !ok {
			t.Errorf("modelsMap missing extension model %q", id)
		}
	}

	// 断言③：内置不回归——内置范围（1..Dummy-1）仍有渠道进入清单。
	builtinKeys := 0
	for ct := 1; ct < channeltype.Dummy; ct++ {
		if _, ok := channelId2Models[ct]; ok {
			builtinKeys++
		}
	}
	if builtinKeys == 0 {
		t.Errorf("no built-in channel types present in channelId2Models (内置行为被破坏)")
	}
}

// rebuildCatalog 把模型目录还原到「已构建」的干净状态，供用例收尾复原全局状态。
func rebuildCatalog() {
	resetModelCatalog()
	ensureModelCatalog()
}

// forceEmptyModelCatalog 把模型目录的三处产物清空到「未构建」状态，并复位惰性标记。
//
// 为何不能只调 resetModelCatalog：它仅复位 catalogBuilt，**不清** models / modelsMap /
// channelId2Models 的产物。目录一旦构建过，产物就非 nil，单看产物无法区分「未构建」与
// 「已构建」。要验证「消费入口是否真的会触发惰性构建」，必须把产物也清掉——此后唯一切实
// 的证据就是入口执行后产物被重新填上。
func forceEmptyModelCatalog() {
	resetModelCatalog()
	models = nil
	modelsMap = nil
	channelId2Models = nil
}

// assertCatalogBuilt 断言目录三处产物均非空，即某入口执行后惰性构建确实被触发。
func assertCatalogBuilt(t *testing.T, entry string) {
	t.Helper()
	if len(channelId2Models) == 0 {
		t.Errorf("%s 执行后 channelId2Models 仍为空：该入口未触发惰性构建（ensureModelCatalog 缺失？）", entry)
	}
	if len(models) == 0 {
		t.Errorf("%s 执行后 models 仍为空：该入口未触发惰性构建（ensureModelCatalog 缺失？）", entry)
	}
	if len(modelsMap) == 0 {
		t.Errorf("%s 执行后 modelsMap 仍为空：该入口未触发惰性构建（ensureModelCatalog 缺失？）", entry)
	}
}

// pickBuiltinModelID 在目录已构建的前提下取一个内置模型 id，作为「已知存在」的请求目标，
// 使 RetrieveModel 的用例能区分「目录未建立」与「目录已建立但查无此模型」。
func pickBuiltinModelID(t *testing.T) string {
	t.Helper()
	ensureModelCatalog()
	for id := range modelsMap {
		return id
	}
	t.Fatal("模型目录中没有内置模型，无法选取请求目标")
	return ""
}

// TestEnsureModelCatalogWiring_DashboardListModels 守护 DashboardListModels 入口的
// ensureModelCatalog() 调用：清空目录后仅经该入口触发，目录必须被重新建立。
//
// 反向意义：删掉 DashboardListModels 里的 ensureModelCatalog() 后本用例必红——这正是要
// 消灭的「静默失败」：生产环境首个请求返回空清单而无人报警。
func TestEnsureModelCatalogWiring_DashboardListModels(t *testing.T) {
	forceEmptyModelCatalog()
	t.Cleanup(rebuildCatalog)

	// 起点确为「未构建」，否则测不到 wiring。
	if channelId2Models != nil {
		t.Fatalf("forceEmptyModelCatalog 后 channelId2Models 应被清空，实为 %v", channelId2Models)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/models", nil)
	DashboardListModels(c)

	if w.Code != http.StatusOK {
		t.Errorf("DashboardListModels 状态码 = %d, want %d", w.Code, http.StatusOK)
	}
	assertCatalogBuilt(t, "DashboardListModels")
}

// TestEnsureModelCatalogWiring_ListAllModels 守护 ListAllModels 入口的 ensureModelCatalog() 调用。
//
// 反向意义：删掉 ListAllModels 里的 ensureModelCatalog() 后本用例必红。
func TestEnsureModelCatalogWiring_ListAllModels(t *testing.T) {
	forceEmptyModelCatalog()
	t.Cleanup(rebuildCatalog)

	if models != nil {
		t.Fatalf("forceEmptyModelCatalog 后 models 应被清空，实为 %v", models)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models/all", nil)
	ListAllModels(c)

	if w.Code != http.StatusOK {
		t.Errorf("ListAllModels 状态码 = %d, want %d", w.Code, http.StatusOK)
	}
	var resp listModelsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 ListAllModels 响应失败: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Errorf("ListAllModels 响应 data 为空：目录未建立")
	}
	assertCatalogBuilt(t, "ListAllModels")
}

// TestEnsureModelCatalogWiring_ListModels 守护 ListModels 入口的 ensureModelCatalog() 调用。
//
// 反向意义：删掉 ListModels 里的 ensureModelCatalog() 后本用例必红。
func TestEnsureModelCatalogWiring_ListModels(t *testing.T) {
	forceEmptyModelCatalog()
	t.Cleanup(rebuildCatalog)

	if models != nil {
		t.Fatalf("forceEmptyModelCatalog 后 models 应被清空，实为 %v", models)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	// 覆写可用模型集合，让 ListModels 走内存路径而非 DB 路径。
	c.Set(ctxkey.AvailableModels, "wiring-probe")
	ListModels(c)

	if w.Code != http.StatusOK {
		t.Errorf("ListModels 状态码 = %d, want %d", w.Code, http.StatusOK)
	}
	assertCatalogBuilt(t, "ListModels")
}

// TestEnsureModelCatalogWiring_RetrieveModel 守护 RetrieveModel 入口的 ensureModelCatalog() 调用。
//
// 断言的是 modelsMap 被建立：reset 后先用一个内置已知模型 id 请求，目录被建立时该 id 命中，
// 不会落到 model_not_found 分支。若模型 id 不存在也仍会返回 200，故这里不依赖状态码，而是
// 用「modelsMap 非空」区分「目录未建立」，再用「未命中 model_not_found」区分「已建立但查无」。
//
// 反向意义：删掉 RetrieveModel 里的 ensureModelCatalog() 后本用例必红。
func TestEnsureModelCatalogWiring_RetrieveModel(t *testing.T) {
	knownID := pickBuiltinModelID(t)
	forceEmptyModelCatalog()
	t.Cleanup(rebuildCatalog)

	if modelsMap != nil {
		t.Fatalf("forceEmptyModelCatalog 后 modelsMap 应被清空，实为 %v", modelsMap)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models/"+knownID, nil)
	c.Params = gin.Params{{Key: "model", Value: knownID}}
	RetrieveModel(c)

	assertCatalogBuilt(t, "RetrieveModel")

	// 「目录已建立但查无此模型」与「目录未建立」的区分：内置已知 id 必须命中。
	if body := w.Body.String(); strings.Contains(body, "model_not_found") {
		t.Errorf("内置模型 %q 未命中（返回 model_not_found），目录可能未建立或内容错误；响应体=%s", knownID, body)
	}
}

// TestModelCatalogRebuildIsIdempotent 守护 buildModelCatalog 里的 `models = nil` 重置：
// 重建目录不得让 models 重复追加（长度与 id 多重集必须稳定）。
//
// models 中个别渠道的模型顺序可能因 map 迭代而不确定（如 channel 33），故一律用
// 排序后的 id 多重集比较，不依赖切片顺序。
//
// 反向意义：删掉 buildModelCatalog 里的 `models = nil` 后，二次构建会重复追加，本用例必红。
func TestModelCatalogRebuildIsIdempotent(t *testing.T) {
	t.Cleanup(rebuildCatalog)

	sortedModelIDs := func() []string {
		ids := make([]string, 0, len(models))
		for _, m := range models {
			ids = append(ids, m.Id)
		}
		sort.Strings(ids)
		return ids
	}

	// 首次构建（清掉可能存在的既有产物，以取得一次干净的基准）。
	resetModelCatalog()
	ensureModelCatalog()
	firstIDs := sortedModelIDs()
	firstModelsLen := len(models)
	firstChannelLen := len(channelId2Models)
	firstMapLen := len(modelsMap)

	if firstModelsLen == 0 {
		t.Fatalf("首次构建后 models 为空，无法做幂等断言")
	}

	// 强制重建：resetModelCatalog 只复位标记，再次 ensure 会真正重跑 buildModelCatalog。
	resetModelCatalog()
	ensureModelCatalog()

	if len(models) != firstModelsLen {
		t.Errorf("重建后 models 长度膨胀: %d -> %d（buildModelCatalog 缺少 models = nil 重置）", firstModelsLen, len(models))
	}
	if secondIDs := sortedModelIDs(); !reflect.DeepEqual(firstIDs, secondIDs) {
		t.Errorf("重建后 models 的 id 多重集发生变化: len=%d -> %d", len(firstIDs), len(secondIDs))
	}
	if len(channelId2Models) != firstChannelLen {
		t.Errorf("重建后 channelId2Models 键数变化: %d -> %d", firstChannelLen, len(channelId2Models))
	}
	if len(modelsMap) != firstMapLen {
		t.Errorf("重建后 modelsMap 键数变化: %d -> %d", firstMapLen, len(modelsMap))
	}
}

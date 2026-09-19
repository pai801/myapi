package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/client"
	"github.com/pai801/myapi/common/ctxkey"
	relay "github.com/pai801/myapi/relay"
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/apitype"
	"github.com/pai801/myapi/relay/channeltype"
	. "github.com/smartystreets/goconvey/convey"
)

// listModelsResponse 对应 ListModels 的响应体结构。
type listModelsResponse struct {
	Object string         `json:"object"`
	Data   []OpenAIModels `json:"data"`
}

func TestListModelsDeduplication(t *testing.T) {
	Convey("ListModels 对内置清单中的重复模型 id 只输出首条", t, func() {
		// 覆写包级内置模型清单：同一 id 三条不同 owned_by + 一条不在可用集合里的模型
		originalModels := models
		models = []OpenAIModels{
			{Id: "gpt-5.5", Object: "model", OwnedBy: "openai"},
			{Id: "gpt-5.5", Object: "model", OwnedBy: "codex"},
			{Id: "gpt-5.5", Object: "model", OwnedBy: "chatgpt-sub"},
			{Id: "gpt-5.5-unavailable", Object: "model", OwnedBy: "openai"},
		}
		defer func() { models = originalModels }()

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		// 设置 AvailableModels 后 ListModels 走内存路径，不触发 DB
		c.Set(ctxkey.AvailableModels, "gpt-5.5")

		ListModels(c)

		So(w.Code, ShouldEqual, http.StatusOK)
		var resp listModelsResponse
		So(json.Unmarshal(w.Body.Bytes(), &resp), ShouldBeNil)

		count := 0
		var matched *OpenAIModels
		for i := range resp.Data {
			if resp.Data[i].Id == "gpt-5.5" {
				count++
				matched = &resp.Data[i]
			}
		}
		// 同一 id 只出现一次，owned_by 取首条
		So(count, ShouldEqual, 1)
		So(matched.OwnedBy, ShouldEqual, "openai")
		// 元数据字段被套用（默认元数据会回填 display_name）
		So(matched.DisplayName, ShouldNotBeEmpty)
		// 不在可用集合里的内置模型不输出
		for _, m := range resp.Data {
			So(m.Id, ShouldNotEqual, "gpt-5.5-unavailable")
		}
	})
}

// TestChannelId2ModelsNeverNil 锁定 GET /api/models 的契约：channelId2Models 的每个值都
// 必须是数组（空清单也是 []），绝不能为 nil —— nil 切片经 encoding/json 序列化成 null，
// 前端 getChannelModels 会把它当数组用（.length / .includes）而崩溃。
//
// 该断言在 controller/model.go 归零逻辑缺失时必须是「红」的，否则说明它没守住契约。
func TestChannelId2ModelsNeverNil(t *testing.T) {
	Convey("channelId2Models 的所有值都非 nil", t, func() {
		So(len(channelId2Models), ShouldBeGreaterThan, 0)
		for _, list := range channelId2Models {
			So(list, ShouldNotBeNil)
		}
	})

	Convey("序列化后不出现 \"NN\": null", t, func() {
		blob, err := json.Marshal(channelId2Models)
		So(err, ShouldBeNil)
		So(strings.Contains(string(blob), ":null"), ShouldBeFalse)
	})
}

// TestBuildChannelId2ModelsSkipsNilAdaptor 证明 buildChannelId2Models 对「查不到 adaptor」
// 的 apiType 会安全跳过，而不是在 nil 上调用 Init / GetModelList 崩溃（PRD §7.2 决策 D5）。
//
// 手法：用 adaptor.MustRegister 把某个真实 apiType 的 factory 覆盖成返回 nil（等价于「未注册」），
// 再触发一次构建。若 model.go 里的 nil 守卫被移除，本测试会因 nil panic 变红；守卫在则绿。
func TestBuildChannelId2ModelsSkipsNilAdaptor(t *testing.T) {
	Convey("未注册的 apiType 在构建 channelId2Models 时被安全跳过", t, func() {
		// 保存并还原全局 map，避免污染同包其它测试。
		originalMap := channelId2Models
		defer func() { channelId2Models = originalMap }()

		// 捕获真实 adaptor 实例，defer 时还原 factory，避免污染同包其它测试。
		real := relay.GetAdaptor(apitype.OpenAI)
		defer adaptor.MustRegister(apitype.OpenAI, func() adaptor.Adaptor { return real })

		// 覆盖为返回 nil，模拟该 apiType 未注册（如某扩展渠道的 adaptor 尚未就绪）。
		adaptor.MustRegister(apitype.OpenAI, func() adaptor.Adaptor { return nil })

		// 守卫缺失时，此处会在 adaptor.Init(meta) 上 nil panic。
		buildChannelId2Models()

		// 所有映射到 OpenAI 的渠道类型都应被跳过，不出现在 map 中。
		skipped := 0
		for channelType := 1; channelType < channeltype.Dummy; channelType++ {
			if channeltype.ToAPIType(channelType) != apitype.OpenAI {
				continue
			}
			skipped++
			_, ok := channelId2Models[channelType]
			So(ok, ShouldBeFalse)
		}
		So(skipped, ShouldBeGreaterThan, 0)
	})
}

// TestFetchChannelModelsSparseBaseURLKey 守护扩展渠道默认 BaseURL 的解析（回归）：
// ChannelBaseURLs 是 map，扩展方可用稀疏键（远大于内置渠道数）。解析必须用存在性判断，
// 而非 `ChannelType < len(map)`——后者对稀疏键恒为假，会静默退化为 400。
func TestFetchChannelModelsSparseBaseURLKey(t *testing.T) {
	Convey("稀疏 ChannelType 也能取到默认 BaseURL 并成功拉取模型", t, func() {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"ext-model","object":"model"}]}`))
		}))
		defer upstream.Close()

		const sparseType = 4242 // 远大于内置渠道类型数，模拟扩展方登记的稀疏键
		channeltype.ChannelBaseURLs[sparseType] = upstream.URL
		defer delete(channeltype.ChannelBaseURLs, sparseType)

		origClient := client.HTTPClient
		client.HTTPClient = &http.Client{}
		defer func() { client.HTTPClient = origClient }()

		body, _ := json.Marshal(FetchModelsRequest{ChannelType: sparseType, Key: "sk-test"})
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/channel/fetch_models", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")

		FetchChannelModels(c)

		So(w.Code, ShouldEqual, http.StatusOK)
		var resp struct {
			Success bool     `json:"success"`
			Data    []string `json:"data"`
		}
		So(json.Unmarshal(w.Body.Bytes(), &resp), ShouldBeNil)
		So(resp.Success, ShouldBeTrue)
		So(resp.Data, ShouldContain, "ext-model")
	})
}

package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/ctxkey"
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

package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestGetChannelDescriptorsEmptyByDefault 锁定「渠道能力清单」端点的默认行为
// （PRD §5.12 层1 / 决策 D7）：主仓不注册任何渠道，故端点必须返回 200 + 空数组。
func TestGetChannelDescriptorsEmptyByDefault(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/descriptors", nil)
	GetChannelDescriptors(c)

	if w.Code != http.StatusOK {
		t.Fatalf("GetChannelDescriptors code = %d, want 200", w.Code)
	}
	var resp struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, w.Body.String())
	}
	if !resp.Success {
		t.Fatalf("success = false, body=%s", w.Body.String())
	}
	// data 必须是 []（非 null），便于前端统一按数组处理。
	if string(resp.Data) != "[]" {
		t.Errorf("data = %s, want [] (no channels registered)", resp.Data)
	}
}

package ctxkey

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestGetRequestBodyMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	concrete := RequestBodyMetadata{
		WellFormed:  true,
		Model:       "gpt-4o",
		ModelValid:  true,
		Stream:      true,
		StreamValid: true,
	}

	tests := []struct {
		name    string
		prepare func(t *testing.T) *gin.Context
		want    RequestBodyMetadata
		wantOk  bool
	}{
		{
			name: "concrete value hit preserves every field",
			prepare: func(t *testing.T) *gin.Context {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Set(KeyRequestBodyMetadata, concrete)
				return c
			},
			want:   concrete,
			wantOk: true,
		},
		{
			name: "cached malformed conclusion is authoritative (ok=true, zero fields)",
			prepare: func(t *testing.T) *gin.Context {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				// 缓存携带 WellFormed=false：读取必须返回该具体值且 ok=true（结论权威），
				// 调用方据此不再自解析 body（AC-1 / 缓存缺失即自解析）。
				c.Set(KeyRequestBodyMetadata, RequestBodyMetadata{WellFormed: false})
				return c
			},
			want:   RequestBodyMetadata{WellFormed: false},
			wantOk: true,
		},
		{
			name: "missing key returns not ok",
			prepare: func(t *testing.T) *gin.Context {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				return c
			},
			want:   RequestBodyMetadata{},
			wantOk: false,
		},
		{
			name: "string value returns not ok",
			prepare: func(t *testing.T) *gin.Context {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Set(KeyRequestBodyMetadata, "not-a-metadata")
				return c
			},
			want:   RequestBodyMetadata{},
			wantOk: false,
		},
		{
			name: "pointer value returns not ok",
			prepare: func(t *testing.T) *gin.Context {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				pointer := concrete
				c.Set(KeyRequestBodyMetadata, &pointer)
				return c
			},
			want:   RequestBodyMetadata{},
			wantOk: false,
		},
		{
			name: "nil context returns not ok without panic",
			prepare: func(t *testing.T) *gin.Context {
				return nil
			},
			want:   RequestBodyMetadata{},
			wantOk: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.prepare(t)
			got, ok := GetRequestBodyMetadata(c)
			if ok != tt.wantOk {
				t.Fatalf("GetRequestBodyMetadata() ok = %v, want %v", ok, tt.wantOk)
			}
			if got != tt.want {
				t.Fatalf("GetRequestBodyMetadata() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

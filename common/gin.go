package common

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/ctxkey"
)

func GetRequestBody(c *gin.Context) ([]byte, error) {
	requestBody, _ := c.Get(ctxkey.KeyRequestBody)
	if requestBody != nil {
		return requestBody.([]byte), nil
	}
	requestBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	_ = c.Request.Body.Close()
	c.Set(ctxkey.KeyRequestBody, requestBody)
	return requestBody.([]byte), nil
}

func UnmarshalBodyReusable(c *gin.Context, v any) error {
	requestBody, err := GetRequestBody(c)
	if err != nil {
		return err
	}
	contentType := c.Request.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		err = json.Unmarshal(requestBody, &v)
	} else {
		c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
		err = c.ShouldBind(&v)
	}
	if err != nil {
		return err
	}
	// Reset request body
	c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
	return nil
}

// GetRequestBodyReusable 读取请求 body 后**恢复** c.Request.Body，使下游仍可完整读取。
//
// 与 GetRequestBody 的区别：GetRequestBody 读后不恢复 c.Request.Body（消费即弃），
// 因此只能在「已经不需要 body 可再读」的场景使用；本函数在读取后把 body 重新写回
// c.Request.Body，可在转发前的中间件里安全调用 —— 否则 relay/controller 下直接读
// c.Request.Body 的路由（proxy.go / audio.go / text.go / image.go）会拿到空 body。
//
// 语义：
//   - c.Request.Body == nil（合成上下文 / 测试）→ 返回 (nil, nil)，不 panic；
//   - ctxkey.KeyRequestBody 已缓存 → 直接返回缓存（不重复读），并按缓存重建 c.Request.Body
//     （缓存可能来自不恢复 body 的 GetRequestBody，此时必须重建才能保证下游读到完整 body）；
//   - 否则全量 io.ReadAll → 写入缓存 → 重建 c.Request.Body；
//   - 读取出错 → 返回错误且**不恢复** c.Request.Body、不写缓存：下游若继续读会读到剩余字节或
//     直接失败，而不会读到被伪装成完整 body 的截断内容。调用方自行静默降级。
//
// 读取出错的取舍（刻意如此）：io.ReadAll 出错时返回的是**已读到的部分字节**。若把它重新包成
// 可读的 NopCloser，下游（GetRequestBody / io.Copy / c.ShouldBind）会「成功」读到一段被截断的
// body 并转发给上游，把显式读错误静默降级成数据损坏的请求。因此宁可让下游读失败/读到剩余字节，
// 也绝不把截断内容当成完整 body 转发；同时不写缓存，避免损坏内容被后续路径当完整 body 复用。
//
// 刻意不做 size cap（不用 io.LimitReader）：LimitReader 会把**截断后**的 body 交给下游，
// 属于数据损坏事故；而 cap 本身也无意义 —— 下游无论如何都会全量读 body
// （GetRequestBody 或 io.Copy），提前全量读 + 缓存 + 恢复的内存开销与现状等价，甚至更省
// （下游可复用缓存）。故与 UnmarshalBodyReusable 的做法一致，直接全量读 + 缓存 + 恢复。
func GetRequestBodyReusable(c *gin.Context) ([]byte, error) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return nil, nil
	}
	if cached, ok := c.Get(ctxkey.KeyRequestBody); ok {
		if body, ok := cached.([]byte); ok {
			c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
			return body, nil
		}
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		// 刻意不恢复 c.Request.Body、不写缓存：io.ReadAll 已消费部分字节，重建可读 reader 只会把
		// 截断内容伪装成完整 body 交给下游（详见函数头注释的取舍说明）。
		return nil, err
	}
	_ = c.Request.Body.Close()
	c.Set(ctxkey.KeyRequestBody, body)
	c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
	return body, nil
}

func SetEventStreamHeaders(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("Transfer-Encoding", "chunked")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
}

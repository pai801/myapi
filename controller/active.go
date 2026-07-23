package controller

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/relay/active"
)

// HandleSSEOptions 处理 SSE 连接前的 OPTIONS preflight（CORS，不要求认证）
func HandleSSEOptions(c *gin.Context) {
	c.Writer.Header().Set("Access-Control-Allow-Origin", c.Request.Header.Get("Origin"))
	c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
	c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	c.Writer.WriteHeader(http.StatusNoContent)
}

// GetAllActiveLogs 获取当前所有活跃（在转发中）的请求
func GetAllActiveLogs(c *gin.Context) {
	activeLogs := active.Global.List()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    activeLogs,
	})
}

// StreamActiveLogs SSE 端点，实时推送活跃请求的增删改事件
func StreamActiveLogs(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("Transfer-Encoding", "chunked")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Header().Set("Access-Control-Allow-Origin", c.Request.Header.Get("Origin"))
	c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
	c.Writer.WriteHeader(http.StatusOK)

	sub := active.Subscribe()
	defer active.Unsubscribe(sub)

	// 推送当前所有活跃请求作为初始快照
	list := active.Global.List()
	for i := range list {
		evt := active.RequestEvent{Type: "start", Data: &list[i]}
		writeSSEEvent(c.Writer, "start", evt)
	}

	// 阻塞读取事件，推送到客户端
	for {
		select {
		case evt := <-sub:
			writeSSEEvent(c.Writer, evt.Type, evt)
		case <-c.Request.Context().Done():
			return
		}
	}
}

// writeSSEEvent 写入一个标准 SSE 事件帧
func writeSSEEvent(w gin.ResponseWriter, event string, data any) {
	dataJSON, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(dataJSON))
	w.Flush()
}

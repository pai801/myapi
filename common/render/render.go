package render

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
)

func StringData(c *gin.Context, str string, event ...string) {
	str = strings.TrimPrefix(str, "data: ")
	str = strings.TrimSuffix(str, "\r")
	ev := ""
	if len(event) > 0 {
		ev = event[0]
	}
	c.Render(-1, common.CustomEvent{Event: ev, Data: "data: " + str})
	c.Writer.Flush()
}

func EventData(c *gin.Context, eventName string, data string) {
	data = strings.TrimPrefix(data, "data: ")
	data = strings.TrimSuffix(data, "\r")
	c.Render(-1, common.CustomEvent{Event: eventName, Data: "data: " + data})
	c.Writer.Flush()
}

func ObjectData(c *gin.Context, object interface{}) error {
	jsonData, err := json.Marshal(object)
	if err != nil {
		return fmt.Errorf("error marshalling object: %w", err)
	}
	StringData(c, string(jsonData))
	return nil
}

func Done(c *gin.Context) {
	StringData(c, "[DONE]")
}

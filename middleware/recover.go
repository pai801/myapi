package middleware

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/logger"
	"net/http"
	"runtime/debug"
)

func RelayPanicRecover() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				logger.Log.Errorf("panic detected: %v", err)
				logger.Log.Errorf("stacktrace from panic: %s", string(debug.Stack()))
				logger.Log.Errorf("request: %s %s", c.Request.Method, c.Request.URL.Path)
				body, _ := common.GetRequestBody(c)
				logger.Log.Errorf("request body: %s", string(body))
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{
						"message": fmt.Sprintf("Panic detected, error: %v. Please submit an issue with the related log here: https://github.com/pai801/myapi", err),
						"type":    "one_api_panic",
					},
				})
				c.Abort()
			}
		}()
		c.Next()
	}
}

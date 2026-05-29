package controller

import (
	"encoding/json"
	"net/http"
	"strings"
)

func MaskAuthorizationHeader(headers http.Header) string {
	result := make(map[string]string)
	for key, values := range headers {
		if strings.EqualFold(key, "Authorization") {
			result[key] = "Bearer ***"
		} else {
			result[key] = strings.Join(values, ", ")
		}
	}
	jsonBytes, _ := json.Marshal(result)
	return string(jsonBytes)
}

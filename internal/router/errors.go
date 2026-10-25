package router

import (
	"encoding/json"
	"net/http"
)

type apiError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

// writeError writes an error in the OpenAI API error format.
func writeError(writer http.ResponseWriter, status int, message, errorType, code string) {
	header := writer.Header()
	header.Del("Content-Length")
	header.Set("Content-Type", "application/json")
	header.Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]apiError{"error": {Message: message, Type: errorType, Code: code}})
}

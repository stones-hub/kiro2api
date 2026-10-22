package server

import (
	"github.com/gin-gonic/gin"
	"kiro2api/config"
	"kiro2api/types"
	"net/http"
)

func handleModels(c *gin.Context) {
	entries := config.SupportedModels()
	models := make([]types.Model, 0, len(entries))
	for _, entry := range entries {
		models = append(models, types.Model{
			ID: entry.ID, Object: "model", Created: 0,
			OwnedBy: entry.Owner(), DisplayName: entry.Name, Type: "text",
			MaxTokens: entry.TokenLimits.Input, MaxOutputTokens: entry.TokenLimits.Output,
			ThinkingMode: entry.Thinking.Mode, ReasoningEfforts: entry.Thinking.Efforts,
		})
	}
	c.JSON(http.StatusOK, types.ModelsResponse{Object: "list", Data: models})
}

// Reject unknown models before a streaming handler commits HTTP 200 headers.
func validateRequestedModel(c *gin.Context, name string) bool {
	if _, ok := config.ResolveModel(name); ok {
		return true
	}
	c.AbortWithStatusJSON(http.StatusBadRequest, types.NewModelNotFoundError(name, GetRequestID(c)))
	return false
}

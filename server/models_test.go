package server

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"kiro2api/config"
	"kiro2api/types"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelsEndpointMatchesRoutingCatalogAndRequiresAuthentication(t *testing.T) {
	router := gin.New()
	router.Use(PathBasedAuthMiddleware("test-models-key", []string{"/v1"}))
	router.GET("/v1/models", handleModels)
	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthorized.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer test-models-key")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var models types.ModelsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	if len(models.Data) != len(config.SupportedModels()) {
		t.Fatal("advertised models do not match the routing catalog")
	}
	previous := ""
	for _, item := range models.Data {
		mapped, ok := config.ResolveModel(item.ID)
		if !ok || item.MaxTokens != mapped.TokenLimits.Input || item.MaxOutputTokens != mapped.TokenLimits.Output || item.OwnedBy != mapped.Owner() {
			t.Fatalf("incorrect advertised model: %+v", item)
		}
		if item.ID <= previous {
			t.Fatal("model list is not deterministic or contains duplicates")
		}
		previous = item.ID
	}
}

func TestUnknownModelIsRejectedBeforeStreamHeaders(t *testing.T) {
	router := gin.New()
	router.POST("/stream", func(c *gin.Context) {
		if !validateRequestedModel(c, "claude-3-5-haiku-20241022") {
			return
		}
		_ = initializeSSEResponse(c)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/stream", nil))
	if response.Code != http.StatusBadRequest || response.Flushed {
		t.Fatalf("unknown model committed a stream: status=%d flushed=%v", response.Code, response.Flushed)
	}
	var body types.ModelNotFoundError
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "model_not_found" {
		t.Fatalf("unexpected error: %+v", body)
	}
}

func TestCurrentKiroEndpointReceivesCanonicalModelAndProfile(t *testing.T) {
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	profile := "arn:aws:codewhisperer:us-east-1:123456789012:profile/test"
	request, err := buildCodeWhispererRequest(c, types.AnthropicRequest{
		Model: "claude-haiku-4-5-20251001", MaxTokens: 16,
		Messages: []types.AnthropicRequestMessage{{Role: "user", Content: "hello"}},
	}, types.TokenInfo{AccessToken: "test-access-token", ProfileArn: profile}, true)
	if err != nil {
		t.Fatal(err)
	}
	if request.URL.Host != "q.us-east-1.amazonaws.com" {
		t.Fatalf("incorrect endpoint: %s", request.URL.Host)
	}
	if request.Header.Get("x-amzn-kiro-agent-mode") != "vibe" {
		t.Fatal("explicit model selection must use Kiro vibe mode")
	}
	if request.Header.Get("x-amz-user-agent") != "aws-sdk-js/1.0.34 KiroIDE-0.11.107" {
		t.Fatal("generation request used the legacy Kiro 0.2 client protocol")
	}
	var payload types.CodeWhispererRequest
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.ProfileArn != profile || payload.ConversationState.CurrentMessage.UserInputMessage.ModelId != "claude-haiku-4.5" {
		t.Fatal("upstream request lost its account profile or canonical model")
	}
	if request.Header.Get("Accept") != "*/*" || payload.InferenceConfig == nil || payload.InferenceConfig.MaxTokens != 16 {
		t.Fatal("upstream request lost its binary stream negotiation or requested output limit")
	}
}

package routes

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/managementmcp"
	ratelimit "github.com/Wei-Shaw/sub2api/internal/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// RegisterMCPRoutes exposes MCP over HTTP and reuses the normal management
// routes in a private router, preserving ownership checks, guards and audit.
func RegisterMCPRoutes(v1 *gin.RouterGroup, h *handler.Handlers, audit middleware.AuditLogMiddleware,
	stepUp middleware.StepUpAuthMiddleware, settings *service.SettingService,
	limiter *middleware.PanelRateLimiter, allowedOrigins []string,
) {
	inner := gin.New()
	inner.Use(gin.Recovery())
	api := inner.Group("/api/v1")
	RegisterUserRoutes(api, h, middleware.JWTAuthMiddleware(managementmcp.DispatchAuth(false)), audit, settings, limiter)
	RegisterAdminRoutes(api, h, middleware.AdminAuthMiddleware(managementmcp.DispatchAuth(true)), audit, stepUp, settings, limiter)
	transport := managementmcp.NewHandler(inner)
	mcp := v1.Group("/mcp", managementmcp.Authenticate(h.MCPOAuth.Service(), allowedOrigins, h.MCPOAuth.ResourceURL), limiter.MCPTransport(), middleware.BackendModeUserGuard(settings))
	for _, method := range []string{"POST", "GET", "DELETE"} {
		mcp.Handle(method, "", gin.WrapH(transport))
	}
}

// RegisterMCPOAuthRoutes exposes OAuth discovery and public-client protocol
// endpoints. Browser consent and grant management live in the JWT user router.
func RegisterMCPOAuthRoutes(r *gin.Engine, v1 *gin.RouterGroup, h *handler.Handlers, redisClient *redis.Client) {
	r.GET("/.well-known/oauth-protected-resource/api/v1/mcp", h.MCPOAuth.ResourceMetadata)
	r.GET("/.well-known/oauth-protected-resource", h.MCPOAuth.ResourceMetadata)
	r.GET("/.well-known/oauth-authorization-server", h.MCPOAuth.AuthorizationMetadata)
	limit := ratelimit.NewRateLimiter(redisClient)
	closed := ratelimit.RateLimitOptions{FailureMode: ratelimit.RateLimitFailClose}
	oauth := v1.Group("/mcp/oauth")
	oauth.POST("/register", limit.LimitWithOptions("mcp-oauth-register", 10, time.Minute, closed), h.MCPOAuth.Register)
	oauth.POST("/token", limit.LimitWithOptions("mcp-oauth-token", 60, time.Minute, closed), h.MCPOAuth.Token)
	oauth.POST("/revoke", limit.LimitWithOptions("mcp-oauth-revoke", 60, time.Minute, closed), h.MCPOAuth.RevokeToken)
}

package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *ChannelMonitorUserHandler) ProbeOriginMiddleware() gin.HandlerFunc {
	var monitor *service.ChannelMonitorService
	if h != nil {
		monitor = h.monitorService
	}
	return monitor.ProbeOriginMiddleware()
}

package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type prismAccountService interface {
	Create(context.Context, int64, string, []int64) (*service.Account, *service.PrismStatus, error)
	Status(context.Context, int64) (*service.PrismStatus, error)
	Reconnect(context.Context, int64) (*service.PrismStatus, error)
}

type prismConfigurationService interface {
	GetConfiguration(context.Context) (*service.PrismConfigurationResult, error)
	UpdateConfiguration(context.Context, service.PrismConfigurationOptions) (*service.PrismConfigurationResult, error)
	ResetConfiguration(context.Context) (*service.PrismConfigurationResult, error)
}

type PrismHandler struct {
	prismService         prismAccountService
	configurationService prismConfigurationService
}

func NewPrismHandler(prismService *service.PrismAccountService) *PrismHandler {
	h := &PrismHandler{}
	if prismService != nil {
		h.prismService = prismService
		h.configurationService = prismService
	}
	return h
}

func prismAccountID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return 0, false
	}
	return id, true
}

func (h *PrismHandler) available(c *gin.Context) bool {
	if h == nil || h.prismService == nil {
		response.ErrorFrom(c, infraerrors.New(http.StatusServiceUnavailable, "PRISM_NOT_CONFIGURED", "The Prism browser service is not configured"))
		return false
	}
	return true
}

func (h *PrismHandler) Create(c *gin.Context) {
	sourceID, valid := prismAccountID(c)
	if !valid {
		return
	}
	var req struct {
		Name     string  `json:"name" binding:"omitempty,max=100"`
		GroupIDs []int64 `json:"group_ids" binding:"omitempty,dive,gt=0"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		response.BadRequest(c, "Invalid Prism account request")
		return
	}
	if !h.available(c) {
		return
	}
	account, status, err := h.prismService.Create(c.Request.Context(), sourceID, req.Name, req.GroupIDs)
	if response.ErrorFrom(c, err) {
		return
	}
	response.Accepted(c, gin.H{
		"account": dto.AccountForObserver(c.Request.Context(), dto.AccountFromService(account)),
		"status":  status,
	})
}

func (h *PrismHandler) Status(c *gin.Context) {
	id, valid := prismAccountID(c)
	if !valid || !h.available(c) {
		return
	}
	status, err := h.prismService.Status(c.Request.Context(), id)
	if response.ErrorFrom(c, err) {
		return
	}
	response.Success(c, status)
}

func (h *PrismHandler) Reconnect(c *gin.Context) {
	id, valid := prismAccountID(c)
	if !valid || !h.available(c) {
		return
	}
	status, err := h.prismService.Reconnect(c.Request.Context(), id)
	if response.ErrorFrom(c, err) {
		return
	}
	response.Accepted(c, status)
}

// Global startup settings never share the scoped account permissions of the
// create/status/reconnect endpoints. Keep this guard even behind admin routes.
func prismConfigurationAdmin(c *gin.Context) bool {
	if _, observer := service.ObserverGroupIDs(c.Request.Context()); observer {
		response.ErrorFrom(c, service.ErrObserverScope)
		return false
	}
	return true
}

func (h *PrismHandler) GetConfiguration(c *gin.Context) {
	if !prismConfigurationAdmin(c) {
		return
	}
	if h == nil || h.configurationService == nil {
		response.Success(c, &service.PrismConfigurationResult{Availability: "not_configured"})
		return
	}
	result, err := h.configurationService.GetConfiguration(c.Request.Context())
	if !response.ErrorFrom(c, err) {
		response.Success(c, result)
	}
}

func (h *PrismHandler) configurationAvailable(c *gin.Context) bool {
	if h == nil || h.configurationService == nil {
		response.ErrorFrom(c, infraerrors.New(http.StatusServiceUnavailable, "PRISM_SETTINGS_NOT_CONFIGURED", "The Prism management connection is not configured"))
		return false
	}
	return true
}

func (h *PrismHandler) UpdateConfiguration(c *gin.Context) {
	if !prismConfigurationAdmin(c) {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.PrismConfigurationRequestLimit)
	options, err := service.DecodePrismConfigurationOptions(c.Request.Body)
	if response.ErrorFrom(c, err) || !h.configurationAvailable(c) {
		return
	}
	result, err := h.configurationService.UpdateConfiguration(c.Request.Context(), options)
	if !response.ErrorFrom(c, err) {
		response.Success(c, result)
	}
}

func (h *PrismHandler) ResetConfiguration(c *gin.Context) {
	if !prismConfigurationAdmin(c) || !h.configurationAvailable(c) {
		return
	}
	result, err := h.configurationService.ResetConfiguration(c.Request.Context())
	if !response.ErrorFrom(c, err) {
		response.Success(c, result)
	}
}

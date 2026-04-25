package channel

import (
	"context"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/432539/gpt2api/pkg/resp"
)

type Handler struct {
	svc    *Service
	router *Router
}

func NewHandler(svc *Service, router *Router) *Handler {
	return &Handler{svc: svc, router: router}
}

// GET /api/admin/channels
func (h *Handler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if size < 1 || size > 100 {
		size = 20
	}
	list, total, err := h.svc.List(c.Request.Context(), (page-1)*size, size)
	if err != nil {
		resp.Internal(c, err.Error())
		return
	}
	resp.OK(c, gin.H{"list": list, "total": total, "page": page, "page_size": size})
}

// POST /api/admin/channels
func (h *Handler) Create(c *gin.Context) {
	var in CreateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		resp.BadRequest(c, err.Error())
		return
	}
	ch, err := h.svc.Create(c.Request.Context(), in)
	if err != nil {
		resp.BadRequest(c, err.Error())
		return
	}
	resp.OK(c, ch)
}

// GET /api/admin/channels/:id
func (h *Handler) Get(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	ch, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		resp.NotFound(c, err.Error())
		return
	}
	resp.OK(c, ch)
}

// PATCH /api/admin/channels/:id
func (h *Handler) Update(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var in UpdateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		resp.BadRequest(c, err.Error())
		return
	}
	ch, err := h.svc.Update(c.Request.Context(), id, in)
	if err != nil {
		resp.BadRequest(c, err.Error())
		return
	}
	resp.OK(c, ch)
}

// DELETE /api/admin/channels/:id
func (h *Handler) Delete(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	if err := h.svc.Delete(c.Request.Context(), id); err != nil {
		resp.Internal(c, err.Error())
		return
	}
	resp.OK(c, gin.H{"deleted": id})
}

// POST /api/admin/channels/:id/test 测试连接。
// 调 Adapter.Ping,结果写入 fail_count / status / last_test_*。
func (h *Handler) Test(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	ad, ch, err := h.router.BuildAdapter(c.Request.Context(), id)
	if err != nil {
		resp.BadRequest(c, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	start := time.Now()
	pingErr := ad.Ping(ctx)
	latency := int(time.Since(start).Milliseconds())
	if pingErr != nil {
		_ = h.svc.MarkHealth(context.Background(), ch, false, pingErr.Error())
		resp.OK(c, gin.H{
			"ok": false, "latency_ms": latency, "error": pingErr.Error(),
		})
		return
	}
	_ = h.svc.MarkHealth(context.Background(), ch, true, "")
	resp.OK(c, gin.H{"ok": true, "latency_ms": latency})
}

// ---- Mapping ----

// GET /api/admin/channels/:id/mappings
func (h *Handler) ListMappings(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	list, err := h.svc.ListMappings(c.Request.Context(), id)
	if err != nil {
		resp.Internal(c, err.Error())
		return
	}
	resp.OK(c, gin.H{"list": list})
}

// POST /api/admin/channels/:id/mappings
func (h *Handler) CreateMapping(c *gin.Context) {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	var in CreateMappingInput
	if err := c.ShouldBindJSON(&in); err != nil {
		resp.BadRequest(c, err.Error())
		return
	}
	in.ChannelID = id
	m, err := h.svc.CreateMapping(c.Request.Context(), in)
	if err != nil {
		resp.BadRequest(c, err.Error())
		return
	}
	resp.OK(c, m)
}

// PATCH /api/admin/channels/mappings/:mid
func (h *Handler) UpdateMapping(c *gin.Context) {
	mid, _ := strconv.ParseUint(c.Param("mid"), 10, 64)
	var in CreateMappingInput
	if err := c.ShouldBindJSON(&in); err != nil {
		resp.BadRequest(c, err.Error())
		return
	}
	m, err := h.svc.UpdateMapping(c.Request.Context(), mid, in)
	if err != nil {
		resp.BadRequest(c, err.Error())
		return
	}
	resp.OK(c, m)
}

// DELETE /api/admin/channels/mappings/:mid
func (h *Handler) DeleteMapping(c *gin.Context) {
	mid, _ := strconv.ParseUint(c.Param("mid"), 10, 64)
	if err := h.svc.DeleteMapping(c.Request.Context(), mid); err != nil {
		resp.Internal(c, err.Error())
		return
	}
	resp.OK(c, gin.H{"deleted": mid})
}

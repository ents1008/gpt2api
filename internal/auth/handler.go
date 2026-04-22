package auth

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/432539/gpt2api/internal/user"
	pkgjwt "github.com/432539/gpt2api/pkg/jwt"
	"github.com/432539/gpt2api/pkg/resp"
)

type Handler struct {
	svc *Service
}

func NewHandler(s *Service) *Handler { return &Handler{svc: s} }

type registerReq struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=6,max=64"`
	Nickname string `json:"nickname" binding:"max=64"`
}

type loginReq struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

type refreshReq struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

type loginResp struct {
	User  *user.User        `json:"user"`
	Token *pkgjwt.TokenPair `json:"token"`
}

// POST /api/auth/register
func (h *Handler) Register(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.BadRequest(c, err.Error())
		return
	}
	u, err := h.svc.Register(c.Request.Context(), req.Email, req.Password, req.Nickname, c.ClientIP())
	if err != nil {
		if errors.Is(err, ErrEmailExists) {
			resp.Conflict(c, "email already registered")
			return
		}
		if errors.Is(err, ErrRegisterDisabled) {
			resp.Forbidden(c, "user registration is currently disabled")
			return
		}
		if errors.Is(err, ErrEmailNotAllowed) {
			resp.BadRequest(c, "this email domain is not allowed for registration")
			return
		}
		if errors.Is(err, ErrPasswordTooShort) {
			resp.BadRequest(c, "password is too short")
			return
		}
		if errors.Is(err, ErrRegisterIPCooldown) {
			resp.RateLimited(c, "同一 IP 注册过于频繁,请稍后再试")
			return
		}
		if errors.Is(err, ErrRegisterIPLimit) {
			resp.RateLimited(c, "同一 IP 24 小时内注册次数过多")
			return
		}
		resp.Internal(c, err.Error())
		return
	}
	resp.OK(c, u)
}

// POST /api/auth/login
func (h *Handler) Login(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.BadRequest(c, err.Error())
		return
	}
	u, pair, err := h.svc.Login(c.Request.Context(), req.Email, req.Password, c.ClientIP())
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidCredential):
			resp.Unauthorized(c, "invalid email or password")
		case errors.Is(err, ErrUserBanned):
			resp.Forbidden(c, "user banned")
		default:
			resp.Internal(c, err.Error())
		}
		return
	}
	resp.OK(c, loginResp{User: u, Token: pair})
}

// POST /api/auth/refresh
func (h *Handler) Refresh(c *gin.Context) {
	var req refreshReq
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.BadRequest(c, err.Error())
		return
	}
	pair, err := h.svc.Refresh(c.Request.Context(), req.RefreshToken)
	if err != nil {
		resp.Unauthorized(c, err.Error())
		return
	}
	resp.OK(c, pair)
}

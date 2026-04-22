package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/432539/gpt2api/internal/billing"
	"github.com/432539/gpt2api/internal/settings"
	"github.com/432539/gpt2api/internal/user"
	pkgjwt "github.com/432539/gpt2api/pkg/jwt"
	"github.com/432539/gpt2api/pkg/mailer"
)

// 错误码
var (
	ErrEmailExists        = errors.New("auth: email already exists")
	ErrInvalidCredential  = errors.New("auth: invalid email or password")
	ErrUserBanned         = errors.New("auth: user banned")
	ErrRegisterDisabled   = errors.New("auth: user registration is disabled")
	ErrEmailNotAllowed    = errors.New("auth: email domain is not allowed by whitelist")
	ErrPasswordTooShort   = errors.New("auth: password too short")
	ErrRegisterIPCooldown = errors.New("auth: register ip cooldown")
	ErrRegisterIPLimit    = errors.New("auth: register ip limit")
)

// Service 封装注册、登录、刷新业务。
type Service struct {
	users      *user.DAO
	jwt        *pkgjwt.Manager
	bcryptCost int

	mail    *mailer.Mailer // 可为 nil;为 nil 时不发邮件
	baseURL string

	// 以下两个用于注册开关 / 赠送积分,均为可选依赖。
	// 未注入时:允许注册(兼容旧行为),不发放赠送积分。
	settings *settings.Service
	billing  *billing.Engine
}

func NewService(udao *user.DAO, jm *pkgjwt.Manager, bcryptCost int) *Service {
	if bcryptCost < bcrypt.MinCost || bcryptCost > bcrypt.MaxCost {
		bcryptCost = 10
	}
	return &Service{users: udao, jwt: jm, bcryptCost: bcryptCost}
}

// SetMailer 把邮件发送器注入进来(可选)。传 nil 或 disabled 的 mailer 即不发邮件。
// 单独出接口,避免 NewService 签名膨胀。
func (s *Service) SetMailer(m *mailer.Mailer, baseURL string) {
	s.mail = m
	s.baseURL = baseURL
}

// SetSettings 注入系统设置服务(用于注册开关 / 默认分组)。
func (s *Service) SetSettings(ss *settings.Service) { s.settings = ss }

// SetBilling 注入计费引擎(用于注册赠送积分)。
func (s *Service) SetBilling(b *billing.Engine) { s.billing = b }

// Register 新用户注册。
func (s *Service) Register(ctx context.Context, email, password, nickname, registerIP string) (*user.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	registerIP = strings.TrimSpace(registerIP)
	if email == "" || password == "" {
		return nil, errors.New("email and password required")
	}

	// 动态密码长度阈值(默认 6);>0 才检查,避免 settings 未初始化时把所有注册阻塞
	if s.settings != nil {
		if min := s.settings.PasswordMinLength(); min > 0 && len(password) < min {
			return nil, ErrPasswordTooShort
		}
		// 邮箱域名白名单(空集 = 不限)
		if wl := s.settings.EmailDomainWhitelist(); len(wl) > 0 {
			at := strings.LastIndex(email, "@")
			if at < 0 {
				return nil, ErrEmailNotAllowed
			}
			if _, ok := wl[email[at+1:]]; !ok {
				return nil, ErrEmailNotAllowed
			}
		}
	}

	n, err := s.users.CountByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, ErrEmailExists
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.bcryptCost)
	if err != nil {
		return nil, err
	}
	// Bootstrap 规则:若当前系统没有任何用户,首位注册者自动获得 admin 角色。
	// 典型部署场景下一次性生效,后续注册仍为普通用户。
	role := "user"
	total, _ := s.users.CountAll(ctx)
	if total == 0 {
		role = "admin"
	} else if s.settings != nil && !s.settings.AllowRegister() {
		// 已有用户且管理员关闭了开放注册 —— 拒绝。
		return nil, ErrRegisterDisabled
	}
	if err := s.checkRegisterIPPolicy(ctx, registerIP, time.Now()); err != nil {
		return nil, err
	}

	var groupID uint64 = 1
	if s.settings != nil {
		if g := s.settings.DefaultGroupID(); g > 0 {
			groupID = g
		}
	}

	u := &user.User{
		Email:         email,
		PasswordHash:  string(hash),
		Nickname:      nickname,
		GroupID:       groupID,
		Role:          role,
		Status:        "active",
		CreditBalance: 0,
		RegisterIP:    registerIP,
	}
	id, err := s.users.Create(ctx, u)
	if err != nil {
		return nil, err
	}
	u.ID = id

	// 注册赠送积分(失败不阻断注册流程,仅打日志)
	if s.settings != nil && s.billing != nil {
		if bonus := s.settings.SignupBonusCredits(); bonus > 0 {
			_, _ = s.billing.AdminAdjust(ctx, u.ID, 0, bonus, "signup_bonus", "auto grant on register")
		}
	}

	// 欢迎邮件(可选,失败不影响注册)
	if s.mail != nil && !s.mail.Disabled() {
		subject, html := mailer.RenderWelcome(u.Nickname, u.Email, s.baseURL)
		s.mail.Send(mailer.Message{To: u.Email, Subject: subject, HTML: html})
	}
	return u, nil
}

func (s *Service) checkRegisterIPPolicy(ctx context.Context, ip string, now time.Time) error {
	if s.settings == nil || strings.TrimSpace(ip) == "" {
		return nil
	}

	cooldownSec := s.settings.RegisterIPCooldownSec()
	maxPer24h := s.settings.RegisterIPMaxPer24h()
	if cooldownSec <= 0 && maxPer24h <= 0 {
		return nil
	}

	var lastAt *time.Time
	if cooldownSec > 0 {
		t, ok, err := s.users.LastRegisterAtByIP(ctx, ip)
		if err != nil {
			return err
		}
		if ok {
			lastAt = &t
		}
	}

	recentCount := 0
	if maxPer24h > 0 {
		n, err := s.users.CountByRegisterIPSince(ctx, ip, now.Add(-24*time.Hour))
		if err != nil {
			return err
		}
		recentCount = n
	}

	return evaluateRegisterIPPolicy(now, lastAt, recentCount, cooldownSec, maxPer24h)
}

func evaluateRegisterIPPolicy(now time.Time, lastAt *time.Time, recentCount, cooldownSec, maxPer24h int) error {
	if cooldownSec > 0 && lastAt != nil {
		cooldownUntil := lastAt.Add(time.Duration(cooldownSec) * time.Second)
		if now.Before(cooldownUntil) {
			return ErrRegisterIPCooldown
		}
	}
	if maxPer24h > 0 && recentCount >= maxPer24h {
		return ErrRegisterIPLimit
	}
	return nil
}

// Login 校验邮箱密码并签发 token。
func (s *Service) Login(ctx context.Context, email, password, ip string) (*user.User, *pkgjwt.TokenPair, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	u, err := s.users.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			return nil, nil, ErrInvalidCredential
		}
		return nil, nil, err
	}
	if u.Status == "banned" {
		return nil, nil, ErrUserBanned
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, nil, ErrInvalidCredential
	}
	pair, err := s.jwt.Issue(u.ID, u.Role)
	if err != nil {
		return nil, nil, err
	}
	_ = s.users.UpdateLoginInfo(ctx, u.ID, ip)
	return u, pair, nil
}

// HashPassword 对外暴露 bcrypt 哈希(cost 由 service 持有),admin 重置密码走这里。
func (s *Service) HashPassword(plain string) (string, error) {
	if len(plain) < 6 {
		return "", errors.New("password too short")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), s.bcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// VerifyPassword 校验指定 user 的明文密码是否正确(不签发 token)。
// 主要用于"高危操作二次确认"场景(如恢复数据库、调整积分)。
// 正确返回 nil;错误返回 ErrInvalidCredential / ErrUserBanned 等。
func (s *Service) VerifyPassword(ctx context.Context, userID uint64, password string) error {
	u, err := s.users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			return ErrInvalidCredential
		}
		return err
	}
	if u.Status == "banned" {
		return ErrUserBanned
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return ErrInvalidCredential
	}
	return nil
}

// Refresh 用 refresh_token 换新的 access_token 对。
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*pkgjwt.TokenPair, error) {
	claims, err := s.jwt.VerifyRefresh(refreshToken)
	if err != nil {
		return nil, err
	}
	u, err := s.users.GetByID(ctx, claims.UserID)
	if err != nil {
		return nil, err
	}
	if u.Status == "banned" {
		return nil, ErrUserBanned
	}
	return s.jwt.Issue(u.ID, u.Role)
}

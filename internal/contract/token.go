package contract

import "time"

// APIToken 是发给用户的机器凭据，用于在脚本里调用面板接口（例如探针页面的
// 设备地址接口）。明文只在创建或重置的那一次响应里出现，之后任何接口都
// 只回这个结构 —— 数据库里存的是 SHA-256 摘要，取不回来。
//
// Prefix 是明文的前 8 位，用来让用户分辨「我手上这把是哪一条」，不足以
// 反推出凭据本身。
type APIToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id,omitempty"`
	Username   string     `json:"username,omitempty"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scope      string     `json:"scope"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	Permanent  bool       `json:"permanent"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// TokenScopeOwnerResources 是当前唯一的作用域：只能操作凭据持有者自己的资源。
// 机器凭据一律按普通用户处理，即使持有者是管理员 —— 一把放在脚本里的密钥
// 不该顺带拥有管理后台的全部权限。
const TokenScopeOwnerResources = "owner-resources"

// MaxTokenLifetime 是有限期凭据的最长有效期。
const MaxTokenLifetime = 365 * 24 * time.Hour

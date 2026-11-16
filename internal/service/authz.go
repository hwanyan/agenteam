package service

import (
	"context"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hwanyan/agenteam/internal/authclient"
	"github.com/hwanyan/agenteam/internal/authctx"
	"github.com/hwanyan/agenteam/internal/store"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

// 授权模型（两套角色互不相干）：
//
//   - 平台级角色：auth 服务写入 Access Token 的 roles（user / admin），回答“这个人是不是整个平台的管理员”。
//     这里只用到 admin：平台管理员可以越过团队成员校验，对任意团队按 owner 对待。
//   - 团队级角色：agenteam 自己 team_members 表里的 owner / member，回答“这个人在这个团队里能做什么”：
//     member 只读并可在工作区对话；owner 还可以增删改 Agent / MCP 工具、管理成员、删除团队。
//
// 非成员访问团队资源一律返回 NotFound（与资源真的不存在无法区分），不暴露团队/Agent/工具是否存在。

// platformAdminRole 是 auth 服务 users.role 里平台管理员的取值。
const platformAdminRole = "admin"

// 用户名镜像（user_profiles）的刷新节流：同一个用户、用户名没变时，至少间隔这么久才再写一次库。
const profileSyncInterval = 10 * time.Minute

// isPlatformAdmin 判断是否为平台管理员（auth 的 admin），与团队内角色无关。
func isPlatformAdmin(id *authclient.Identity) bool { return id.HasRole(platformAdminRole) }

// roleSatisfies 判断团队角色 have 是否满足最低要求 need：owner 满足一切，member 只满足 member。
func roleSatisfies(have, need agenteamv1.TeamRole) bool {
	if have == agenteamv1.TeamRole_TEAM_ROLE_OWNER {
		return true
	}
	return have == agenteamv1.TeamRole_TEAM_ROLE_MEMBER && need == agenteamv1.TeamRole_TEAM_ROLE_MEMBER
}

// profileSync 记录最近一次把哪些用户的 (user_id, username) 刷新进了 user_profiles，用于节流。零值可用。
type profileSync struct {
	mu   sync.Mutex
	seen map[string]profileSeen
}

type profileSeen struct {
	username string
	at       time.Time
}

func (p *profileSync) due(userID, username string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.seen[userID]
	return !ok || e.username != username || now.Sub(e.at) >= profileSyncInterval
}

func (p *profileSync) done(userID, username string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen == nil {
		p.seen = make(map[string]profileSeen)
	}
	p.seen[userID] = profileSeen{username: username, at: now}
}

// currentUser 返回当前请求的用户身份；没有身份（请求没有经过鉴权）返回 Unauthenticated（fail-closed）。
//
// 同时把 (user_id, username) 限频地刷新进 user_profiles：auth 服务没有“按用户名查询用户”的接口，
// 邀请成员只能靠这份镜像把用户名解析成 user_id。刷新失败只记日志，不影响本次请求。
func (d *Deps) currentUser(ctx context.Context) (*authclient.Identity, error) {
	id, ok := authctx.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "未登录或登录已失效")
	}
	if id.UserID != "" && id.Username != "" {
		now := time.Now()
		if d.profiles.due(id.UserID, id.Username, now) {
			if err := d.Store.UpsertUserProfile(ctx, id.UserID, id.Username); err != nil {
				log.Printf("service: 刷新用户信息失败 user=%s: %v", id.UserID, err)
			} else {
				d.profiles.done(id.UserID, id.Username, now)
			}
		}
	}
	return id, nil
}

// authorizeTeam 校验当前用户能否以至少 need 的团队角色访问团队 teamID，返回其身份与在该团队内的有效角色。
//   - 平台管理员：越过成员校验，按 owner 对待（团队不存在仍返回 NotFound）；
//   - 其他用户：必须是团队成员；非成员 / 团队不存在 / 团队已删除统一返回 NotFound；
//     是成员但角色不够返回 PermissionDenied。
func (d *Deps) authorizeTeam(ctx context.Context, teamID string, need agenteamv1.TeamRole) (*authclient.Identity, agenteamv1.TeamRole, error) {
	id, err := d.currentUser(ctx)
	if err != nil {
		return nil, agenteamv1.TeamRole_TEAM_ROLE_UNSPECIFIED, err
	}
	if teamID == "" {
		return nil, agenteamv1.TeamRole_TEAM_ROLE_UNSPECIFIED, teamNotFoundOrErr(store.ErrNotFound)
	}

	var role agenteamv1.TeamRole
	if isPlatformAdmin(id) {
		if _, err := d.Store.GetTeam(ctx, teamID); err != nil {
			return nil, agenteamv1.TeamRole_TEAM_ROLE_UNSPECIFIED, teamNotFoundOrErr(err)
		}
		role = agenteamv1.TeamRole_TEAM_ROLE_OWNER
	} else {
		role, err = d.Store.GetMemberRole(ctx, teamID, id.UserID)
		if err != nil {
			return nil, agenteamv1.TeamRole_TEAM_ROLE_UNSPECIFIED, teamNotFoundOrErr(err)
		}
	}

	if !roleSatisfies(role, need) {
		return nil, role, status.Error(codes.PermissionDenied, "仅团队 owner 可执行此操作")
	}
	return id, role, nil
}

// authorizeAgent 按 Agent id 取出 Agent，并以其所属团队校验当前用户的权限。
// 非成员得到与“agent 不存在”完全相同的 NotFound，不暴露 Agent 是否存在。
func (d *Deps) authorizeAgent(ctx context.Context, agentID string, need agenteamv1.TeamRole) (*agenteamv1.Agent, error) {
	if _, err := d.currentUser(ctx); err != nil {
		return nil, err
	}
	agent, err := d.Store.GetAgent(ctx, agentID)
	if err != nil {
		return nil, agentNotFoundOrErr(err)
	}
	if _, _, err := d.authorizeTeam(ctx, agent.TeamId, need); err != nil {
		return nil, maskNotFound(err, agentNotFoundOrErr(store.ErrNotFound))
	}
	return agent, nil
}

// authorizeMcpTool 按 MCP 工具 id 取出工具，并以其所属团队校验当前用户的权限；语义同 authorizeAgent。
func (d *Deps) authorizeMcpTool(ctx context.Context, toolID string, need agenteamv1.TeamRole) (*agenteamv1.McpTool, error) {
	if _, err := d.currentUser(ctx); err != nil {
		return nil, err
	}
	tool, _, err := d.Store.GetMcpTool(ctx, toolID)
	if err != nil {
		return nil, mcpToolNotFoundOrErr(err)
	}
	if _, _, err := d.authorizeTeam(ctx, tool.TeamId, need); err != nil {
		return nil, maskNotFound(err, mcpToolNotFoundOrErr(store.ErrNotFound))
	}
	return tool, nil
}

// maskNotFound 把授权失败里的“团队不存在”替换为具体资源的“不存在”错误，其余错误原样返回。
func maskNotFound(err, notFound error) error {
	if status.Code(err) == codes.NotFound {
		return notFound
	}
	return err
}

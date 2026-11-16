package service

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hwanyan/agenteam/internal/store"
	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

// maxUsernameLen 限制邀请时用户名的长度，避免异常大的入参。
const maxUsernameLen = 128

// 团队成员管理。这里的角色是团队级角色（owner / member），与 auth 服务 Token 里的平台级角色无关。

// ListTeamMembers 返回团队成员（owner 在前，其余按加入时间升序）。团队成员可见。
func (s *TeamServer) ListTeamMembers(ctx context.Context, req *agenteamv1.ListTeamMembersRequest) (*agenteamv1.ListTeamMembersResponse, error) {
	if _, _, err := s.authorizeTeam(ctx, req.TeamId, agenteamv1.TeamRole_TEAM_ROLE_MEMBER); err != nil {
		return nil, err
	}
	members, err := s.Store.ListTeamMembers(ctx, req.TeamId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "查询团队成员失败: %v", err)
	}
	return &agenteamv1.ListTeamMembersResponse{Members: members}, nil
}

// AddTeamMember 邀请用户加入团队（直接加入，无需对方确认）。仅 owner 可用。
// 被邀请人按用户名指定，必须已登录过本平台（auth 服务没有按用户名查询用户的接口，
// 用户名到 user_id 的对应来自登录时记录的 user_profiles 镜像）。role 留空按 member 处理。
func (s *TeamServer) AddTeamMember(ctx context.Context, req *agenteamv1.AddTeamMemberRequest) (*agenteamv1.AddTeamMemberResponse, error) {
	if _, _, err := s.authorizeTeam(ctx, req.TeamId, agenteamv1.TeamRole_TEAM_ROLE_OWNER); err != nil {
		return nil, err
	}

	username := strings.TrimSpace(req.Username)
	if username == "" {
		return nil, status.Error(codes.InvalidArgument, "用户名不能为空")
	}
	if len(username) > maxUsernameLen {
		return nil, status.Errorf(codes.InvalidArgument, "用户名过长，最大支持 %d 字符", maxUsernameLen)
	}
	role := req.Role
	if role == agenteamv1.TeamRole_TEAM_ROLE_UNSPECIFIED {
		role = agenteamv1.TeamRole_TEAM_ROLE_MEMBER
	}
	if !validTeamRole(role) {
		return nil, status.Error(codes.InvalidArgument, "不支持的团队角色")
	}

	userID, err := s.Store.FindUserIDByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "用户不存在，或该用户尚未登录过本平台")
		}
		return nil, status.Errorf(codes.Internal, "查询用户失败: %v", err)
	}

	if err := s.Store.AddTeamMember(ctx, req.TeamId, userID, role); err != nil {
		switch {
		case errors.Is(err, store.ErrAlreadyExists):
			return nil, status.Error(codes.AlreadyExists, "该用户已是团队成员")
		case errors.Is(err, store.ErrNotFound):
			return nil, teamNotFoundOrErr(err)
		}
		return nil, status.Errorf(codes.Internal, "添加团队成员失败: %v", err)
	}

	return &agenteamv1.AddTeamMemberResponse{Member: s.memberOf(ctx, req.TeamId, userID, role)}, nil
}

// UpdateTeamMemberRole 修改成员角色。仅 owner 可用；不允许把团队最后一个 owner 降级。
func (s *TeamServer) UpdateTeamMemberRole(ctx context.Context, req *agenteamv1.UpdateTeamMemberRoleRequest) (*agenteamv1.UpdateTeamMemberRoleResponse, error) {
	if _, _, err := s.authorizeTeam(ctx, req.TeamId, agenteamv1.TeamRole_TEAM_ROLE_OWNER); err != nil {
		return nil, err
	}
	if req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id 不能为空")
	}
	if !validTeamRole(req.Role) {
		return nil, status.Error(codes.InvalidArgument, "不支持的团队角色")
	}

	if err := s.Store.UpdateTeamMemberRole(ctx, req.TeamId, req.UserId, req.Role); err != nil {
		return nil, memberChangeErr(err, "修改成员角色失败")
	}
	return &agenteamv1.UpdateTeamMemberRoleResponse{Member: s.memberOf(ctx, req.TeamId, req.UserId, req.Role)}, nil
}

// RemoveTeamMember 移除成员。owner 可移除任何人；任何成员都可以移除自己（退出团队）。
// 不允许移除团队最后一个 owner。
func (s *TeamServer) RemoveTeamMember(ctx context.Context, req *agenteamv1.RemoveTeamMemberRequest) (*agenteamv1.RemoveTeamMemberResponse, error) {
	user, err := s.currentUser(ctx)
	if err != nil {
		return nil, err
	}
	if req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id 不能为空")
	}

	need := agenteamv1.TeamRole_TEAM_ROLE_OWNER
	if req.UserId == user.UserID {
		need = agenteamv1.TeamRole_TEAM_ROLE_MEMBER // 退出团队
	}
	if _, _, err := s.authorizeTeam(ctx, req.TeamId, need); err != nil {
		return nil, err
	}

	if err := s.Store.RemoveTeamMember(ctx, req.TeamId, req.UserId); err != nil {
		return nil, memberChangeErr(err, "移除团队成员失败")
	}
	return &agenteamv1.RemoveTeamMemberResponse{}, nil
}

func validTeamRole(r agenteamv1.TeamRole) bool {
	return r == agenteamv1.TeamRole_TEAM_ROLE_OWNER || r == agenteamv1.TeamRole_TEAM_ROLE_MEMBER
}

// memberChangeErr 把成员变更的存储错误转成面向用户的 gRPC 错误。
func memberChangeErr(err error, internalMsg string) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, "团队成员不存在")
	case errors.Is(err, store.ErrLastOwner):
		return status.Error(codes.FailedPrecondition, "团队至少需要保留一名 owner，请先指定其他 owner")
	}
	return status.Errorf(codes.Internal, "%s: %v", internalMsg, err)
}

// memberOf 取出成员的完整展示信息（含用户名）；查询失败时降级为只带 user_id 与角色的最小信息，
// 因为写操作本身已经成功，不应让一次展示信息的查询失败把整个请求变成错误。
func (s *TeamServer) memberOf(ctx context.Context, teamID, userID string, role agenteamv1.TeamRole) *agenteamv1.TeamMember {
	if members, err := s.Store.ListTeamMembers(ctx, teamID); err == nil {
		for _, m := range members {
			if m.UserId == userID {
				return m
			}
		}
	}
	return &agenteamv1.TeamMember{UserId: userID, Role: role}
}

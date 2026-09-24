package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var identityGroupIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type identityGroupInput struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (in *identityGroupInput) validate() error {
	in.ID = strings.TrimSpace(in.ID)
	in.Name = strings.TrimSpace(in.Name)
	if !identityGroupIDPattern.MatchString(in.ID) {
		return errors.New("身份用户组 ID 必须为 1–64 位字母、数字、下划线或短横线")
	}
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 190 {
		return errors.New("身份用户组名称不能为空且不能超过 190 个字符")
	}
	return nil
}

// Lock the identity while changing it or adding a reference to it. A concurrent
// ID edit must not leave users or device groups pointing at the old ID.
func (s *Server) identityGroupForUpdate(ctx context.Context, tx *sql.Tx, groupID string) (contract.IdentityGroup, error) {
	query := `SELECT id,name FROM cp_identity_groups WHERE id=?`
	if s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var group contract.IdentityGroup
	err := tx.QueryRowContext(ctx, s.q(query), groupID).Scan(&group.ID, &group.Name)
	return group, err
}

func (s *Server) groupAuthorized(ctx context.Context, q rowQuerier, groupID, userID string) (bool, error) {
	var count int
	err := q.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM cp_group_identity_groups gig JOIN cp_users u ON u.identity_group_id=gig.identity_group_id WHERE gig.group_id=? AND u.id=?`), groupID, userID).Scan(&count)
	return count > 0, err
}

func (s *Server) identityGroups(w http.ResponseWriter, r *http.Request) {
	n, o := pages(r)
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	where := ""
	args := []any{}
	if search != "" {
		where = " WHERE name LIKE ? OR id LIKE ?"
		pattern := "%" + search + "%"
		args = append(args, pattern, pattern)
	}
	var total int
	if err := s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_identity_groups"+where), args...).Scan(&total); err != nil {
		fail(w, 500, "identity group query failed")
		return
	}
	args = append(args, n, o)
	rows, err := s.Store.DB.QueryContext(r.Context(), s.q(`SELECT ig.id,ig.name,(SELECT COUNT(*) FROM cp_users u WHERE u.identity_group_id=ig.id),(SELECT COUNT(*) FROM cp_group_identity_groups gig WHERE gig.identity_group_id=ig.id) FROM cp_identity_groups ig`+where+` ORDER BY ig.name,ig.id LIMIT ? OFFSET ?`), args...)
	if err != nil {
		fail(w, 500, "identity group query failed")
		return
	}
	defer rows.Close()
	items := []contract.IdentityGroup{}
	for rows.Next() {
		var group contract.IdentityGroup
		if err = rows.Scan(&group.ID, &group.Name, &group.UserCount, &group.DeviceGroupCount); err != nil {
			fail(w, 500, "identity group query failed")
			return
		}
		items = append(items, group)
	}
	if err = rows.Err(); err != nil {
		fail(w, 500, "identity group query failed")
		return
	}
	reply(w, 200, map[string]any{"items": items, "total": total})
}

func (s *Server) createIdentityGroup(w http.ResponseWriter, r *http.Request) {
	var in identityGroupInput
	if !decode(w, r, &in) {
		return
	}
	if err := in.validate(); err != nil {
		fail(w, 400, err.Error())
		return
	}
	group := contract.IdentityGroup{ID: in.ID, Name: in.Name}
	actor, _ := UserFromContext(r.Context())
	err := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_identity_groups(id,name) VALUES(?,?)`), group.ID, group.Name); err != nil {
			return err
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "identity-group.create", group.ID)
	})
	if err != nil {
		fail(w, 409, "身份用户组创建失败，ID 或名称可能已存在")
		return
	}
	reply(w, 201, group)
}

func (s *Server) updateIdentityGroup(w http.ResponseWriter, r *http.Request) {
	var in identityGroupInput
	if !decode(w, r, &in) {
		return
	}
	if err := in.validate(); err != nil {
		fail(w, 400, err.Error())
		return
	}
	actor, _ := UserFromContext(r.Context())
	group := contract.IdentityGroup{ID: in.ID, Name: in.Name}
	err := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		ctx := r.Context()
		previous, err := s.identityGroupForUpdate(ctx, tx, r.PathValue("id"))
		if err != nil {
			return err
		}
		groups := []contract.Group{}
		if previous.ID != group.ID {
			query := `SELECT g.payload,g.version FROM cp_groups g JOIN cp_group_identity_groups gig ON gig.group_id=g.id WHERE gig.identity_group_id=? ORDER BY g.id`
			if s.Store.Dialect != "sqlite" {
				query += " FOR UPDATE"
			}
			rows, err := tx.QueryContext(ctx, s.q(query), previous.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var raw string
				var version int64
				var deviceGroup contract.Group
				if err = rows.Scan(&raw, &version); err == nil {
					err = json.Unmarshal([]byte(raw), &deviceGroup)
				}
				if err != nil {
					rows.Close()
					return err
				}
				deviceGroup.Version = version
				groups = append(groups, deviceGroup)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			// Recreate only this identity's links inside the transaction, so the
			// primary key can change without disabling foreign-key constraints.
			if _, err = tx.ExecContext(ctx, s.q(`DELETE FROM cp_group_identity_groups WHERE identity_group_id=?`), previous.ID); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, s.q(`UPDATE cp_identity_groups SET id=?,name=? WHERE id=?`), group.ID, group.Name, previous.ID); err != nil {
			return err
		}
		if previous.ID != group.ID {
			if _, err = tx.ExecContext(ctx, s.q(`UPDATE cp_users SET identity_group_id=? WHERE identity_group_id=?`), group.ID, previous.ID); err != nil {
				return err
			}
			for _, deviceGroup := range groups {
				if _, err = tx.ExecContext(ctx, s.q(`INSERT INTO cp_group_identity_groups(group_id,identity_group_id) VALUES(?,?)`), deviceGroup.ID, group.ID); err != nil {
					return err
				}
				rows, err := tx.QueryContext(ctx, s.q(`SELECT identity_group_id FROM cp_group_identity_groups WHERE group_id=? ORDER BY identity_group_id`), deviceGroup.ID)
				if err != nil {
					return err
				}
				deviceGroup.IdentityGroupIDs = []string{}
				for rows.Next() {
					var identityID string
					if err = rows.Scan(&identityID); err != nil {
						rows.Close()
						return err
					}
					deviceGroup.IdentityGroupIDs = append(deviceGroup.IdentityGroupIDs, identityID)
				}
				err = rows.Err()
				rows.Close()
				if err != nil {
					return err
				}
				deviceGroup.UserIDs = nil
				deviceGroup.Version++
				// Bump the version so an open device-group editor cannot silently
				// save the obsolete authorization IDs after the rename.
				result, err := tx.ExecContext(ctx, s.q(`UPDATE cp_groups SET payload=?,version=? WHERE id=? AND version=?`), strJSON(deviceGroup), deviceGroup.Version, deviceGroup.ID, deviceGroup.Version-1)
				if err != nil {
					return err
				}
				if affected, err := result.RowsAffected(); err != nil || affected != 1 {
					return errConflict
				}
			}
		}
		if err = tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM cp_users WHERE identity_group_id=?`), group.ID).Scan(&group.UserCount); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM cp_group_identity_groups WHERE identity_group_id=?`), group.ID).Scan(&group.DeviceGroupCount); err != nil {
			return err
		}
		return s.AuditTx(ctx, tx, actor.ID, "identity-group.update", strJSON(map[string]string{"previous_id": previous.ID, "id": group.ID}))
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, 404, "身份用户组不存在，请刷新列表")
		} else {
			fail(w, 409, "身份用户组更新失败，ID 或名称可能已存在，或关联数据已变更，请刷新后重试")
		}
		return
	}
	reply(w, 200, group)
}

func (s *Server) deleteIdentityGroup(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("id")
	actor, _ := UserFromContext(r.Context())
	err := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		if _, err := s.identityGroupForUpdate(r.Context(), tx, target); err != nil {
			return err
		}
		var users, deviceGroups int
		if err := tx.QueryRowContext(r.Context(), s.q(`SELECT COUNT(*) FROM cp_users WHERE identity_group_id=?`), target).Scan(&users); err != nil {
			return err
		}
		if err := tx.QueryRowContext(r.Context(), s.q(`SELECT COUNT(*) FROM cp_group_identity_groups WHERE identity_group_id=?`), target).Scan(&deviceGroups); err != nil {
			return err
		}
		if users > 0 || deviceGroups > 0 {
			return errConflict
		}
		result, err := tx.ExecContext(r.Context(), s.q(`DELETE FROM cp_identity_groups WHERE id=?`), target)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return sql.ErrNoRows
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "identity-group.delete", target)
	})
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			fail(w, 404, "身份用户组不存在")
		case errors.Is(err, errConflict):
			fail(w, 409, "身份用户组仍被用户或设备组引用，请先解除关联")
		default:
			fail(w, 500, "身份用户组删除失败")
		}
		return
	}
	w.WriteHeader(204)
}

func (s *Server) setUserIdentityGroup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IdentityGroupID string `json:"identity_group_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.IdentityGroupID = strings.TrimSpace(in.IdentityGroupID)
	if in.IdentityGroupID == "" {
		fail(w, 400, "请选择身份用户组")
		return
	}
	target := r.PathValue("id")
	actor, _ := UserFromContext(r.Context())
	err := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		identity, err := s.identityGroupForUpdate(r.Context(), tx, in.IdentityGroupID)
		if errors.Is(err, sql.ErrNoRows) {
			return errConflict
		}
		if err != nil {
			return err
		}
		in.IdentityGroupID = identity.ID
		var previous string
		if err := tx.QueryRowContext(r.Context(), s.q(`SELECT identity_group_id FROM cp_users WHERE id=?`), target).Scan(&previous); err != nil {
			return err
		}
		if previous == in.IdentityGroupID {
			return nil
		}
		if _, err := tx.ExecContext(r.Context(), s.q(`UPDATE cp_users SET identity_group_id=? WHERE id=?`), in.IdentityGroupID, target); err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(), s.q(`UPDATE cp_nodes SET desired_version=desired_version+1 WHERE id IN(SELECT node_id FROM cp_rules WHERE user_id=? AND deleted=0)`), target); err != nil {
			return err
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "user.identity-group", target)
	})
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			fail(w, 404, "用户不存在")
		case errors.Is(err, errConflict):
			fail(w, 409, "身份用户组不存在")
		default:
			fail(w, 500, "身份用户组更新失败")
		}
		return
	}
	w.WriteHeader(204)
}
